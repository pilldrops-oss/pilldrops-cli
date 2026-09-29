package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops-cli/pkg/mcp"
)

func TestAICatalogPrintsWorkflowsAndContract(t *testing.T) {
	code, out, _ := runCLI(t, "", "ai")
	if code != ExitOK {
		t.Fatalf("code = %d, want 0", code)
	}
	for _, must := range []string{
		"agent catalog", "exit_code", "sandbox create", "$PORT",
		"pilldrops history", "Error → remediation", "--json",
	} {
		if !strings.Contains(out, must) {
			t.Errorf("catalog missing %q", must)
		}
	}
}

// The catalog is generated from the command table, so these two must hold in
// both directions: a verb that exists is advertised, and a verb that is
// advertised runs. A catalog that still passes after a verb is removed is the
// failure this test exists to prevent.
func TestCatalogAndDispatchAgree(t *testing.T) {
	_, catalog, _ := runCLI(t, "", "ai")

	for _, c := range commandTable() {
		if !strings.Contains(catalog, c.name) {
			t.Errorf("command %q is dispatchable but absent from the `ai` catalog", c.name)
		}
		if c.run == nil {
			t.Errorf("command %q has no implementation", c.name)
		}
		// Stage 0.3: every entry carries one worked example, executed
		// against a live control plane by TestCatalogExamplesRun.
		if c.example == "" {
			t.Errorf("command %q has no worked example", c.name)
		} else if !strings.HasPrefix(c.example, "pilldrops ") {
			t.Errorf("command %q example is not a pilldrops invocation: %q", c.name, c.example)
		}
	}

	paths := commandPaths()
	if len(paths) < 20 {
		t.Fatalf("only %d command paths — the table has drifted from how commands are "+
			"declared, so this test is no longer checking anything", len(paths))
	}
	for _, p := range paths {
		verb := strings.Fields(p)[0]
		if _, ok := lookup(verb); !ok {
			t.Errorf("the catalog advertises %q but %q does not dispatch", p, verb)
		}
	}

	// And the number nobody types.
	if !strings.Contains(catalog, fmt.Sprintf("%d tools", mcp.ToolCount())) {
		t.Errorf("catalog does not state the live MCP tool count (%d)", mcp.ToolCount())
	}
}

func TestAICatalogJSON(t *testing.T) {
	code, out, _ := runCLI(t, "", "--json", "ai")
	if code != ExitOK {
		t.Fatalf("code = %d, want 0", code)
	}
	var v struct {
		Version  string   `json:"version"`
		Catalog  string   `json:"catalog"`
		Commands []string `json:"commands"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if v.Version != Version || v.Catalog == "" || len(v.Commands) == 0 {
		t.Fatalf("incomplete JSON catalog: %+v", v.Version)
	}
}

func TestVersionCommand(t *testing.T) {
	code, out, _ := runCLI(t, "", "version")
	if code != ExitOK || !strings.Contains(out, Version) {
		t.Fatalf("version output %q (code %d)", out, code)
	}
}

func TestDoctorReportsMissingKeyButReachableServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	code, out, _ := runCLI(t, srv.URL, "--json", "doctor")
	if code != ExitAPI {
		t.Fatalf("doctor without key should fail overall, code = %d", code)
	}
	var v struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Name string `json:"name"`
			OK   bool   `json:"ok"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("doctor --json invalid: %v\n%s", err, out)
	}
	got := map[string]bool{}
	for _, ch := range v.Checks {
		got[ch.Name] = ch.OK
	}
	if v.OK || got["api_key"] {
		t.Fatalf("api_key check should fail: %+v", v)
	}
	if !got["reachable"] {
		t.Fatalf("reachable should pass against test server: %+v", v)
	}
}

func TestDoctorAuthCheckPasses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Write([]byte(`{"status":"ok"}`))
		case "/v1/templates":
			w.Write([]byte(`{"templates":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("PILLDROPS_API_KEY", "sfc_ok")
	code, _, _ := runCLI2(t, srv.URL, "doctor")
	if code != ExitOK {
		t.Fatalf("doctor with valid key = %d, want 0", code)
	}
}

// runCLI2 mirrors runCLI but preserves PILLDROPS_API_KEY set by the caller
// (runCLI blanks only the SANDFORGE_ names).
func runCLI2(t *testing.T, srvURL string, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("SANDFORGE_CONFIG", t.TempDir()+"/config.json")
	t.Setenv("SANDFORGE_API_URL", srvURL)
	var out, errb strings.Builder
	code := Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestJSONErrorCarriesHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"no such sandbox"}`))
	}))
	defer srv.Close()
	t.Setenv("PILLDROPS_API_KEY", "sfc_ok")
	code, _, errS := runCLI2(t, srv.URL, "--json", "sandbox", "get", "sb_missing")
	if code != ExitAPI {
		t.Fatalf("code = %d, want %d", code, ExitAPI)
	}
	var v struct {
		Error string `json:"error"`
		Hint  string `json:"hint"`
	}
	if err := json.Unmarshal([]byte(errS), &v); err != nil {
		t.Fatalf("stderr not JSON: %v\n%s", err, errS)
	}
	if !strings.Contains(v.Hint, "history") {
		t.Fatalf("404 hint should mention history, got %q", v.Hint)
	}
}
