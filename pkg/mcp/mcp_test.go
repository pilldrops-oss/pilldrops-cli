package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeBackend records the last call and replies with canned status/body.
type fakeBackend struct {
	method, path, ctype string
	body                []byte
	status              int
	resp                string
}

func (f *fakeBackend) fn() Backend {
	return func(method, path string, body []byte, ctype string) (int, []byte, error) {
		f.method, f.path, f.body, f.ctype = method, path, body, ctype
		return f.status, []byte(f.resp), nil
	}
}

func rpc(t *testing.T, be Backend, method string, params any) map[string]any {
	t.Helper()
	p, _ := json.Marshal(params)
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": json.RawMessage(p)})
	out := HandleMessage(msg, be)
	if out == nil {
		t.Fatalf("no response for %s", method)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("bad JSON response: %v", err)
	}
	return v
}

func TestInitializeEchoesSupportedVersion(t *testing.T) {
	v := rpc(t, (&fakeBackend{status: 200}).fn(), "initialize",
		map[string]any{"protocolVersion": "2025-03-26"})
	res := v["result"].(map[string]any)
	if res["protocolVersion"] != "2025-03-26" {
		t.Fatalf("protocolVersion = %v", res["protocolVersion"])
	}
	if res["instructions"] == "" {
		t.Fatal("initialize should carry usage instructions")
	}
}

func TestInitializeUnknownVersionOffersNewest(t *testing.T) {
	v := rpc(t, (&fakeBackend{status: 200}).fn(), "initialize",
		map[string]any{"protocolVersion": "1999-01-01"})
	if got := v["result"].(map[string]any)["protocolVersion"]; got != protocolVersions[0] {
		t.Fatalf("protocolVersion = %v, want %v", got, protocolVersions[0])
	}
}

func TestToolsListHasFullCatalog(t *testing.T) {
	v := rpc(t, (&fakeBackend{status: 200}).fn(), "tools/list", map[string]any{})
	tools := v["result"].(map[string]any)["tools"].([]any)
	// The exact count is pinned by tools.json (TestToolManifestMatchesCatalog),
	// which is also the parity contract with the platform's copy. A second hand-
	// typed number here would only be one more thing to forget to update.
	names := map[string]bool{}
	for _, tl := range tools {
		m := tl.(map[string]any)
		names[m["name"].(string)] = true
		if m["inputSchema"] == nil || m["description"] == "" {
			t.Fatalf("tool %v missing schema or description", m["name"])
		}
	}
	for _, must := range []string{"create_sandbox", "run_command", "get_logs", "destroy_sandbox", "write_file", "get_events", "wait_ready",
		"set_networks", "set_egress", "create_volume", "list_volumes", "delete_volume", "fork_sandbox", "save_checkpoint", "create_backup", "list_saved_states",
		"restore_sandbox", "delete_saved_state", "set_backup_schedule", "save_snapshot",
		"list_snapshots", "delete_snapshot", "standby_sandbox", "renew_sandbox", "list_files",
		"list_secrets", "put_secret", "delete_secret", "forward_port", "list_port_forwards",
		"remove_port_forward", "add_domain", "list_domains", "remove_domain"} {
		if !names[must] {
			t.Fatalf("missing tool %s", must)
		}
	}
}

func TestRunCommandBuildsExecRequest(t *testing.T) {
	fb := &fakeBackend{status: 200, resp: `{"stdout":"4\n","stderr":"","exit_code":0}`}
	v := rpc(t, fb.fn(), "tools/call", map[string]any{
		"name": "run_command",
		"arguments": map[string]any{
			"sandbox_id": "sb_x", "cmd": "python3 -c 'print(2+2)'", "timeout_seconds": 30,
		},
	})
	if fb.method != "POST" || fb.path != "/v1/sandboxes/sb_x/exec" {
		t.Fatalf("called %s %s", fb.method, fb.path)
	}
	var req map[string]any
	_ = json.Unmarshal(fb.body, &req)
	if req["cmd"] == "" || req["timeout"].(float64) != 30 {
		t.Fatalf("bad exec body: %s", fb.body)
	}
	res := v["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("unexpected tool error: %v", res)
	}
	if res["structuredContent"].(map[string]any)["exit_code"].(float64) != 0 {
		t.Fatalf("structuredContent missing: %v", res)
	}
}

func TestAuthErrorSurfacesActionableHint(t *testing.T) {
	fb := &fakeBackend{status: 401, resp: `{"error":"invalid api key"}`}
	v := rpc(t, fb.fn(), "tools/call", map[string]any{"name": "list_sandboxes"})
	res := v["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatal("401 should be a tool error")
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "Authorization: Bearer") || !strings.Contains(text, "pilldrops login") {
		t.Fatalf("hint not actionable: %q", text)
	}
}

func TestNotificationsProduceNoResponse(t *testing.T) {
	out := HandleMessage([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), (&fakeBackend{status: 200}).fn())
	if out != nil {
		t.Fatalf("notification got response: %s", out)
	}
}

func TestUnknownMethodIsRPCError(t *testing.T) {
	v := rpc(t, (&fakeBackend{status: 200}).fn(), "resources/list", map[string]any{})
	if v["error"] == nil {
		t.Fatal("want -32601 error")
	}
}

func TestHTTPServerRoundTrip(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sfc_k" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"no auth"}`))
			return
		}
		w.Write([]byte(`{"sandboxes":[]}`))
	})
	srv := &HTTPServer{API: api}

	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"list_sandboxes"}}`))
	req.Header.Set("Authorization", "Bearer sfc_k")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"sandboxes"`) {
		t.Fatalf("round trip failed: %d %s", rec.Code, rec.Body.String())
	}

	// GET has no stream to offer in stateless mode.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/mcp", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", rec.Code)
	}
}

// Discovery is open; tool calls without a token get the OAuth-triggering 401.
func TestHTTPAuthContract(t *testing.T) {
	srv := &HTTPServer{API: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})}

	// initialize + tools/list need no credentials.
	for _, msg := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest("POST", "/mcp", strings.NewReader(msg)))
		if rec.Code != 200 {
			t.Fatalf("unauthenticated discovery should pass, got %d for %s", rec.Code, msg)
		}
	}

	// tools/call without Authorization → 401 + WWW-Authenticate (OAuth trigger).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_sandboxes"}}`))
	req.Host = "pilldrops.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated tools/call = %d, want 401", rec.Code)
	}
	wa := rec.Header().Get("WWW-Authenticate")
	if !strings.Contains(wa, "https://pilldrops.com/.well-known/oauth-protected-resource") {
		t.Fatalf("WWW-Authenticate should point at resource metadata, got %q", wa)
	}
}

// destroy_sandbox with a missing/empty sandbox_id must fail validation loudly
// instead of dispatching DELETE /v1/sandboxes/ and surfacing a misleading 404.
func TestDestroySandboxRequiresSandboxID(t *testing.T) {
	fb := &fakeBackend{status: 200}
	v := rpc(t, fb.fn(), "tools/call", map[string]any{
		"name": "destroy_sandbox", "arguments": map[string]any{},
	})
	res := v["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("expected isError, got %v", res)
	}
	txt := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(txt, "sandbox_id") {
		t.Fatalf("error must name the missing sandbox_id arg: %s", txt)
	}
	if fb.path != "" {
		t.Fatalf("backend must not be dispatched on validation failure, got %s", fb.path)
	}
}
