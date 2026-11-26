package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/internal/cloudruntime"
)

// nodeMux mirrors the handler's proxy contract at the transport boundary: each
// route calls the same cloudRuntimeProxy.Do method the real Multica handler
// uses and writes the response unchanged. It proves the built-in provisioner
// is a drop-in backend over real HTTP without needing the database-gated
// handler test suite.
func nodeMux(p *LocalProvisioner) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		resp, err := p.Do(r.Context(), cloudruntime.Request{
			Method: r.Method, Path: "/api/v1/nodes", UserID: r.Header.Get("X-User-ID"), Body: body,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(resp.Body)
	})
	return mux
}

func TestNodeContractOverHTTP(t *testing.T) {
	for _, providerName := range []string{ProviderAWS, ProviderAliyun} {
		t.Run(providerName, func(t *testing.T) {
			statePath := filepath.Join(t.TempDir(), "nodes.json")
			p, handle := NewFakeProvisionerHandle(t, providerName, statePath)
			server := httptest.NewServer(nodeMux(p))
			defer server.Close()

			post := func(payload map[string]any) (int, map[string]any) {
				raw, _ := json.Marshal(payload)
				req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
					server.URL+"/api/v1/nodes", bytes.NewReader(raw))
				req.Header.Set("X-User-ID", fakeOwner)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("http post: %v", err)
				}
				defer resp.Body.Close()
				var out map[string]any
				json.NewDecoder(resp.Body).Decode(&out)
				return resp.StatusCode, out
			}

			status, node := post(map[string]any{"provider": providerName, "name": "http-node"})
			if status != http.StatusCreated || node["status"] != StatusRunning {
				t.Fatalf("expected 201 running, got %d %v", status, node)
			}
			instanceID, _ := node["instance_id"].(string)
			if instanceID == "" || handle.Launches() != 1 {
				t.Fatalf("expected one launch, got launches=%d node=%v", handle.Launches(), node)
			}

			// Idempotent retry over real HTTP returns the same instance.
			if _, again := post(map[string]any{"provider": providerName, "name": "http-node"}); again["instance_id"] != instanceID {
				t.Fatalf("retry changed the instance: %v", again)
			}
			if handle.Launches() != 1 {
				t.Fatalf("retry opened a second instance: %d", handle.Launches())
			}

			// Authorization fault maps to HTTP 403 and bills nothing new.
			handle.SetAuthFailure(true)
			if status, body := post(map[string]any{"provider": providerName, "name": "denied-http"}); status != http.StatusForbidden {
				t.Fatalf("expected 403 on insufficient authorization, got %d %v", status, body)
			}
			handle.SetAuthFailure(false)
		})
	}
}
