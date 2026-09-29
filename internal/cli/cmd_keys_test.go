package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The routes and shapes are the platform's: GET|POST /v1/keys and
// DELETE /v1/keys/{id} (internal/httpapi/opsroutes.go), POST taking exactly one
// field — name (internal/httpapi/admin.go) — and returning the plaintext once.
func TestKeysMintUseRevoke(t *testing.T) {
	var minted string
	revoked := map[string]bool{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		// Anything minted and not revoked is a working credential.
		if tok != "sf_root" && (tok == "" || tok != minted || revoked[tok]) {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"invalid API key","code":"unauthorized"}`))
			return
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/keys":
			body, _ := io.ReadAll(r.Body)
			var in map[string]any
			_ = json.Unmarshal(body, &in)
			if in["name"] != "ci" {
				t.Errorf("POST /v1/keys body = %s, want the one field the handler decodes", body)
			}
			minted = "sf_minted"
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":"key_abcd","name":"ci","key":"sf_minted","masked_key":"sf_live_****abcd"}`))
		case r.Method == "GET" && r.URL.Path == "/v1/keys":
			_, _ = w.Write([]byte(`[{"id":"key_abcd","name":"ci","masked_key":"sf_live_****abcd","last_used":0}]`))
		case r.Method == "DELETE" && r.URL.Path == "/v1/keys/key_abcd":
			revoked[minted] = true
			_, _ = w.Write([]byte(`{"status":"revoked"}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"not found","code":"not_found"}`))
		}
	}))
	defer srv.Close()

	t.Setenv("PILLDROPS_API_KEY", "sf_root")
	code, out, errS := runCLI2(t, srv.URL, "--json", "keys", "create", "ci")
	if code != ExitOK {
		t.Fatalf("keys create = %d: %s", code, errS)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("keys create --json: %v\n%s", err, out)
	}
	key, id := str(created["key"]), str(created["id"])
	if key == "" || id == "" {
		t.Fatalf("create returned no usable key: %v", created)
	}

	// The minted key works.
	t.Setenv("PILLDROPS_API_KEY", key)
	if code, out, errS := runCLI2(t, srv.URL, "--json", "keys", "list"); code != ExitOK {
		t.Fatalf("list with the minted key = %d: %s%s", code, out, errS)
	}

	// Revoke it, and the next call is 401 — exit 3, not 1.
	if code, _, errS := runCLI2(t, srv.URL, "--json", "keys", "rm", id); code != ExitOK {
		t.Fatalf("keys rm = %d: %s", code, errS)
	}
	code, _, errS = runCLI2(t, srv.URL, "--json", "keys", "list")
	if code != ExitAuth {
		t.Fatalf("call after revoke = %d, want %d (401)", code, ExitAuth)
	}
	var failure map[string]any
	if err := json.Unmarshal([]byte(errS), &failure); err != nil {
		t.Fatalf("stderr not JSON: %v\n%s", err, errS)
	}
	if failure["code"] != "unauthorized" || failure["status"] != float64(401) {
		t.Fatalf("revoked-key failure = %v, want the platform's code and status", failure)
	}
}
