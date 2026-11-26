package provision

import (
	"testing"
	"time"
)

// FakeCloudHandle exposes fault and observation controls for an in-process
// fake cloud to tests in other packages without exporting the cloud itself.
type FakeCloudHandle struct {
	SetAuthFailure func(bool)
	SetStall       func(bool)
	ActiveIDs      func() []string
	Launches       func() int
}

// NewFakeProvisioner wires a LocalProvisioner against an in-process fake cloud
// for the given provider. It is intended for handler-level integration tests
// that exercise the real node REST contract without a real cloud account.
func NewFakeProvisioner(t *testing.T, providerName string, cloud *fakeCloud, statePath string) *LocalProvisioner {
	t.Helper()
	endpoint := cloud.start()
	t.Cleanup(endpoint.Close)
	return newProvisionerFromEndpoint(t, providerName, endpoint.URL, statePath)
}

// NewFakeProvisionerHandle builds a provisioner on a fresh fake cloud and
// returns the controls tests need to inject faults and observe resources.
func NewFakeProvisionerHandle(t *testing.T, providerName, statePath string) (*LocalProvisioner, FakeCloudHandle) {
	t.Helper()
	cloud := newFakeCloud(providerName)
	endpoint := cloud.start()
	t.Cleanup(endpoint.Close)
	p := newProvisionerFromEndpoint(t, providerName, endpoint.URL, statePath)
	handle := FakeCloudHandle{
		SetAuthFailure: func(v bool) { cloud.authFailure = v },
		SetStall:       func(v bool) { cloud.stall = v },
		ActiveIDs:      func() []string { return cloud.activeIDs() },
		Launches:       func() int { return cloud.launches },
	}
	return p, handle
}

func newProvisionerFromEndpoint(t *testing.T, providerName, endpointURL, statePath string) *LocalProvisioner {
	t.Helper()

	defaults := ProviderDefaults{
		ImageID: "ami-helios", InstanceType: "t3.micro",
		SubnetID: "subnet-helios", SecurityGroupIDs: []string{"sg-helios"}, InstanceProfile: "helios-role",
	}
	var provider CloudProvider
	switch providerName {
	case ProviderAWS:
		defaults.Region = "us-east-1"
		adapter, err := NewAwsProvider(AWSConfig{Mode: "fake", Region: "us-east-1", Endpoint: endpointURL})
		if err != nil {
			t.Fatalf("aws provider: %v", err)
		}
		provider = adapter
	case ProviderAliyun:
		defaults.Region = "cn-hangzhou"
		defaults.ImageID = "aliyun-helios"
		defaults.InstanceType = "ecs.t6-c1m1.large"
		defaults.SubnetID = "vsw-helios"
		adapter, err := NewAliyunProvider(AliyunConfig{Mode: "fake", Region: "cn-hangzhou", Endpoint: endpointURL})
		if err != nil {
			t.Fatalf("aliyun provider: %v", err)
		}
		provider = adapter
	default:
		t.Fatalf("unknown provider: %s", providerName)
	}

	p, err := New(Config{
		Enabled: true, StatePath: statePath,
		ReadinessTimeout: 2 * time.Second, PollInterval: 5 * time.Millisecond,
		WorkspaceURL: endpointURL, GatewayURL: endpointURL + "/gateway",
		Providers: map[string]CloudProvider{providerName: provider},
		Defaults:  map[string]ProviderDefaults{providerName: defaults},
	})
	if err != nil {
		t.Fatalf("new provisioner: %v", err)
	}
	return p
}
