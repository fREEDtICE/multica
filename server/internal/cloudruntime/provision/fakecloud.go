package provision

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
)

// fakeInstance is the simulated cloud machine shared by the two fake vendors.
type fakeInstance struct {
	id            string
	provider      string
	state         string
	userData      string
	clientToken   string
	describeCount int
	console       string
}

// fakeCloud implements the real EC2/ECS wire contracts over httptest, including
// signature verification, ClientToken idempotency, pending->running progression
// and operator-controlled faults. The production adapters run against it
// unchanged, so the integration test exercises the same code path as live.
type fakeCloud struct {
	mu           sync.Mutex
	provider     string
	instances    map[string]*fakeInstance
	tokenToID    map[string]string
	counter      int
	authFailure  bool
	roleReject   bool
	stall        bool
	launches     int
	lastLaunchAK string
	lastDiskGB   string
}

func newFakeCloud(provider string) *fakeCloud {
	return &fakeCloud{provider: provider, instances: map[string]*fakeInstance{}, tokenToID: map[string]string{}}
}

func (f *fakeCloud) activeIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id, inst := range f.instances {
		if inst.state != StatusTerminated {
			ids = append(ids, id)
		}
	}
	return ids
}

func (f *fakeCloud) nextID() string {
	f.counter++
	if f.provider == ProviderAWS {
		return fmt.Sprintf("i-%017x", f.counter)
	}
	return fmt.Sprintf("i-bp%014x", f.counter)
}

func (f *fakeCloud) launch(userData, token string) *fakeInstance {
	if id, ok := f.tokenToID[token]; ok {
		return f.instances[id]
	}
	inst := &fakeInstance{
		id: f.nextID(), provider: f.provider, state: StatusPending,
		userData: userData, clientToken: token,
		console: "cloud-init: executing user-data\nhelios-agent: registered through " + RegisterPath,
	}
	f.instances[inst.id] = inst
	f.tokenToID[token] = inst.id
	f.launches++
	return inst
}

func (f *fakeCloud) advance(id string) *fakeInstance {
	inst := f.instances[id]
	if inst.state == StatusPending {
		inst.describeCount++
		if !f.stall && inst.describeCount >= 2 {
			inst.state = StatusRunning
		}
	}
	return inst
}

// start wires the fake vendor server and returns its endpoint URL.
func (f *fakeCloud) start() *httptest.Server {
	if f.provider == ProviderAWS {
		return httptest.NewServer(http.HandlerFunc(f.handleAWS))
	}
	return httptest.NewServer(http.HandlerFunc(f.handleAliyun))
}

func (f *fakeCloud) reject(w http.ResponseWriter, status int, code, message string, xml bool) {
	if xml {
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(status)
		fmt.Fprintf(w, `<Response><Errors><Error><Code>%s</Code><Message>%s</Message></Error></Errors></Response>`, code, message)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"Code": code, "Message": message})
}

// --------------------------------------------------------------------------- //
// AWS EC2/STS query API
// --------------------------------------------------------------------------- //

func (f *fakeCloud) handleAWS(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		f.reject(w, http.StatusBadRequest, "MalformedQuery", "bad body", true)
		return
	}
	form, err := url.ParseQuery(string(raw))
	if err != nil {
		f.reject(w, http.StatusBadRequest, "MalformedQuery", "bad form", true)
		return
	}
	accessKey, secretKey, sessionToken, ok := f.awsExpectedCredentials(r, form)
	if !ok {
		f.reject(w, http.StatusForbidden, "SignatureDoesNotMatch", "fake signature check failed", true)
		return
	}
	signed, err := AWSSignRequest(r.Method, "http://"+r.Host+r.URL.Path, url.Values{},
		map[string]string{"Content-Type": r.Header.Get("Content-Type")}, raw,
		accessKey, secretKey, sessionToken, "us-east-1",
		serviceForAction(form.Get("Action")), r.Header.Get("X-Amz-Date"))
	if err != nil || signed["Authorization"] != r.Header.Get("Authorization") {
		f.reject(w, http.StatusForbidden, "SignatureDoesNotMatch", "fake signature check failed", true)
		return
	}
	action := form.Get("Action")
	if f.roleReject && action == "AssumeRole" {
		f.reject(w, http.StatusForbidden, "AccessDenied", "role trust policy does not allow this identity", true)
		return
	}
	authGuarded := map[string]bool{"GetCallerIdentity": true, "DescribeSubnets": true,
		"DescribeSecurityGroups": true, "RunInstances": true}
	if f.authFailure && authGuarded[action] {
		f.reject(w, http.StatusForbidden, "AuthFailure", "test account not authorized for "+action, true)
		return
	}
	w.Header().Set("Content-Type", "text/xml")
	switch action {
	case "GetCallerIdentity":
		account, arn, userID := "123456789012", "arn:aws:iam::123456789012:user/helios", "AIDAFAKE"
		if sessionToken != "" {
			arn = "arn:aws:sts::123456789012:assumed-role/MulticaCloudRuntime/helios-session"
			userID = "AROAFAKE:helios-session"
		}
		fmt.Fprintf(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>%s</Account><Arn>%s</Arn><UserId>%s</UserId></GetCallerIdentityResult></GetCallerIdentityResponse>`,
			account, arn, userID)
	case "AssumeRole":
		fmt.Fprint(w, `<AssumeRoleResponse><AssumeRoleResult><Credentials>`+
			`<AccessKeyId>`+fakeAWSSessionKey+`</AccessKeyId>`+
			`<SecretAccessKey>`+fakeAWSSessionSecret+`</SecretAccessKey>`+
			`<SessionToken>`+fakeSessionToken+`</SessionToken>`+
			`<Expiration>2099-01-01T00:00:00Z</Expiration>`+
			`</Credentials></AssumeRoleResult></AssumeRoleResponse>`)
	case "DescribeSubnets", "DescribeSecurityGroups":
		fmt.Fprint(w, `<DescribeResponse><result><ok>true</ok></result></DescribeResponse>`)
	case "RunInstances":
		userData, _ := base64.StdEncoding.DecodeString(form.Get("UserData"))
		inst := f.launch(string(userData), form.Get("ClientToken"))
		f.lastLaunchAK = accessKey
		f.lastDiskGB = form.Get("BlockDeviceMapping.1.Ebs.VolumeSize")
		fmt.Fprintf(w, `<RunInstancesResponse><instancesSet><item><instanceId>%s</instanceId><imageId>%s</imageId><clientToken>%s</clientToken><instanceState><name>%s</name></instanceState><iamInstanceProfile><name>%s</name></iamInstanceProfile></item></instancesSet></RunInstancesResponse>`,
			inst.id, form.Get("ImageId"), inst.clientToken, inst.state, form.Get("IamInstanceProfile.Name"))
	case "DescribeInstances":
		inst := f.advance(form.Get("InstanceId.1"))
		fmt.Fprintf(w, `<DescribeInstancesResponse><reservationSet><item><instancesSet><item><instanceId>%s</instanceId><privateIpAddress>10.0.0.21</privateIpAddress><clientToken>%s</clientToken><instanceState><name>%s</name></instanceState></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`,
			inst.id, inst.clientToken, inst.state)
	case "DescribeInstanceAttribute":
		inst := f.instances[form.Get("InstanceId")]
		fmt.Fprintf(w, `<DescribeInstanceAttributeResponse><instanceAttribute><userData><value>%s</value></userData></instanceAttribute></DescribeInstanceAttributeResponse>`,
			base64.StdEncoding.EncodeToString([]byte(inst.userData)))
	case "GetConsoleOutput":
		inst := f.instances[form.Get("InstanceId")]
		fmt.Fprintf(w, `<GetConsoleOutputResponse><instanceId>%s</instanceId><output>%s</output></GetConsoleOutputResponse>`,
			inst.id, base64.StdEncoding.EncodeToString([]byte(inst.console)))
	case "TerminateInstances":
		inst := f.instances[form.Get("InstanceId.1")]
		if inst != nil {
			inst.state = StatusTerminated
		}
		fmt.Fprintf(w, `<TerminateInstancesResponse><instancesSet><item><instanceId>%s</instanceId><currentState><name>terminated</name></currentState></item></instancesSet></TerminateInstancesResponse>`,
			form.Get("InstanceId.1"))
	default:
		f.reject(w, http.StatusBadRequest, "InvalidAction", action, true)
	}
}

func serviceForAction(action string) string {
	if action == "GetCallerIdentity" || action == "AssumeRole" {
		return "sts"
	}
	return "ec2"
}

// awsExpectedCredentials selects the signing key pair the request must have
// used: deployment identity keys without a session token, or the short-lived
// assumed-role session keys together with their token.
func (f *fakeCloud) awsExpectedCredentials(r *http.Request, form url.Values) (ak, sk, token string, ok bool) {
	token = r.Header.Get("X-Amz-Security-Token")
	auth := r.Header.Get("Authorization")
	ak = credentialFromAuthorization(auth)
	if token != "" {
		if token != fakeSessionToken || ak != fakeAWSSessionKey {
			return "", "", "", false
		}
		return fakeAWSSessionKey, fakeAWSSessionSecret, token, true
	}
	if ak != fakeAWSAccessKey {
		return "", "", "", false
	}
	return fakeAWSAccessKey, fakeAWSSecretKey, "", true
}

func credentialFromAuthorization(auth string) string {
	const marker = "Credential="
	start := strings.Index(auth, marker)
	if start < 0 {
		return ""
	}
	rest := auth[start+len(marker):]
	end := strings.Index(rest, "/")
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// --------------------------------------------------------------------------- //
// Alibaba ECS RPC API
// --------------------------------------------------------------------------- //

func (f *fakeCloud) handleAliyun(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	signature := query.Get("Signature")
	query.Del("Signature")
	secretKey, ok := f.aliyunExpectedCredentials(query)
	if !ok || AliyunSignature(query, secretKey, http.MethodGet) != signature {
		f.reject(w, http.StatusForbidden, "SignatureDoesNotMatch", "fake signature check failed", false)
		return
	}
	action := query.Get("Action")
	if f.roleReject && action == "AssumeRole" {
		f.reject(w, http.StatusForbidden, "NoPermission", "RAM role trust policy does not allow this identity", false)
		return
	}
	authGuarded := map[string]bool{"DescribeRegions": true, "DescribeVSwitches": true,
		"DescribeSecurityGroups": true, "RunInstances": true, "GetCallerIdentity": true}
	if f.authFailure && authGuarded[action] {
		f.reject(w, http.StatusForbidden, "Forbidden.RAM", "test RAM user lacks "+action, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	switch action {
	case "GetCallerIdentity":
		enc.Encode(map[string]any{
			"AccountId":   "123456789012",
			"Arn":         "acs:ram::123456789012:role/MulticaCloudRuntime",
			"PrincipalId": "271234567890123456",
			"RequestId":   "fake",
		})
	case "AssumeRole":
		enc.Encode(map[string]any{
			"Credentials": map[string]any{
				"AccessKeyId":     fakeAliyunSessionAK,
				"AccessKeySecret": fakeAliyunSessionSK,
				"SecurityToken":   fakeSessionToken,
				"Expiration":      "2099-01-01T00:00:00Z",
			},
			"RequestId": "fake",
		})
	case "DescribeRegions":
		enc.Encode(map[string]any{"Regions": map[string]any{"Region": []map[string]string{{"RegionId": "cn-hangzhou"}}}})
	case "DescribeVSwitches", "DescribeSecurityGroups":
		enc.Encode(map[string]any{"ok": true})
	case "RunInstances":
		userData, _ := base64.StdEncoding.DecodeString(query.Get("UserData"))
		inst := f.launch(string(userData), query.Get("ClientToken"))
		f.lastLaunchAK = query.Get("AccessKeyId")
		f.lastDiskGB = query.Get("SystemDisk.Size")
		enc.Encode(map[string]any{"InstanceIdSets": map[string]any{"InstanceIdSet": []string{inst.id}}})
	case "DescribeInstances":
		var ids []string
		json.Unmarshal([]byte(query.Get("InstanceIds")), &ids)
		inst := f.advance(ids[0])
		enc.Encode(map[string]any{"Instances": map[string]any{"Instance": []map[string]any{{
			"InstanceId": inst.id, "Status": map[string]string{StatusPending: "Pending", StatusRunning: "Running",
				StatusTerminated: "Stopped"}[inst.state],
			"ImageId":       query.Get("ImageId"),
			"ClientToken":   inst.clientToken,
			"VpcAttributes": map[string]any{"PrivateIpAddress": map[string]any{"IpAddress": []string{"10.0.0.22"}}},
		}}}})
	case "DescribeUserData":
		inst := f.instances[query.Get("InstanceId")]
		enc.Encode(map[string]any{"InstanceId": inst.id,
			"UserData": base64.StdEncoding.EncodeToString([]byte(inst.userData))})
	case "DeleteInstance":
		inst := f.instances[query.Get("InstanceId")]
		if inst != nil {
			inst.state = StatusTerminated
		}
		enc.Encode(map[string]string{"RequestId": "fake"})
	default:
		f.reject(w, http.StatusBadRequest, "InvalidAction", action, false)
	}
}

// aliyunExpectedCredentials mirrors awsExpectedCredentials for the RPC
// protocol: base identity keys without a token, or assumed-role session keys
// together with SecurityToken.
func (f *fakeCloud) aliyunExpectedCredentials(query url.Values) (string, bool) {
	accessKey := query.Get("AccessKeyId")
	if token := query.Get("SecurityToken"); token != "" {
		if token != fakeSessionToken || accessKey != fakeAliyunSessionAK {
			return "", false
		}
		return fakeAliyunSessionSK, true
	}
	if accessKey != fakeAliyunAccessKey {
		return "", false
	}
	return fakeAliyunSecretKey, true
}
