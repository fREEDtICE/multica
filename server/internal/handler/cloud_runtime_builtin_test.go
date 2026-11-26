package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

			req := newRequest(http.MethodPost, "/api/cloud-runtime/nodes", map[string]any{
				"provider":      providerName,
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

// TestBuiltinCloudRuntimeAuthorizationFailure proves an insufficient
// authorization surfaces a 403 through the handler and bills no instance.
func TestBuiltinCloudRuntimeAuthorizationFailure(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "nodes.json")
	provisioner, handle := provision.NewFakeProvisionerHandle(t, provision.ProviderAWS, statePath)
	useCloudRuntimeProxy(t, provisioner)
	handle.SetAuthFailure(true)

	req := newRequest(http.MethodPost, "/api/cloud-runtime/nodes",
		map[string]any{"provider": provision.ProviderAWS, "name": "denied"})
	w := httptest.NewRecorder()
	testHandler.CreateCloudRuntimeNode(w, req)

	if w.Code != http.StatusForbidden || handle.Launches() != 0 {
		t.Fatalf("expected 403 with no launch, got %d launches=%d body=%s",
			w.Code, handle.Launches(), w.Body.String())
	}
}
