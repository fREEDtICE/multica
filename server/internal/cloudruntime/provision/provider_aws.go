package provision

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const awsAPIVersion = "2016-11-15"

var awsAuthCodes = map[string]bool{
	"AuthFailure": true, "UnauthorizedOperation": true, "InvalidClientTokenId": true,
	"SignatureDoesNotMatch": true, "UnauthorizedAccess": true, "AccessDenied": true,
}

// AWSConfig configures the EC2/STS adapter. In fake mode Endpoint points at
// the in-test server and the fixed fake credentials are used.
type AWSConfig struct {
	Region          string
	Endpoint        string
	STSEndpoint     string
	Mode            string
	AccessKeyEnv    string
	SecretKeyEnv    string
	SessionTokenEnv string
}

// AwsProvider implements CloudProvider against the EC2 query API. The same
// type serves both the deployment identity (static env credentials) and an
// assumed role (short-lived STS credentials), since signing and the wire
// contract are identical.
type AwsProvider struct {
	cfg           AWSConfig
	creds         CredentialProvider
	client        *http.Client
	assumeRoleARN string
}

// NewAwsProvider builds an adapter. In live mode a missing deployment key no
// longer fails construction: the driver stays available so the UI can report
// "deployment identity missing" and disable account connection; every signed
// call then returns ErrIdentityMissing.
func NewAwsProvider(cfg AWSConfig) (*AwsProvider, error) {
	p := &AwsProvider{cfg: cfg, client: &http.Client{Timeout: 20 * time.Second}}
	if cfg.Mode == "fake" {
		p.creds = staticCredentials{creds: Credentials{AccessKey: fakeAWSAccessKey, SecretKey: fakeAWSSecretKey}}
		return p, nil
	}
	p.creds = staticCredentials{creds: Credentials{
		AccessKey:    os.Getenv(cfg.AccessKeyEnv),
		SecretKey:    os.Getenv(cfg.SecretKeyEnv),
		SessionToken: os.Getenv(cfg.SessionTokenEnv),
	}}
	return p, nil
}

func (p *AwsProvider) Name() string { return ProviderAWS }

// IdentityReady reports whether the deployment owns a cloud-recognised
// identity. Account connection and provisioning stay disabled until true.
func (p *AwsProvider) IdentityReady() bool {
	creds, err := p.creds.Credentials(context.Background())
	return err == nil && creds.AccessKey != ""
}

func (p *AwsProvider) ec2Endpoint() string {
	if p.cfg.Endpoint != "" {
		return p.cfg.Endpoint
	}
	return "https://ec2." + p.cfg.Region + ".amazonaws.com"
}

func (p *AwsProvider) stsEndpoint() string {
	if p.cfg.STSEndpoint != "" {
		return p.cfg.STSEndpoint
	}
	if p.cfg.Mode == "fake" {
		return p.ec2Endpoint()
	}
	return "https://sts." + p.cfg.Region + ".amazonaws.com"
}

// call signs and sends an EC2/STS query request and parses the XML envelope.
func (p *AwsProvider) call(ctx context.Context, endpoint, service, action, version string, params url.Values) (*xmlNode, error) {
	creds, err := p.creds.Credentials(ctx)
	if err != nil {
		return nil, err
	}
	params.Set("Action", action)
	params.Set("Version", version)
	rawURL := endpoint + "/"
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	body := []byte(params.Encode())
	headers, err := AWSSignRequest(http.MethodPost, rawURL, url.Values{},
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		body, creds.AccessKey, creds.SecretKey, creds.SessionToken, p.cfg.Region, service, amzDate)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, &DriverError{Code: ErrUnavailable, Message: "EC2 endpoint unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 400 {
		return nil, p.wrapError(data)
	}
	return parseXML(data)
}

func (p *AwsProvider) wrapError(data []byte) error {
	root, parseErr := parseXML(data)
	code, message := "", string(data)
	if parseErr == nil && root != nil {
		code = root.deepText("Code")
		if text := root.deepText("Message"); text != "" {
			message = text
		}
	}
	if awsAuthCodes[code] {
		return &DriverError{Code: ErrAuthorization, Message: "AWS " + code + ": " + message,
			Actionable: "Check the role trust policy and IAM permissions, then verify the authorization again."}
	}
	return &DriverError{Code: ErrUnavailable, Message: "AWS request failed: " + code + " " + message}
}

// Identity returns the deployment identity behind the current credentials via
// STS GetCallerIdentity.
func (p *AwsProvider) Identity(ctx context.Context) (map[string]string, error) {
	root, err := p.call(ctx, p.stsEndpoint(), "sts", "GetCallerIdentity", "2011-06-15", url.Values{})
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"account": root.text("GetCallerIdentityResult", "Account"),
		"arn":     root.text("GetCallerIdentityResult", "Arn"),
		"user_id": root.text("GetCallerIdentityResult", "UserId"),
	}, nil
}

// Authorize verifies the current credentials before any resource is created.
func (p *AwsProvider) Authorize(ctx context.Context) (map[string]string, error) {
	return p.Identity(ctx)
}

// assumeRoleOnce performs one STS AssumeRole and returns the short-lived
// session. It satisfies roleAssumer for credential refresh.
func (p *AwsProvider) assumeRoleOnce(ctx context.Context, roleARN, sessionName string, duration time.Duration) (Credentials, time.Time, error) {
	params := url.Values{
		"RoleArn":         {roleARN},
		"RoleSessionName": {sessionName},
		"DurationSeconds": {strconv.Itoa(int(duration.Seconds()))},
	}
	root, err := p.call(ctx, p.stsEndpoint(), "sts", "AssumeRole", "2011-06-15", params)
	if err != nil {
		return Credentials{}, time.Time{}, err
	}
	cred := root.first("AssumeRoleResult", "Credentials")
	if cred == nil {
		return Credentials{}, time.Time{}, &DriverError{Code: ErrUnavailable, Message: "AssumeRole returned no credentials"}
	}
	expires, _ := time.Parse(time.RFC3339, cred.text("Expiration"))
	return Credentials{
		AccessKey:    cred.text("AccessKeyId"),
		SecretKey:    cred.text("SecretAccessKey"),
		SessionToken: cred.text("SessionToken"),
	}, expires, nil
}

// AssumeRole mints short-lived credentials for the administrator-authorized
// role and returns a derived provider that signs every subsequent call with
// them, together with the assumed role identity.
func (p *AwsProvider) AssumeRole(ctx context.Context, roleARN, sessionName string, durationSecs int) (CloudProvider, map[string]string, error) {
	duration := time.Duration(durationSecs) * time.Second
	session := &assumedCredentials{
		parent: p, roleARN: roleARN, sessionName: sessionName, duration: duration,
		refreshAhead: 60 * time.Second,
	}
	creds, err := session.Credentials(ctx)
	if err != nil {
		return nil, nil, err
	}
	derived := &AwsProvider{
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

func (p *AwsProvider) EnsureNetwork(ctx context.Context, subnetID string, securityGroupIDs []string) error {
	params := url.Values{"SubnetId.1": {subnetID}}
	if _, err := p.call(ctx, p.ec2Endpoint(), "ec2", "DescribeSubnets", awsAPIVersion, params); err != nil {
		return err
	}
	for i, group := range securityGroupIDs {
		gp := url.Values{}
		gp.Set("GroupId."+strconv.Itoa(i+1), group)
		if _, err := p.call(ctx, p.ec2Endpoint(), "ec2", "DescribeSecurityGroups", awsAPIVersion, gp); err != nil {
			return err
		}
	}
	return nil
}

func (p *AwsProvider) RunInstance(ctx context.Context, spec Spec) (*Instance, error) {
	params := url.Values{
		"ImageId": {spec.ImageID}, "InstanceType": {spec.InstanceType},
		"SubnetId": {spec.SubnetID}, "MinCount": {"1"}, "MaxCount": {"1"},
		"UserData":                {base64.StdEncoding.EncodeToString([]byte(spec.UserData))},
		"IamInstanceProfile.Name": {spec.InstanceProfile}, "ClientToken": {spec.ClientToken},
	}
	if spec.DiskSizeGB > 0 {
		params.Set("BlockDeviceMapping.1.DeviceName", "/dev/xvda")
		params.Set("BlockDeviceMapping.1.Ebs.VolumeSize", strconv.Itoa(spec.DiskSizeGB))
		params.Set("BlockDeviceMapping.1.Ebs.DeleteOnTermination", "true")
	}
	for i, group := range spec.SecurityGroupIDs {
		params.Set("SecurityGroupId."+strconv.Itoa(i+1), group)
	}
	params.Set("TagSpecification.1.ResourceType", "instance")
	i := 1
	for k, v := range specTags(spec) {
		params.Set("Tag.1."+strconv.Itoa(i)+".Key", k)
		params.Set("Tag.1."+strconv.Itoa(i)+".Value", v)
		i++
	}
	root, err := p.call(ctx, p.ec2Endpoint(), "ec2", "RunInstances", awsAPIVersion, params)
	if err != nil {
		return nil, err
	}
	item := root.first("instancesSet", "item")
	if item == nil {
		return nil, &DriverError{Code: ErrUnavailable, Message: "RunInstances returned no instance"}
	}
	return p.parseInstance(item), nil
}

func (p *AwsProvider) DescribeInstance(ctx context.Context, id string) (*Instance, error) {
	root, err := p.call(ctx, p.ec2Endpoint(), "ec2", "DescribeInstances", awsAPIVersion,
		url.Values{"InstanceId.1": {id}})
	if err != nil {
		return nil, err
	}
	item := root.first("reservationSet", "item", "instancesSet", "item")
	if item == nil {
		return nil, &DriverError{Code: ErrUnavailable, Message: "instance not found: " + id}
	}
	return p.parseInstance(item), nil
}

func (p *AwsProvider) GetUserData(ctx context.Context, id string) (string, error) {
	root, err := p.call(ctx, p.ec2Endpoint(), "ec2", "DescribeInstanceAttribute", awsAPIVersion,
		url.Values{"InstanceId": {id}, "Attribute": {"userData"}})
	if err != nil {
		return "", err
	}
	encoded := root.text("instanceAttribute", "userData", "value")
	if encoded == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	return string(raw), err
}

func (p *AwsProvider) ConsoleOutput(ctx context.Context, id string) (string, error) {
	root, err := p.call(ctx, p.ec2Endpoint(), "ec2", "GetConsoleOutput", awsAPIVersion,
		url.Values{"InstanceId": {id}})
	if err != nil {
		return "", err
	}
	encoded := root.text("output")
	if encoded == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	return string(raw), err
}

func (p *AwsProvider) TerminateInstance(ctx context.Context, id string) error {
	_, err := p.call(ctx, p.ec2Endpoint(), "ec2", "TerminateInstances", awsAPIVersion,
		url.Values{"InstanceId.1": {id}})
	return err
}

func (p *AwsProvider) parseInstance(item *xmlNode) *Instance {
	state := item.text("instanceState", "name")
	if state == "" {
		state = StatusPending
	}
	profile := item.text("iamInstanceProfile", "arn")
	if profile == "" {
		profile = item.text("iamInstanceProfile", "name")
	}
	if idx := strings.LastIndex(profile, "/"); idx >= 0 {
		profile = profile[idx+1:]
	}
	tags := map[string]string{}
	for _, tag := range item.findAll("item") {
		if k := tag.text("key"); k != "" {
			tags[k] = tag.text("value")
		}
	}
	return &Instance{
		ID:          item.text("instanceId"),
		State:       state,
		Provider:    ProviderAWS,
		Region:      p.cfg.Region,
		ImageID:     item.text("imageId"),
		PrivateIP:   item.text("privateIpAddress"),
		PublicIP:    item.text("ipAddress"),
		ClientToken: item.text("clientToken"),
		Profile:     profile,
		Tags:        tags,
	}
}

func specTags(spec Spec) map[string]string {
	return map[string]string{
		"Name":             "helios-" + spec.RuntimeName,
		"helios.workspace": spec.WorkspaceID,
		"helios.runtime":   spec.RuntimeName,
		"helios.managed":   "true",
	}
}
