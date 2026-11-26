package provision

import (
	"encoding/json"
	"log/slog"
	"os"
	"time"
)

const (
	// EnvBuiltinEnabled turns on the self-hosted built-in driver. It is only
	// consulted when MULTICA_CLOUD_URL (the managed Fleet endpoint) is empty.
	EnvBuiltinEnabled = "MULTICA_CLOUD_BUILTIN"
	// EnvBuiltinConfig carries the JSON configuration for the built-in driver.
	EnvBuiltinConfig = "MULTICA_CLOUD_BUILTIN_CONFIG"
)

type envProvider struct {
	Mode            string `json:"mode"`
	Region          string `json:"region"`
	Endpoint        string `json:"endpoint"`
	AccessKeyEnv    string `json:"access_key_env"`
	SecretKeyEnv    string `json:"secret_key_env"`
	SessionTokenEnv string `json:"session_token_env"`
}

type envDefaults struct {
	Region           string   `json:"region"`
	ImageID          string   `json:"image_id"`
	InstanceType     string   `json:"instance_type"`
	SubnetID         string   `json:"subnet_id"`
	SecurityGroupIDs []string `json:"security_group_ids"`
	InstanceProfile  string   `json:"instance_profile"`
}

type envConfig struct {
	StatePath            string                 `json:"state_path"`
	WorkspaceURL         string                 `json:"workspace_url"`
	GatewayURL           string                 `json:"gateway_url"`
	ReadinessTimeoutSecs int                    `json:"readiness_timeout_seconds"`
	PollIntervalMillis   int                    `json:"poll_interval_millis"`
	Providers            map[string]envProvider `json:"providers"`
	Defaults             map[string]envDefaults `json:"defaults"`
}

// BuiltinEnabled reports whether the self-hosted driver switch is on.
func BuiltinEnabled() bool {
	return os.Getenv(EnvBuiltinEnabled) == "true"
}

// NewFromEnv builds a LocalProvisioner from MULTICA_CLOUD_BUILTIN_CONFIG. A
// configuration or credential error is returned so the caller can keep the
// managed-Fleet path instead of starting a half-wired driver.
func NewFromEnv() (*LocalProvisioner, error) {
	var raw envConfig
	if err := json.Unmarshal([]byte(os.Getenv(EnvBuiltinConfig)), &raw); err != nil {
		return nil, &DriverError{Code: ErrConfiguration, Message: "invalid " + EnvBuiltinConfig + ": " + err.Error()}
	}
	providers := map[string]CloudProvider{}
	for name, pc := range raw.Providers {
		provider, err := buildProvider(name, pc)
		if err != nil {
			return nil, err
		}
		providers[name] = provider
	}
	defaults := map[string]ProviderDefaults{}
	for name, d := range raw.Defaults {
		defaults[name] = ProviderDefaults{
			Region: d.Region, ImageID: d.ImageID, InstanceType: d.InstanceType,
			SubnetID: d.SubnetID, SecurityGroupIDs: d.SecurityGroupIDs, InstanceProfile: d.InstanceProfile,
		}
	}
	cfg := Config{
		Enabled: true, StatePath: raw.StatePath, Providers: providers, Defaults: defaults,
		WorkspaceURL: raw.WorkspaceURL, GatewayURL: raw.GatewayURL,
		ReadinessTimeout: time.Duration(raw.ReadinessTimeoutSecs) * time.Second,
		PollInterval:     time.Duration(raw.PollIntervalMillis) * time.Millisecond,
	}
	return New(cfg)
}

func buildProvider(name string, pc envProvider) (CloudProvider, error) {
	switch name {
	case ProviderAWS:
		return NewAwsProvider(AWSConfig{
			Mode: pc.Mode, Region: pc.Region, Endpoint: pc.Endpoint,
			AccessKeyEnv:    firstNonEmpty(pc.AccessKeyEnv, "HELIOS_AWS_AK"),
			SecretKeyEnv:    firstNonEmpty(pc.SecretKeyEnv, "HELIOS_AWS_SK"),
			SessionTokenEnv: pc.SessionTokenEnv,
		})
	case ProviderAliyun:
		return NewAliyunProvider(AliyunConfig{
			Mode: pc.Mode, Region: pc.Region, Endpoint: pc.Endpoint,
			AccessKeyEnv: firstNonEmpty(pc.AccessKeyEnv, "HELIOS_ALIYUN_AK"),
			SecretKeyEnv: firstNonEmpty(pc.SecretKeyEnv, "HELIOS_ALIYUN_SK"),
		})
	default:
		return nil, &DriverError{Code: ErrConfiguration, Message: "unsupported built-in provider: " + name,
			Actionable: "The built-in driver supports aws and aliyun."}
	}
}

// NewFromEnvOrDisable returns the built-in provisioner when configured, or nil
// after logging the reason. Callers leave the managed-Fleet client in place on nil.
func NewFromEnvOrDisable() *LocalProvisioner {
	provisioner, err := NewFromEnv()
	if err != nil {
		slog.Warn("built-in cloud runtime disabled: " + err.Error())
		return nil
	}
	return provisioner
}
