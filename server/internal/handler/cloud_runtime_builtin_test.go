package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/cloudruntime/provision"
)

// TestBuiltinCloudRuntimeProvisionThroughHandler drives the real
// CreateCloudRuntimeNode entry with the self-hosted built-in provisioner backed
// by an in-process fake cloud, proving the handler wiring and the node REST
// contract work without the managed Fleet service.
func TestBuiltinCloudRuntimeProvisionThroughHandler(t *testing.T) {
	for _, providerName := range []string{provision.ProviderAWS, provision.ProviderAliyun} {
		t.Run(providerName, func(t *testing.T) {
			statePath := filepath.Join(t.TempDir(), "nodes.json")
			provisioner, handle := provision.NewFakeProvisionerHandle(t, providerName, statePath)
			useCloudRuntimeProxy(t, provisioner)

			// Route B gates provisioning on a connected, verified account.
			accountID := provision.VerifyFakeAccount(t, provisioner, providerName)

			req := newRequest(http.MethodPost, "/api/cloud-runtime/nodes", map[string]any{
				"provider":      providerName,
				"account_id":    accountID,
				"name":          "builtin-node",
				"instance_type": "t3.micro",
			})
			w := httptest.NewRecorder()
			testHandler.CreateCloudRuntimeNode(w, req)

			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			var node map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &node); err != nil {
				t.Fatalf("node response is not JSON: %v body=%s", err, w.Body.String())
			}
			if node["status"] != provision.StatusRunning || node["provider"] != providerName {
				t.Fatalf("unexpected node: %v", node)
			}
			if node["instance_id"] == "" || handle.Launches() != 1 {
				t.Fatalf("expected exactly one cloud launch, launches=%d node=%v", handle.Launches(), node)
			}
			metadata, _ := node["metadata"].(map[string]any)
			if metadata == nil || metadata["instance_id"] != node["instance_id"] {
				t.Fatalf("node metadata must prove the execution instance: %v", metadata)
			}

			// Reclaim through the same handler entry leaves no active cloud resource.
			instanceID, _ := node["instance_id"].(string)
			deleteReq := newRequest(http.MethodDelete, "/api/cloud-runtime/nodes",
				map[string]any{"instance_id": instanceID})
			deleteW := httptest.NewRecorder()
			testHandler.DeleteCloudRuntimeNode(deleteW, deleteReq)
			if deleteW.Code != http.StatusOK || len(handle.ActiveIDs()) != 0 {
				t.Fatalf("delete did not reclaim the instance: status=%d active=%v",
					deleteW.Code, handle.ActiveIDs())
			}
		})
	}
}

// TestBuiltinCloudRuntimeRouteBFlow drives the full Route B product flow
// through the real HTTP handlers: connect and verify a role-based account,
// open a runtime on short-lived role credentials, set it as the workspace
// default, then reclaim it. Both vendors share the same contract.
func TestBuiltinCloudRuntimeRouteBFlow(t *testing.T) {
	for _, providerName := range []string{provision.ProviderAWS, provision.ProviderAliyun} {
		t.Run(providerName, func(t *testing.T) {
			statePath := filepath.Join(t.TempDir(), "nodes.json")
			provisioner, handle := provision.NewFakeProvisionerHandle(t, providerName, statePath)
			useCloudRuntimeProxy(t, provisioner)

			// Blank workspace: provisioning without a verified account is refused.
			blockedReq := newRequest(http.MethodPost, "/api/cloud-runtime/nodes",
				map[string]any{"provider": providerName, "name": "blocked"})
			blockedW := httptest.NewRecorder()
			testHandler.CreateCloudRuntimeNode(blockedW, blockedReq)
			if blockedW.Code != http.StatusForbidden {
				t.Fatalf("expected 403 without a verified account, got %d body=%s", blockedW.Code, blockedW.Body.String())
			}

			// The trust bundle is rendered before any account exists.
			trustReq := newRequest(http.MethodGet, "/api/cloud-runtime/accounts/trust?provider="+providerName, nil)
			trustW := httptest.NewRecorder()
			testHandler.GetCloudAccountTrust(trustW, trustReq)
			if trustW.Code != http.StatusOK || !strings.Contains(trustW.Body.String(), "trust_policy") {
				t.Fatalf("trust bundle not served: %d %s", trustW.Code, trustW.Body.String())
			}

			// Connect the account with a role ARN; verification mints short-lived
			// credentials and returns the checked identity.
			verifyReq := newRequest(http.MethodPost, "/api/cloud-runtime/accounts/verify", map[string]string{
				"provider": providerName,
				"role_arn": provision.FakeRoleARNs[providerName],
				"label":    "test-account",
			})
			verifyW := httptest.NewRecorder()
			testHandler.VerifyCloudAccount(verifyW, verifyReq)
			if verifyW.Code != http.StatusOK {
				t.Fatalf("verify status = %d body = %s", verifyW.Code, verifyW.Body.String())
			}
			var account map[string]any
			json.Unmarshal(verifyW.Body.Bytes(), &account)
			accountID, _ := account["id"].(string)
			if account["status"] != "verified" || accountID == "" {
				t.Fatalf("account not verified: %v", account)
			}

			// Open the runtime under the verified account.
			createReq := newRequest(http.MethodPost, "/api/cloud-runtime/nodes", map[string]any{
				"provider": providerName, "account_id": accountID,
				"name": "route-b-handler", "disk_size_gb": 30,
			})
			createW := httptest.NewRecorder()
			testHandler.CreateCloudRuntimeNode(createW, createReq)
			if createW.Code != http.StatusCreated {
				t.Fatalf("create status = %d body = %s", createW.Code, createW.Body.String())
			}
			var node map[string]any
			json.Unmarshal(createW.Body.Bytes(), &node)
			if node["status"] != provision.StatusRunning || node["stage"] != provision.StageReady {
				t.Fatalf("node did not reach ready: %v", node)
			}
			if handle.Launches() != 1 {
				t.Fatalf("expected exactly one launch, got %d", handle.Launches())
			}
			nodeID, _ := node["id"].(string)

			// A ready runtime can become the workspace default.
			defaultReq := newRequest(http.MethodPut, "/api/cloud-runtime/default-runtime",
				map[string]string{"node_id": nodeID})
			defaultW := httptest.NewRecorder()
			testHandler.SetDefaultCloudRuntime(defaultW, defaultReq)
			if defaultW.Code != http.StatusOK {
				t.Fatalf("set default status = %d body = %s", defaultW.Code, defaultW.Body.String())
			}

			// Reclaim through the delete entry and confirm no active cloud resource.
			instanceID, _ := node["instance_id"].(string)
			deleteReq := newRequest(http.MethodDelete, "/api/cloud-runtime/nodes",
				map[string]string{"instance_id": instanceID})
			deleteW := httptest.NewRecorder()
			testHandler.DeleteCloudRuntimeNode(deleteW, deleteReq)
			if deleteW.Code != http.StatusOK || len(handle.ActiveIDs()) != 0 {
				t.Fatalf("delete did not reclaim the instance: %d active=%v", deleteW.Code, handle.ActiveIDs())
			}
		})
	}
}

// TestBuiltinCloudRuntimeAuthorizationFailure proves an insufficient
// authorization surfaces a 403 through the handler and bills no instance.
func TestBuiltinCloudRuntimeAuthorizationFailure(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "nodes.json")
	provisioner, handle := provision.NewFakeProvisionerHandle(t, provision.ProviderAWS, statePath)
	useCloudRuntimeProxy(t, provisioner)
	accountID := provision.VerifyFakeAccount(t, provisioner, provision.ProviderAWS)
	handle.SetAuthFailure(true)

	req := newRequest(http.MethodPost, "/api/cloud-runtime/nodes",
		map[string]any{"provider": provision.ProviderAWS, "name": "denied", "account_id": accountID})
	w := httptest.NewRecorder()
	testHandler.CreateCloudRuntimeNode(w, req)

	if w.Code != http.StatusForbidden || handle.Launches() != 0 {
		t.Fatalf("expected 403 with no launch, got %d launches=%d body=%s",
			w.Code, handle.Launches(), w.Body.String())
	}
}
