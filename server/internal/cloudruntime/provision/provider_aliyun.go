package provision

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

const aliyunVersion = "2014-05-26"
const aliyunSTSVersion = "2015-08-15"

var aliyunAuthCodes = map[string]bool{
	"InvalidAccessKeyId.NotFound": true, "SignatureDoesNotMatch": true,
	"Forbidden.RAM": true, "Unauthorized": true, "InvalidAccessKeyId.BlankCheckFailed": true,
	"NoPermission": true, "AccessDenied": true,
}

// AliyunConfig configures the ECS/RAM STS adapter.
type AliyunConfig struct {
	Region       string
	Endpoint     string
	STSEndpoint  string
	Mode         string
	AccessKeyEnv string
	SecretKeyEnv string
}

// AliyunProvider implements CloudProvider against the ECS RPC API. As with the
// AWS adapter, one type covers both the deployment identity and an assumed
// RAM role backed by short-lived STS credentials.
type AliyunProvider struct {
	cfg           AliyunConfig
	creds         CredentialProvider
	client        *http.Client
	assumeRoleARN string
}

// NewAliyunProvider builds an adapter. Missing deployment keys disable signed
// calls with ErrIdentityMissing instead of failing construction, so the
// driver can surface the gap in the account-connection UI.
func NewAliyunProvider(cfg AliyunConfig) (*AliyunProvider, error) {
	p := &AliyunProvider{cfg: cfg, client: &http.Client{Timeout: 20 * time.Second}}
	if cfg.Mode == "fake" {
		p.creds = staticCredentials{creds: Credentials{AccessKey: fakeAliyunAccessKey, SecretKey: fakeAliyunSecretKey}}
		return p, nil
	}
	p.creds = staticCredentials{creds: Credentials{
		AccessKey: os.Getenv(cfg.AccessKeyEnv),
		SecretKey: os.Getenv(cfg.SecretKeyEnv),
	}}
	return p, nil
}

func (p *AliyunProvider) Name() string { return ProviderAliyun }

// IdentityReady reports whether the deployment RAM identity is configured.
func (p *AliyunProvider) IdentityReady() bool {
	creds, err := p.creds.Credentials(context.Background())
	return err == nil && creds.AccessKey != ""
}

func (p *AliyunProvider) endpoint() string {
	if p.cfg.Endpoint != "" {
		return p.cfg.Endpoint
	}
	return "https://ecs." + p.cfg.Region + ".aliyuncs.com"
}

func (p *AliyunProvider) stsEndpoint() string {
	if p.cfg.STSEndpoint != "" {
		return p.cfg.STSEndpoint
	}
	if p.cfg.Mode == "fake" {
		return p.endpoint()
	}
	return "https://sts." + p.cfg.Region + ".aliyuncs.com"
}

// call signs and sends an RPC request against the given endpoint and returns
// the parsed JSON envelope.
func (p *AliyunProvider) call(ctx context.Context, endpoint, action, version string, params url.Values) (map[string]any, error) {
	creds, err := p.creds.Credentials(ctx)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	for k, vs := range params {
		for _, v := range vs {
			query.Set(k, v)
		}
	}
	query.Set("Action", action)
	query.Set("Format", "JSON")
	query.Set("Version", version)
	query.Set("AccessKeyId", creds.AccessKey)
	if creds.SessionToken != "" {
		query.Set("SecurityToken", creds.SessionToken)
	}
	query.Set("SignatureMethod", "HMAC-SHA1")
	query.Set("SignatureVersion", "1.0")
	query.Set("SignatureNonce", strconv.FormatInt(time.Now().UnixNano(), 10))
	query.Set("Timestamp", time.Now().UTC().Format("2006-01-02T15:04:05Z"))
	query.Set("RegionId", p.cfg.Region)
	query.Set("Signature", AliyunSignature(query, creds.SecretKey, http.MethodGet))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, &DriverError{Code: ErrUnavailable, Message: "cloud endpoint unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, &DriverError{Code: ErrUnavailable, Message: "invalid cloud response"}
	}
	if resp.StatusCode >= 400 {
		return nil, p.wrapError(payload, resp.StatusCode)
	}
	return payload, nil
}

func (p *AliyunProvider) ecsCall(ctx context.Context, action string, params url.Values) (map[string]any, error) {
	return p.call(ctx, p.endpoint(), action, aliyunVersion, params)
}

func (p *AliyunProvider) stsCall(ctx context.Context, action string, params url.Values) (map[string]any, error) {
	return p.call(ctx, p.stsEndpoint(), action, aliyunSTSVersion, params)
}

func (p *AliyunProvider) wrapError(payload map[string]any, status int) error {
	code, _ := payload["Code"].(string)
	message, _ := payload["Message"].(string)
	if code == "" {
		code = strconv.Itoa(status)
	}
	if aliyunAuthCodes[code] {
		return &DriverError{Code: ErrAuthorization, Message: "Aliyun " + code + ": " + message,
			Actionable: "Check the RAM role trust policy and permissions, then verify the authorization again."}
	}
	return &DriverError{Code: ErrUnavailable, Message: "Aliyun request failed: " + code + " " + message}
}

// Identity returns the deployment RAM identity via STS GetCallerIdentity.
func (p *AliyunProvider) Identity(ctx context.Context) (map[string]string, error) {
	payload, err := p.stsCall(ctx, "GetCallerIdentity", url.Values{})
	if err != nil {
		return nil, err
	}
	identity := map[string]string{
		"account": stringField(payload, "AccountId"),
		"arn":     stringField(payload, "Arn"),
		"user_id": stringField(payload, "PrincipalId"),
	}
	return identity, nil
}

// Authorize proves the current credentials can reach ECS, mirroring the AWS
// adapter's pre-flight check before any resource is created.
func (p *AliyunProvider) Authorize(ctx context.Context) (map[string]string, error) {
	if _, err := p.ecsCall(ctx, "DescribeRegions", url.Values{}); err != nil {
		return nil, err
	}
	identity, err := p.Identity(ctx)
	if err != nil {
		return nil, err
	}
	identity["authorized"] = "true"
	if p.assumeRoleARN != "" {
		identity["role_arn"] = p.assumeRoleARN
	}
	return identity, nil
}

// assumeRoleOnce performs one RAM AssumeRole call.
func (p *AliyunProvider) assumeRoleOnce(ctx context.Context, roleARN, sessionName string, duration time.Duration) (Credentials, time.Time, error) {
	payload, err := p.stsCall(ctx, "AssumeRole", url.Values{
		"RoleArn":         {roleARN},
		"RoleSessionName": {sessionName},
		"DurationSeconds": {strconv.Itoa(int(duration.Seconds()))},
	})
	if err != nil {
		return Credentials{}, time.Time{}, err
	}
	credMap, ok := payload["Credentials"].(map[string]any)
	if !ok {
		return Credentials{}, time.Time{}, &DriverError{Code: ErrUnavailable, Message: "AssumeRole returned no credentials"}
	}
	expires, _ := time.Parse("2006-01-02T15:04:05Z", stringField(credMap, "Expiration"))
	return Credentials{
		AccessKey:    stringField(credMap, "AccessKeyId"),
		SecretKey:    stringField(credMap, "AccessKeySecret"),
		SessionToken: stringField(credMap, "SecurityToken"),
	}, expires, nil
}

// AssumeRole returns a derived provider running on short-lived RAM role
// credentials, together with the verified role identity.
func (p *AliyunProvider) AssumeRole(ctx context.Context, roleARN, sessionName string, durationSecs int) (CloudProvider, map[string]string, error) {
	duration := time.Duration(durationSecs) * time.Second
	session := &assumedCredentials{
		parent: p, roleARN: roleARN, sessionName: sessionName, duration: duration,
		refreshAhead: 60 * time.Second,
	}
	creds, err := session.Credentials(ctx)
	if err != nil {
		return nil, nil, err
	}
	derived := &AliyunProvider{
		cfg:           p.cfg,
		creds:         session,
		client:        p.client,
		assumeRoleARN: roleARN,
	}
	identity, err := derived.Authorize(ctx)
	if err != nil {
		return nil, nil, err
	}
	identity["role_arn"] = roleARN
	identity["session_access_key"] = creds.AccessKey
	return derived, identity, nil
}

func (p *AliyunProvider) EnsureNetwork(ctx context.Context, subnetID string, securityGroupIDs []string) error {
	if _, err := p.ecsCall(ctx, "DescribeVSwitches", url.Values{"VSwitchId.1": {subnetID}}); err != nil {
		return err
	}
	for i, group := range securityGroupIDs {
		params := url.Values{}
		params.Set("SecurityGroupId."+strconv.Itoa(i+1), group)
		if _, err := p.ecsCall(ctx, "DescribeSecurityGroups", params); err != nil {
			return err
		}
	}
	return nil
}

func (p *AliyunProvider) RunInstance(ctx context.Context, spec Spec) (*Instance, error) {
	params := url.Values{
		"ImageId": {spec.ImageID}, "InstanceType": {spec.InstanceType},
		"VSwitchId": {spec.SubnetID}, "Amount": {"1"}, "InstanceName": {"helios-" + spec.RuntimeName},
		"UserData":    {base64.StdEncoding.EncodeToString([]byte(spec.UserData))},
		"RamRoleName": {spec.InstanceProfile}, "ClientToken": {spec.ClientToken},
	}
	if spec.DiskSizeGB > 0 {
		params.Set("SystemDisk.Size", strconv.Itoa(spec.DiskSizeGB))
	}
	for i, group := range spec.SecurityGroupIDs {
		params.Set("SecurityGroupId."+strconv.Itoa(i+1), group)
	}
	i := 1
	for k, v := range specTags(spec) {
		params.Set("Tag."+strconv.Itoa(i)+".Key", k)
		params.Set("Tag."+strconv.Itoa(i)+".Value", v)
		i++
	}
	payload, err := p.ecsCall(ctx, "RunInstances", params)
	if err != nil {
		return nil, err
	}
	id, _ := payload["InstanceIdSets"].(map[string]any)["InstanceIdSet"].([]any)[0].(string)
	return p.DescribeInstance(ctx, id)
}

func (p *AliyunProvider) DescribeInstance(ctx context.Context, id string) (*Instance, error) {
	raw, err := json.Marshal([]string{id})
	if err != nil {
		return nil, err
	}
	payload, err := p.ecsCall(ctx, "DescribeInstances", url.Values{"InstanceIds": {string(raw)}})
	if err != nil {
		return nil, err
	}
	list, _ := payload["Instances"].(map[string]any)["Instance"].([]any)
	if len(list) == 0 {
		return nil, &DriverError{Code: ErrUnavailable, Message: "instance not found: " + id}
	}
	item, _ := list[0].(map[string]any)
	return p.parseInstance(item), nil
}

func (p *AliyunProvider) GetUserData(ctx context.Context, id string) (string, error) {
	payload, err := p.ecsCall(ctx, "DescribeUserData", url.Values{"InstanceId": {id}})
	if err != nil {
		return "", err
	}
	encoded, _ := payload["UserData"].(string)
	if encoded == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	return string(raw), err
}

func (p *AliyunProvider) ConsoleOutput(ctx context.Context, id string) (string, error) {
	// ECS exposes the serial console through a separate action; the built-in
	// driver relies on the registration path for readiness and returns empty.
	return "", nil
}

func (p *AliyunProvider) TerminateInstance(ctx context.Context, id string) error {
	_, err := p.ecsCall(ctx, "DeleteInstance", url.Values{"InstanceId": {id}, "Force": {"true"}})
	return err
}

func (p *AliyunProvider) parseInstance(item map[string]any) *Instance {
	status, _ := item["Status"].(string)
	state := map[string]string{
		"Pending": StatusPending, "Running": StatusRunning,
		"Stopping": StatusTerminated, "Stopped": StatusTerminated,
	}[status]
	if state == "" {
		state = StatusPending
	}
	privateIP := ""
	if vpc, ok := item["VpcAttributes"].(map[string]any); ok {
		if ips, ok := vpc["PrivateIpAddress"].(map[string]any)["IpAddress"].([]any); ok && len(ips) > 0 {
			privateIP, _ = ips[0].(string)
		}
	}
	id, _ := item["InstanceId"].(string)
	imageID, _ := item["ImageId"].(string)
	token, _ := item["ClientToken"].(string)
	role, _ := item["RamRoleName"].(string)
	return &Instance{
		ID: id, State: state, Provider: ProviderAliyun, Region: p.cfg.Region,
		ImageID: imageID, PrivateIP: privateIP, ClientToken: token, Profile: role,
	}
}

func stringField(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return value
}
