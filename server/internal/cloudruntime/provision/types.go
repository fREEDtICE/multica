// Package provision implements a self-hosted cloud runtime provisioner that
// can stand in for the managed Multica Cloud Fleet. It talks the real AWS
// EC2 (Signature V4) and Alibaba Cloud ECS (RPC HMAC-SHA1) wire protocols
// using only the standard library, so a deployment without MULTICA_CLOUD_URL
// can still open Linux Agent runtimes directly in an authorized account.
package provision

import (
	"context"
	"errors"
	"time"
)

// Provider names supported by the built-in driver.
const (
	ProviderAWS    = "aws"
	ProviderAliyun = "aliyun"
)

// Node lifecycle states. They mirror the status vocabulary already used by
// the cloud runtime web client so the UI needs no translation.
const (
	StatusLaunching  = "launching"
	StatusPending    = "pending"
	StatusRunning    = "running"
	StatusTerminated = "terminated"
	StatusFailed     = "failed"
)

// MaxLeaseSeconds caps the short-lived task lease rendered into user-data.
// The instance never receives a long-lived cloud management credential.
const MaxLeaseSeconds = 900

// ErrCode is a stable, machine-readable failure reason surfaced to callers.
type ErrCode string

const (
	ErrAuthorization ErrCode = "authorization_failed"
	ErrTimeout       ErrCode = "provision_timeout"
	ErrUnavailable   ErrCode = "runtime_unavailable"
	ErrConfiguration ErrCode = "configuration_error"
	ErrOrphan        ErrCode = "orphan_resource"
)

// DriverError carries a stable reason plus an actionable operator hint.
type DriverError struct {
	Code       ErrCode
	Message    string
	Actionable string
}

func (e *DriverError) Error() string { return string(e.Code) + ": " + e.Message }

// AsDriverError unwraps a DriverError from err when present.
func AsDriverError(err error) (*DriverError, bool) {
	var target *DriverError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// Spec is the provider-independent launch request.
type Spec struct {
	Provider         string
	Name             string
	Region           string
	ImageID          string
	InstanceType     string
	SubnetID         string
	SecurityGroupIDs []string
	InstanceProfile  string
	UserData         string
	// ClientToken is the stable idempotency key derived from the node identity,
	// so a retried launch never opens a second billable instance.
	ClientToken string
	WorkspaceID string
	RuntimeName string
}

// Instance is the normalized cloud instance record.
type Instance struct {
	ID            string
	State         string
	Provider      string
	Region        string
	ImageID       string
	PrivateIP     string
	PublicIP      string
	ClientToken   string
	Profile       string
	UserData      string
	ConsoleOutput string
	Tags          map[string]string
}

// CloudProvider is the contract each vendor adapter implements.
type CloudProvider interface {
	Name() string
	// Authorize verifies the credentials before any resource is created.
	Authorize(ctx context.Context) (map[string]string, error)
	EnsureNetwork(ctx context.Context, subnetID string, securityGroupIDs []string) error
	RunInstance(ctx context.Context, spec Spec) (*Instance, error)
	DescribeInstance(ctx context.Context, id string) (*Instance, error)
	TerminateInstance(ctx context.Context, id string) error
	GetUserData(ctx context.Context, id string) (string, error)
	ConsoleOutput(ctx context.Context, id string) (string, error)
}

// ProviderDefaults are the account/network settings an admin configures once
// per provider; node requests only override name and instance type.
type ProviderDefaults struct {
	Region           string
	ImageID          string
	InstanceType     string
	SubnetID         string
	SecurityGroupIDs []string
	InstanceProfile  string
}

// Config holds the runtime settings for one built-in provisioner.
type Config struct {
	// Defaults holds the per-provider account/network configuration.
	Defaults map[string]ProviderDefaults
	// Enabled gates the driver independently of MULTICA_CLOUD_URL.
	Enabled bool
	// StatePath, when non-empty, persists node records as JSON so a restart
	// resumes the same instance instead of opening another one.
	StatePath string
	// ReadinessTimeout bounds the pending->running and registration waits.
	ReadinessTimeout time.Duration
	// PollInterval is the delay between cloud describe calls.
	PollInterval time.Duration
	// Providers maps provider name to its adapter.
	Providers map[string]CloudProvider
	// WorkspaceURL is the Multica base URL rendered into first-boot config.
	WorkspaceURL string
	// GatewayURL is the model gateway the instance is allowed to reach.
	GatewayURL string
}
