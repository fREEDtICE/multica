package provision

import (
	"context"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cloudruntime"
)

const (
	fakeOwner = "user-helios"
)

func newTestProvisioner(t *testing.T, providerName string, cloud *fakeCloud) (*LocalProvisioner, CloudProvider) {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "nodes.json")
	p := NewFakeProvisioner(t, providerName, cloud, statePath)
	return p, p.cfg.Providers[providerName]
}

func doCreate(t *testing.T, p *LocalProvisioner, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := p.Do(context.Background(), cloudruntime.Request{
		Method: "POST", Path: "/api/v1/nodes", UserID: fakeOwner, Body: raw,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	var out map[string]any
	json.Unmarshal(resp.Body, &out)
	return resp.StatusCode, out
}

// TestAWSSigV4OfficialVectors anchors signing on the AWS-published test suite
// shipped in botocore (get-vanilla and get-vanilla-with-session-token).
func TestAWSSigV4OfficialVectors(t *testing.T) {
	headers, err := AWSSignRequest("GET", "https://example.amazonaws.com/", url.Values{},
		map[string]string{"X-Amz-Date": "20150830T123600Z"}, nil,
		"AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "",
		"us-east-1", "service", "20150830T123600Z")
	if err != nil {
		t.Fatal(err)
	}
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if headers["Authorization"] != want {
		t.Fatalf("vanilla vector mismatch\n got %s\nwant %s", headers["Authorization"], want)
	}

	withToken, err := AWSSignRequest("GET", "https://example.amazonaws.com/", url.Values{},
		map[string]string{"X-Amz-Date": "20150830T123600Z"}, nil,
		"AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		"6e86291e8372ff2a2260956d9b8aae1d763fbf315fa00fa31553b73ebf194267",
		"us-east-1", "service", "20150830T123600Z")
	if err != nil {
		t.Fatal(err)
	}
	wantToken := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date;x-amz-security-token, Signature=07ec1639c89043aa0e3e2de82b96708f198cceab042d4a97044c66dd9f74e7f8"
	if withToken["Authorization"] != wantToken {
		t.Fatalf("session-token vector mismatch\n got %s\nwant %s", withToken["Authorization"], wantToken)
	}
}

func TestProvisionHappyPathBothProviders(t *testing.T) {
	for _, providerName := range []string{ProviderAWS, ProviderAliyun} {
		t.Run(providerName, func(t *testing.T) {
			cloud := newFakeCloud(providerName)
			p, provider := newTestProvisioner(t, providerName, cloud)

			status, node := doCreate(t, p, map[string]any{"provider": providerName, "name": "main"})
			if status != 201 || node["status"] != StatusRunning {
				t.Fatalf("expected running node, status=%d body=%v", status, node)
			}
			instanceID, _ := node["instance_id"].(string)
			if instanceID == "" {
				t.Fatal("node has no cloud instance identity")
			}

			// The exact cloud-stored user-data carries only a short-lived lease.
			stored, err := provider.GetUserData(context.Background(), instanceID)
			if err != nil {
				t.Fatalf("get user data: %v", err)
			}
			if strings.Contains(stored, fakeAWSSecretKey) || strings.Contains(stored, fakeAliyunSecretKey) {
				t.Fatal("user-data leaked a long-lived cloud secret")
			}
			lease, err := ParseLease(stored)
			if err != nil {
				t.Fatalf("parse lease: %v", err)
			}
			if time.Until(time.Unix(lease.ExpiresAt, 0)) > MaxLeaseSeconds*time.Second {
				t.Fatalf("lease TTL exceeds cap: %v", time.Until(time.Unix(lease.ExpiresAt, 0)))
			}
			cfg, err := ParseRuntimeConfig(stored)
			if err != nil || cfg.RegisterPath != RegisterPath || cfg.RuntimeMode != "cloud" {
				t.Fatalf("runtime config not bound to the formal register entry: %+v err=%v", cfg, err)
			}
			if providerName == ProviderAWS {
				console, _ := node["metadata"].(map[string]any)["console_output"].(string)
				if !strings.Contains(console, "registered through /api/daemon/register") {
					t.Fatalf("console output lacks registration marker: %q", console)
				}
			}

			// A repeated request with the same identity is idempotent.
			_, again := doCreate(t, p, map[string]any{"provider": providerName, "name": "main"})
			if again["instance_id"] != instanceID || cloud.launches != 1 {
				t.Fatalf("retry opened a second instance: launches=%d node=%v", cloud.launches, again)
			}

			// Deletion terminates the cloud instance and leaves no active resource.
			raw, _ := json.Marshal(map[string]string{"instance_id": instanceID})
			resp, err := p.Do(context.Background(), cloudruntime.Request{
				Method: "DELETE", Path: "/api/v1/nodes", UserID: fakeOwner, Body: raw,
			})
			if err != nil || resp.StatusCode != 200 || len(cloud.activeIDs()) != 0 {
				t.Fatalf("delete did not reclaim the instance: status=%d active=%v err=%v",
					resp.StatusCode, cloud.activeIDs(), err)
			}
		})
	}
}

func TestAuthorizationFailureBillsNothingThenRecovers(t *testing.T) {
	for _, providerName := range []string{ProviderAWS, ProviderAliyun} {
		t.Run(providerName, func(t *testing.T) {
			cloud := newFakeCloud(providerName)
			p, _ := newTestProvisioner(t, providerName, cloud)
			cloud.authFailure = true

			status, body := doCreate(t, p, map[string]any{"provider": providerName, "name": "auth"})
			errObj, _ := body["error"].(map[string]any)
			if status != 403 || errObj["code"] != string(ErrAuthorization) || cloud.launches != 0 {
				t.Fatalf("expected a 403 authorization failure with no launch, got %d %v launches=%d",
					status, body, cloud.launches)
			}

			cloud.authFailure = false
			status, node := doCreate(t, p, map[string]any{"provider": providerName, "name": "auth"})
			if status != 201 || node["status"] != StatusRunning || cloud.launches != 1 {
				t.Fatalf("retry after authorization did not recover: status=%d node=%v launches=%d",
					status, node, cloud.launches)
			}
		})
	}
}

func TestProvisionTimeoutKeepsInstanceAndReclaims(t *testing.T) {
	cloud := newFakeCloud(ProviderAWS)
	p, _ := newTestProvisioner(t, ProviderAWS, cloud)
	p.cfg.ReadinessTimeout = 60 * time.Millisecond
	cloud.stall = true

	status, body := doCreate(t, p, map[string]any{"provider": ProviderAWS, "name": "stall"})
	errObj, _ := body["error"].(map[string]any)
	if status != 504 || errObj["code"] != string(ErrTimeout) {
		t.Fatalf("expected provision_timeout 504, got %d %v", status, body)
	}
	if cloud.launches != 1 || len(cloud.activeIDs()) != 1 {
		t.Fatalf("timed-out launch must retain the half-open instance: launches=%d active=%v",
			cloud.launches, cloud.activeIDs())
	}

	// Cancellation must release the retained instance so nothing is orphaned.
	instanceID := cloud.activeIDs()[0]
	raw, _ := json.Marshal(map[string]string{"instance_id": instanceID})
	resp, err := p.Do(context.Background(), cloudruntime.Request{
		Method: "POST", Path: "/api/v1/nodes/stop", UserID: fakeOwner, Body: raw,
	})
	if err != nil || resp.StatusCode != 200 || len(cloud.activeIDs()) != 0 {
		t.Fatalf("stop did not reclaim the stalled instance: status=%d active=%v err=%v",
			resp.StatusCode, cloud.activeIDs(), err)
	}
}

func TestTamperedSignatureRejected(t *testing.T) {
	cloud := newFakeCloud(ProviderAWS)
	p, _ := newTestProvisioner(t, ProviderAWS, cloud)
	aws := p.cfg.Providers[ProviderAWS].(*AwsProvider)
	aws.secretKey = fakeAWSSecretKey + "tampered"

	_, err := aws.Authorize(context.Background())
	driverErr, ok := AsDriverError(err)
	if !ok || driverErr.Code != ErrAuthorization {
		t.Fatalf("expected authorization_failed for a forged signature, got %v", err)
	}
}

func TestStatePersistsAcrossRestart(t *testing.T) {
	cloud := newFakeCloud(ProviderAWS)
	statePath := filepath.Join(t.TempDir(), "nodes.json")
	endpoint := cloud.start()
	t.Cleanup(endpoint.Close)
	adapter, err := NewAwsProvider(AWSConfig{Mode: "fake", Region: "us-east-1", Endpoint: endpoint.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Enabled: true, StatePath: statePath, ReadinessTimeout: time.Second, PollInterval: 5 * time.Millisecond,
		WorkspaceURL: endpoint.URL, Providers: map[string]CloudProvider{ProviderAWS: adapter},
		Defaults: map[string]ProviderDefaults{ProviderAWS: {
			Region: "us-east-1", ImageID: "ami", InstanceType: "t3.micro", SubnetID: "s",
			SecurityGroupIDs: []string{"g"}, InstanceProfile: "role",
		}},
	}
	first, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, node := doCreate(t, first, map[string]any{"provider": ProviderAWS, "name": "persist"})
	instanceID := node["instance_id"].(string)

	// Rebuild the provisioner from disk; the node and its cloud handle survive.
	second, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	nodes := second.store.List()
	if len(nodes) != 1 || nodes[0].InstanceID != instanceID || nodes[0].Status != StatusRunning {
		t.Fatalf("restart did not restore the node: %+v", nodes)
	}
}
