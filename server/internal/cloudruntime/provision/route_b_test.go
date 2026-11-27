package provision

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cloudruntime"
)

// doRaw drives the built-in driver through the same REST contract the handler
// proxies, which is the real entry point the web client uses.
func doRaw(t *testing.T, p *LocalProvisioner, method, path string, body any, query url.Values) (int, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	resp, err := p.Do(context.Background(), cloudruntime.Request{
		Method: method, Path: path, Query: query, UserID: fakeOwner, Body: raw,
	})
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatalf("decode %s response: %v body=%s", path, err, string(resp.Body))
	}
	return resp.StatusCode, out
}

// doRawBytes returns the raw JSON body for list endpoints whose Fleet
// contract is a bare array rather than a wrapped object.
func doRawBytes(t *testing.T, p *LocalProvisioner, method, path string, body any) (int, []byte) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	resp, err := p.Do(context.Background(), cloudruntime.Request{
		Method: method, Path: path, UserID: fakeOwner, Body: raw,
	})
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp.StatusCode, resp.Body
}

func newRouteBProvisioner(t *testing.T, providerName string) (*LocalProvisioner, *fakeCloud, string) {
	t.Helper()
	cloud := newFakeCloud(providerName)
	statePath := filepath.Join(t.TempDir(), "nodes.json")
	p := newTestProvisionerWithCloud(t, providerName, cloud, statePath)
	return p, cloud, statePath
}

func newTestProvisionerWithCloud(t *testing.T, providerName string, cloud *fakeCloud, statePath string) *LocalProvisioner {
	t.Helper()
	endpoint := cloud.start()
	t.Cleanup(endpoint.Close)
	return newProvisionerFromEndpoint(t, providerName, endpoint.URL, statePath)
}

// TestRouteBEmptyWorkspaceGate: from a blank workspace nothing can open before
// a cloud account is connected and verified.
func TestRouteBEmptyWorkspaceGate(t *testing.T) {
	for _, providerName := range []string{ProviderAWS, ProviderAliyun} {
		t.Run(providerName, func(t *testing.T) {
			p, cloud, _ := newRouteBProvisioner(t, providerName)

			status, body := doRaw(t, p, http.MethodPost, "/api/v1/nodes",
				map[string]any{"provider": providerName, "name": "first"}, nil)
			errObj, _ := body["error"].(map[string]any)
			if status != http.StatusForbidden || errObj["code"] != string(ErrNoAccount) || cloud.launches != 0 {
				t.Fatalf("blank workspace must reject provisioning with no_verified_account, got %d %v launches=%d",
					status, body, cloud.launches)
			}

			// The trust bundle is available before any account exists and
			// includes copyable trust + minimum-privilege policies.
			status, bundle := doRaw(t, p, http.MethodGet, "/api/v1/accounts/trust", nil,
				url.Values{"provider": {providerName}})
			if status != http.StatusOK || bundle["identity_present"] != true {
				t.Fatalf("trust bundle missing or identity absent: %d %v", status, bundle)
			}
			for _, key := range []string{"trust_policy", "permissions_policy", "console_guide_url", "role_arn_example"} {
				if text, _ := bundle[key].(string); text == "" {
					t.Fatalf("trust bundle missing %s: %v", key, bundle)
				}
			}

			// Regression: the node list must keep the managed-Fleet contract
			// (a bare JSON array), not a {nodes: [...]} wrapper, or the web
			// client parses an empty list.
			listStatus, rawList := doRawBytes(t, p, http.MethodGet, "/api/v1/nodes", nil)
			if listStatus != http.StatusOK {
				t.Fatalf("node list status = %d", listStatus)
			}
			var bareList []any
			if err := json.Unmarshal(rawList, &bareList); err != nil {
				t.Fatalf("node list must be a bare JSON array, got %s", string(rawList))
			}
		})
	}
}

// TestRouteBDeploymentIdentityMissing disables the whole flow when the
// self-hosted deployment itself has no cloud-recognised identity.
func TestRouteBDeploymentIdentityMissing(t *testing.T) {
	cloud := newFakeCloud(ProviderAWS)
	endpoint := cloud.start()
	t.Cleanup(endpoint.Close)
	// Live mode with no HELIOS_AWS_AK/SK in the environment: the adapter
	// constructs, but every signed path is blocked by the identity gate.
	adapter, err := NewAwsProvider(AWSConfig{Mode: "live", Region: "us-east-1", Endpoint: endpoint.URL})
	if err != nil {
		t.Fatalf("provider should construct without credentials: %v", err)
	}
	if adapter.IdentityReady() {
		t.Fatal("provider reports a ready identity with no credentials configured")
	}
	statePath := filepath.Join(t.TempDir(), "nodes.json")
	p, err := New(Config{
		Enabled: true, StatePath: statePath, ReadinessTimeout: time.Second, PollInterval: 5 * time.Millisecond,
		Providers: map[string]CloudProvider{ProviderAWS: adapter},
		Defaults:  map[string]ProviderDefaults{ProviderAWS: {Region: "us-east-1", ImageID: "ami", InstanceType: "t3.micro", SubnetID: "s"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	status, body := doRaw(t, p, http.MethodPost, "/api/v1/accounts/verify",
		map[string]string{"provider": ProviderAWS, "role_arn": FakeRoleARNs[ProviderAWS]}, nil)
	errObj, _ := body["error"].(map[string]any)
	if status != http.StatusConflict || errObj["code"] != string(ErrIdentityMissing) {
		t.Fatalf("verify must report deployment_identity_missing, got %d %v", status, body)
	}
	status, body = doRaw(t, p, http.MethodPost, "/api/v1/nodes",
		map[string]any{"provider": ProviderAWS, "name": "blocked"}, nil)
	errObj, _ = body["error"].(map[string]any)
	if status != http.StatusConflict || errObj["code"] != string(ErrIdentityMissing) || cloud.launches != 0 {
		t.Fatalf("provisioning must stay disabled without a deployment identity, got %d %v", status, body)
	}
}

// TestRouteBVerificationFailureThenRetry: a rejected role is recorded as failed
// with an actionable error; fixing the trust policy and re-verifying the same
// role ARN reuses the record instead of duplicating it.
func TestRouteBVerificationFailureThenRetry(t *testing.T) {
	for _, providerName := range []string{ProviderAWS, ProviderAliyun} {
		t.Run(providerName, func(t *testing.T) {
			p, cloud, _ := newRouteBProvisioner(t, providerName)
			cloud.roleReject = true

			status, failed := doRaw(t, p, http.MethodPost, "/api/v1/accounts/verify",
				map[string]string{"provider": providerName, "role_arn": FakeRoleARNs[providerName], "label": "acct"}, nil)
			if status != http.StatusForbidden || failed["status"] != AccountFailed {
				t.Fatalf("rejected role must fail verification, got %d %v", status, failed)
			}
			lastErr, _ := failed["last_error"].(map[string]any)
			if lastErr == nil || lastErr["Actionable"] == "" {
				t.Fatalf("failed account must carry an actionable hint: %v", failed)
			}
			status, list := doRaw(t, p, http.MethodGet, "/api/v1/accounts", nil, nil)
			accounts, _ := list["accounts"].([]any)
			if status != http.StatusOK || len(accounts) != 1 {
				t.Fatalf("failed attempt must still be listed once, got %v", list)
			}

			// An invalid ARN shape is rejected without touching the cloud.
			status, body := doRaw(t, p, http.MethodPost, "/api/v1/accounts/verify",
				map[string]string{"provider": providerName, "role_arn": "not-an-arn"}, nil)
			errObj, _ := body["error"].(map[string]any)
			if status != http.StatusBadRequest || errObj["code"] != string(ErrConfiguration) {
				t.Fatalf("invalid ARN must be a 400 configuration error, got %d %v", status, body)
			}

			cloud.roleReject = false
			status, verified := doRaw(t, p, http.MethodPost, "/api/v1/accounts/verify",
				map[string]string{"provider": providerName, "role_arn": FakeRoleARNs[providerName], "label": "acct"}, nil)
			if status != http.StatusOK || verified["status"] != AccountVerified {
				t.Fatalf("re-verification must succeed, got %d %v", status, verified)
			}
			identity, _ := verified["identity"].(map[string]any)
			if identity == nil || identity["account"] == "" {
				t.Fatalf("verified account must echo the checked cloud identity: %v", verified)
			}
			_, list = doRaw(t, p, http.MethodGet, "/api/v1/accounts", nil, nil)
			accounts, _ = list["accounts"].([]any)
			if len(accounts) != 1 {
				t.Fatalf("re-verification created a duplicate account: %v", accounts)
			}
		})
	}
}

// TestRouteBProvisionRunsOnShortLivedRoleCredentials covers both vendors:
// launch uses the assumed-role session key (never the deployment key), the
// node advances through provisioning → connecting → ready, the disk size is
// forwarded, and rendered user-data carries no long-lived secret.
func TestRouteBProvisionRunsOnShortLivedRoleCredentials(t *testing.T) {
	for _, providerName := range []string{ProviderAWS, ProviderAliyun} {
		t.Run(providerName, func(t *testing.T) {
			p, cloud, _ := newRouteBProvisioner(t, providerName)
			accountID := VerifyFakeAccount(t, p, providerName)
			deploymentAK := fakeAWSAccessKey
			if providerName == ProviderAliyun {
				deploymentAK = fakeAliyunAccessKey
			}
			sessionAK := fakeAWSSessionKey
			if providerName == ProviderAliyun {
				sessionAK = fakeAliyunSessionAK
			}

			status, node := doRaw(t, p, http.MethodPost, "/api/v1/nodes", map[string]any{
				"provider": providerName, "account_id": accountID,
				"name": "route-b", "disk_size_gb": 42,
			}, nil)
			if status != http.StatusCreated || node["status"] != StatusRunning || node["stage"] != StageReady {
				t.Fatalf("node must finish ready, got %d %v", status, node)
			}
			if cloud.launches != 1 || cloud.lastLaunchAK != sessionAK || cloud.lastLaunchAK == deploymentAK {
				t.Fatalf("launch must use the short-lived role session key, launches=%d ak=%q", cloud.launches, cloud.lastLaunchAK)
			}
			if cloud.lastDiskGB != "42" {
				t.Fatalf("disk size not forwarded to the cloud, got %q", cloud.lastDiskGB)
			}
			events := eventNames(node)
			for _, want := range []string{"lease_issued", "instance_launched", "connecting", "runtime_ready"} {
				if !contains(events, want) {
					t.Fatalf("node events missing %s: %v", want, events)
				}
			}
			instanceID, _ := node["instance_id"].(string)
			provider := p.cfg.Providers[providerName]
			stored, err := provider.GetUserData(context.Background(), instanceID)
			if err != nil {
				t.Fatalf("get user data: %v", err)
			}
			for _, secret := range []string{fakeAWSSecretKey, fakeAliyunSecretKey, fakeAWSSessionSecret, fakeAliyunSessionSK, fakeSessionToken} {
				if strings.Contains(stored, secret) {
					t.Fatalf("user-data leaked credential material %q", secret)
				}
			}
			lease, err := ParseLease(stored)
			if err != nil || time.Until(time.Unix(lease.ExpiresAt, 0)) > MaxLeaseSeconds*time.Second {
				t.Fatalf("embedded lease is not a short-lived lease: %+v err=%v", lease, err)
			}
		})
	}
}

// TestRouteBDefaultRuntime only allows a ready node to become the workspace
// default, and the choice survives a process restart.
func TestRouteBDefaultRuntime(t *testing.T) {
	p, _, statePath := newRouteBProvisioner(t, ProviderAWS)
	accountID := VerifyFakeAccount(t, p, ProviderAWS)
	_, node := doRaw(t, p, http.MethodPost, "/api/v1/nodes",
		map[string]any{"provider": ProviderAWS, "account_id": accountID, "name": "default-candidate"}, nil)
	nodeID, _ := node["id"].(string)

	status, body := doRaw(t, p, http.MethodPut, "/api/v1/default-runtime",
		map[string]string{"node_id": "crn-does-not-exist"}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("unknown node must not become the default, got %d %v", status, body)
	}
	if status, _ = doRaw(t, p, http.MethodPut, "/api/v1/default-runtime",
		map[string]string{"node_id": nodeID}, nil); status != http.StatusOK {
		t.Fatalf("ready node must be settable as default, got %d", status)
	}
	_, current := doRaw(t, p, http.MethodGet, "/api/v1/default-runtime", nil, nil)
	if current["node_id"] != nodeID {
		t.Fatalf("default runtime not persisted: %v", current)
	}

	// Rebuild from disk: accounts and the default runtime survive a restart.
	rebuilt, err := New(Config{
		Enabled: true, StatePath: statePath, ReadinessTimeout: time.Second, PollInterval: 5 * time.Millisecond,
		Providers: p.cfg.Providers, Defaults: p.cfg.Defaults,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rebuilt.accounts.VerifiedAccounts(ProviderAWS)) != 1 {
		t.Fatal("verified account did not survive restart")
	}
	if rebuilt.accounts.DefaultNode() != nodeID {
		t.Fatal("default runtime did not survive restart")
	}
}

// TestRouteBCancelReclaimsInFlightLaunch proves cancelling a stalled launch
// terminates the half-open instance and records the reclamation result.
func TestRouteBCancelReclaimsInFlightLaunch(t *testing.T) {
	p, cloud, _ := newRouteBProvisioner(t, ProviderAWS)
	accountID := VerifyFakeAccount(t, p, ProviderAWS)
	p.cfg.ReadinessTimeout = 60 * time.Millisecond
	cloud.stall = true

	status, _ := doRaw(t, p, http.MethodPost, "/api/v1/nodes",
		map[string]any{"provider": ProviderAWS, "account_id": accountID, "name": "stalled"}, nil)
	if status != http.StatusGatewayTimeout || cloud.launches != 1 || len(cloud.activeIDs()) != 1 {
		t.Fatalf("expected a retained half-open instance after timeout, got %d launches=%d active=%v",
			status, cloud.launches, cloud.activeIDs())
	}
	failedID := lookupFailedNodeID(t, p)

	status, cancelled := doRaw(t, p, http.MethodPost, "/api/v1/nodes/cancel",
		map[string]string{"id": failedID}, nil)
	if status != http.StatusOK || cancelled["status"] != StatusTerminated || len(cloud.activeIDs()) != 0 {
		t.Fatalf("cancel must reclaim the instance, got %d %v active=%v", status, cancelled, cloud.activeIDs())
	}
	if !contains(eventNames(cancelled), "cancel_reclaimed") {
		t.Fatalf("cancel result not recorded in events: %v", cancelled)
	}
}

// TestRouteBCleanupAfterAuthorizationLapse: when termination fails because the
// role authorization lapsed, the node enters a visible cleanup queue; after the
// authorization is restored, retry reclaims it and clears the queue.
func TestRouteBCleanupAfterAuthorizationLapse(t *testing.T) {
	p, cloud, _ := newRouteBProvisioner(t, ProviderAWS)
	accountID := VerifyFakeAccount(t, p, ProviderAWS)
	_, node := doRaw(t, p, http.MethodPost, "/api/v1/nodes",
		map[string]any{"provider": ProviderAWS, "account_id": accountID, "name": "lapsed"}, nil)
	nodeID, _ := node["id"].(string)

	cloud.authFailure = true
	status, failed := doRaw(t, p, http.MethodDelete, "/api/v1/nodes",
		map[string]string{"id": nodeID}, nil)
	if status != http.StatusConflict || failed["status"] != StatusCleanupRequired {
		t.Fatalf("lapsed authorization must park the node in cleanup_required, got %d %v", status, failed)
	}
	_, queue := doRaw(t, p, http.MethodGet, "/api/v1/cleanup", nil, nil)
	pending, _ := queue["nodes"].([]any)
	if len(pending) != 1 {
		t.Fatalf("cleanup queue must list the stranded resource: %v", queue)
	}

	// A retry while still broken stays in the queue with no panic and no loss.
	if status, _ = doRaw(t, p, http.MethodPost, "/api/v1/cleanup/retry",
		map[string]string{"id": nodeID}, nil); status != http.StatusConflict {
		t.Fatalf("retry before authorization is restored must remain blocked, got %d", status)
	}

	cloud.authFailure = false
	status, recovered := doRaw(t, p, http.MethodPost, "/api/v1/cleanup/retry",
		map[string]string{"id": nodeID}, nil)
	if status != http.StatusOK || recovered["status"] != StatusTerminated || len(cloud.activeIDs()) != 0 {
		t.Fatalf("cleanup retry must reclaim the instance, got %d %v active=%v",
			status, recovered, cloud.activeIDs())
	}
	_, queue = doRaw(t, p, http.MethodGet, "/api/v1/cleanup", nil, nil)
	if pending, _ = queue["nodes"].([]any); len(pending) != 0 {
		t.Fatalf("cleanup queue must be empty after recovery: %v", queue)
	}
}

// TestRouteBAccountDisconnectProtection refuses to disconnect an account that
// still backs an active runtime, and allows it once the runtime is terminated.
func TestRouteBAccountDisconnectProtection(t *testing.T) {
	p, cloud, _ := newRouteBProvisioner(t, ProviderAWS)
	accountID := VerifyFakeAccount(t, p, ProviderAWS)
	_, node := doRaw(t, p, http.MethodPost, "/api/v1/nodes",
		map[string]any{"provider": ProviderAWS, "account_id": accountID, "name": "guarded"}, nil)
	nodeID, _ := node["id"].(string)

	status, body := doRaw(t, p, http.MethodDelete, "/api/v1/accounts",
		map[string]string{"id": accountID}, nil)
	if status != http.StatusConflict {
		t.Fatalf("disconnecting an account with an active runtime must be refused, got %d %v", status, body)
	}
	if status, _ = doRaw(t, p, http.MethodPost, "/api/v1/nodes/stop",
		map[string]string{"id": nodeID}, nil); status != http.StatusOK {
		t.Fatalf("stop failed: %d", status)
	}
	status, body = doRaw(t, p, http.MethodDelete, "/api/v1/accounts",
		map[string]string{"id": accountID}, nil)
	if status != http.StatusOK {
		t.Fatalf("account disconnect must succeed after termination, got %d %v", status, body)
	}
	if len(cloud.activeIDs()) != 0 {
		t.Fatalf("termination left an active instance: %v", cloud.activeIDs())
	}
}

func eventNames(node map[string]any) []string {
	rawEvents, _ := node["events"].([]any)
	names := make([]string, 0, len(rawEvents))
	for _, raw := range rawEvents {
		event, _ := raw.(map[string]any)
		if name, _ := event["event"].(string); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func lookupFailedNodeID(t *testing.T, p *LocalProvisioner) string {
	t.Helper()
	status, raw := doRawBytes(t, p, http.MethodGet, "/api/v1/nodes", nil)
	if status != http.StatusOK {
		t.Fatalf("node list status = %d body=%s", status, string(raw))
	}
	// The node list follows the Fleet contract: a bare JSON array.
	var nodes []map[string]any
	if err := json.Unmarshal(raw, &nodes); err != nil {
		t.Fatalf("expected a bare node array, got %s", string(raw))
	}
	if len(nodes) != 1 {
		t.Fatalf("expected one node, got %v", nodes)
	}
	id, _ := nodes[0]["id"].(string)
	return id
}
