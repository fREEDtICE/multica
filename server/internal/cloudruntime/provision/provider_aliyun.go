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

var aliyunAuthCodes = map[string]bool{
	"InvalidAccessKeyId.NotFound": true, "SignatureDoesNotMatch": true,
	"Forbidden.RAM": true, "Unauthorized": true, "InvalidAccessKeyId.BlankCheckFailed": true,
}

// AliyunConfig configures the ECS RPC adapter.
type AliyunConfig struct {
	Region       string
	Endpoint     string
	Mode         string
	AccessKeyEnv string
	SecretKeyEnv string
}

// AliyunProvider implements CloudProvider against the ECS RPC API.
type AliyunProvider struct {
	cfg       AliyunConfig
	accessKey string
	secretKey string
	client    *http.Client
}

// NewAliyunProvider builds an adapter, reading live credentials from the env.
func NewAliyunProvider(cfg AliyunConfig) (*AliyunProvider, error) {
	p := &AliyunProvider{cfg: cfg, client: &http.Client{Timeout: 20 * time.Second}}
	if cfg.Mode == "fake" {
		p.accessKey = fakeAliyunAccessKey
		p.secretKey = fakeAliyunSecretKey
		return p, nil
	}
	p.accessKey = os.Getenv(cfg.AccessKeyEnv)
	p.secretKey = os.Getenv(cfg.SecretKeyEnv)
	if p.accessKey == "" || p.secretKey == "" {
		return nil, &DriverError{Code: ErrConfiguration,
			Message:    "missing Alibaba Cloud credentials in " + cfg.AccessKeyEnv + "/" + cfg.SecretKeyEnv,
			Actionable: "Provide an authorized RAM AccessKey or switch the driver to fake mode."}
	}
	return p, nil
}

func (p *AliyunProvider) Name() string { return ProviderAliyun }

func (p *AliyunProvider) endpoint() string {
	if p.cfg.Endpoint != "" {
		return p.cfg.Endpoint
	}
	return "https://ecs." + p.cfg.Region + ".aliyuncs.com"
}

func (p *AliyunProvider) call(ctx context.Context, action string, params url.Values) (map[string]any, error) {
	query := url.Values{}
	for k, vs := range params {
		for _, v := range vs {
			query.Set(k, v)
		}
	}
	query.Set("Action", action)
	query.Set("Format", "JSON")
	query.Set("Version", aliyunVersion)
	query.Set("AccessKeyId", p.accessKey)
	query.Set("SignatureMethod", "HMAC-SHA1")
	query.Set("SignatureVersion", "1.0")
	query.Set("SignatureNonce", strconv.FormatInt(time.Now().UnixNano(), 10))
	query.Set("Timestamp", time.Now().UTC().Format("2006-01-02T15:04:05Z"))
	query.Set("RegionId", p.cfg.Region)
	query.Set("Signature", AliyunSignature(query, p.secretKey, http.MethodGet))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint()+"/?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, &DriverError{Code: ErrUnavailable, Message: "ECS endpoint unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, &DriverError{Code: ErrUnavailable, Message: "invalid ECS response"}
	}
	if resp.StatusCode >= 400 {
		return nil, p.wrapError(payload, resp.StatusCode)
	}
	return payload, nil
}

func (p *AliyunProvider) wrapError(payload map[string]any, status int) error {
	code, _ := payload["Code"].(string)
	message, _ := payload["Message"].(string)
	if code == "" {
		code = strconv.Itoa(status)
	}
	if aliyunAuthCodes[code] {
		return &DriverError{Code: ErrAuthorization, Message: "Aliyun " + code + ": " + message,
			Actionable: "Check the RAM policy for this ECS/VPC action or use authorized credentials."}
	}
	return &DriverError{Code: ErrUnavailable, Message: "Aliyun ECS request failed: " + code + " " + message}
}

func (p *AliyunProvider) Authorize(ctx context.Context) (map[string]string, error) {
	if _, err := p.call(ctx, "DescribeRegions", url.Values{}); err != nil {
		return nil, err
	}
	return map[string]string{"account": p.accessKey, "region": p.cfg.Region, "authorized": "true"}, nil
}

func (p *AliyunProvider) EnsureNetwork(ctx context.Context, subnetID string, securityGroupIDs []string) error {
	if _, err := p.call(ctx, "DescribeVSwitches", url.Values{"VSwitchId.1": {subnetID}}); err != nil {
		return err
	}
	for i, group := range securityGroupIDs {
		params := url.Values{}
		params.Set("SecurityGroupId."+strconv.Itoa(i+1), group)
		if _, err := p.call(ctx, "DescribeSecurityGroups", params); err != nil {
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
	for i, group := range spec.SecurityGroupIDs {
		params.Set("SecurityGroupId."+strconv.Itoa(i+1), group)
	}
	i := 1
	for k, v := range specTags(spec) {
		params.Set("Tag."+strconv.Itoa(i)+".Key", k)
		params.Set("Tag."+strconv.Itoa(i)+".Value", v)
		i++
	}
	payload, err := p.call(ctx, "RunInstances", params)
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
	payload, err := p.call(ctx, "DescribeInstances", url.Values{"InstanceIds": {string(raw)}})
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
	payload, err := p.call(ctx, "DescribeUserData", url.Values{"InstanceId": {id}})
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
	_, err := p.call(ctx, "DeleteInstance", url.Values{"InstanceId": {id}, "Force": {"true"}})
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
