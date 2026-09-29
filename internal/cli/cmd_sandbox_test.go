package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// fakePortal implements the /v1 endpoints the lifecycle commands use.
func fakePortal(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer sfc_good" {
				w.WriteHeader(401)
				w.Write([]byte(`{"error":"invalid api key"}`))
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /v1/templates", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":"code-interpreter","name":"python-3.11","cpu":2,"memory_mb":1024}]`))
	}))
	mux.HandleFunc("POST /v1/sandboxes", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["template"] != "python-3.11" {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"unknown template"}`))
			return
		}
		w.WriteHeader(201)
		w.Write([]byte(`{"id":"sb_1","template":"python-3.11","status":"running"}`))
	}))
	mux.HandleFunc("GET /v1/sandboxes/sb_1", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"sb_1","template":"python-3.11","status":"running","url":"https://sb-1.example"}`))
	}))
	mux.HandleFunc("DELETE /v1/sandboxes/sb_1", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"destroyed"}`))
	}))
	mux.HandleFunc("POST /v1/sandboxes/sb_1/pause", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"paused"}`))
	}))
	mux.HandleFunc("POST /v1/sandboxes/sb_1/resume", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"running"}`))
	}))
	return httptest.NewServer(mux)
}

// authedCLI configures env so commands run against srv with a valid key.
func authedCLI(t *testing.T, srv *httptest.Server, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_URL", srv.URL)
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")
	var out, errb bytes.Buffer
	code := Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestSandboxCreateJSON(t *testing.T) {
	srv := fakePortal(t)
	defer srv.Close()
	code, out, errS := authedCLI(t, srv, "--json", "sandbox", "create", "--template", "python-3.11")
	if code != ExitOK {
		t.Fatalf("create = %d stderr=%s", code, errS)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json emitted non-JSON: %v\n%s", err, out)
	}
	if got["id"] != "sb_1" {
		t.Fatalf("created id = %v", got["id"])
	}
}

func TestSandboxGetRmPauseResume(t *testing.T) {
	srv := fakePortal(t)
	defer srv.Close()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"sandbox", "get", "sb_1"}, "running"},
		{[]string{"sandbox", "pause", "sb_1"}, "paused"},
		{[]string{"sandbox", "resume", "sb_1"}, "running"},
		{[]string{"sandbox", "rm", "sb_1", "--confirm"}, "destroyed"},
	} {
		code, out, errS := authedCLI(t, srv, tc.args...)
		if code != ExitOK {
			t.Fatalf("%v = %d stderr=%s", tc.args, code, errS)
		}
		if !strings.Contains(out, tc.want) {
			t.Fatalf("%v out = %q, want %q", tc.args, out, tc.want)
		}
	}
}

func TestSandboxGetMissingIDUsage(t *testing.T) {
	srv := fakePortal(t)
	defer srv.Close()
	if code, _, _ := authedCLI(t, srv, "sandbox", "get"); code != ExitUsage {
		t.Fatalf("get without id = %d, want %d", code, ExitUsage)
	}
}

func TestSandboxUnknownSubcommandIsUsageEvenWithoutAuth(t *testing.T) {
	// No key set — an unknown subcommand must still be ExitUsage, not ExitAuth.
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_KEY", "")
	t.Setenv("SANDFORGE_API_URL", "http://unused.invalid")
	var out, errb bytes.Buffer
	if code := Run([]string{"sandbox", "bogus"}, &out, &errb); code != ExitUsage {
		t.Fatalf("unknown subcommand = %d, want %d", code, ExitUsage)
	}
}

func TestTemplatesList(t *testing.T) {
	srv := fakePortal(t)
	defer srv.Close()
	code, out, _ := authedCLI(t, srv, "templates")
	if code != ExitOK || !strings.Contains(out, "python-3.11") {
		t.Fatalf("templates = %d out=%q", code, out)
	}
}

// Three branches each added verbs to this switch, and each spelled the list in
// its own usage line. This keeps the two lists one list.
func TestSandboxSubUsageListsEveryKnownSubcommand(t *testing.T) {
	listed := map[string]bool{}
	for _, v := range strings.Split(sandboxSubUsage, "|") {
		listed[v] = true
	}
	for sub := range knownSandboxSub {
		if !listed[sub] {
			t.Errorf("sandbox %q works but is not in sandboxSubUsage, so no help mentions it", sub)
		}
	}
	for sub := range listed {
		if !knownSandboxSub[sub] {
			t.Errorf("sandboxSubUsage advertises %q, which is not a subcommand", sub)
		}
	}
}
