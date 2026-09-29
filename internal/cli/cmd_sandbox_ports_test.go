package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// portsPortal serves /ports and PATCH for sb_1 and records the PATCH body.
func portsPortal(t *testing.T) (*httptest.Server, *map[string]any) {
	t.Helper()
	var patched map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/sb_1/ports", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ports":[{"port":3000,"listening":false},{"port":8000,"listening":true}]}`))
	})
	mux.HandleFunc("PATCH /v1/sandboxes/sb_1", func(w http.ResponseWriter, r *http.Request) {
		patched = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&patched)
		w.Write([]byte(`{"id":"sb_1","name":"web","status":"running","url":"https://3000-sb_1.example/"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &patched
}

func TestSandboxPortsShowsEveryProbedPortAndWhichAnswered(t *testing.T) {
	srv, _ := portsPortal(t)
	code, out, errS := authedCLI(t, srv, "sandbox", "ports", "sb_1")
	if code != ExitOK {
		t.Fatalf("ports = %d stderr=%s", code, errS)
	}
	for _, want := range []string{"PORT", "LISTENING", "3000", "8000", "yes", "no"} {
		if !strings.Contains(out, want) {
			t.Fatalf("ports out = %q, missing %q — the command whose help promises "+
				"\"each with its own hostname\" printed a bare list", out, want)
		}
	}
}

func TestSandboxSetPortAndRenamePatch(t *testing.T) {
	srv, patched := portsPortal(t)
	code, out, errS := authedCLI(t, srv, "sandbox", "set-port", "sb_1", "3000")
	if code != ExitOK {
		t.Fatalf("set-port = %d stderr=%s", code, errS)
	}
	if (*patched)["port"] != float64(3000) || (*patched)["name"] != nil {
		t.Fatalf("set-port sent %v, want {port:3000} and no name", *patched)
	}
	if !strings.Contains(out, "https://3000-sb_1.example/") {
		t.Fatalf("set-port out = %q, want the new preview url", out)
	}
	if code, _, _ := authedCLI(t, srv, "sandbox", "set-port", "sb_1", "70000"); code != ExitUsage {
		t.Fatalf("set-port 70000 = %d, want usage error", code)
	}

	if code, _, errS := authedCLI(t, srv, "sandbox", "rename", "sb_1", "--name", "web"); code != ExitOK {
		t.Fatalf("rename = %d stderr=%s", code, errS)
	}
	if (*patched)["name"] != "web" || (*patched)["port"] != nil {
		t.Fatalf("rename sent %v, want {name:web} and no port", *patched)
	}
	if code, _, errS := authedCLI(t, srv, "sandbox", "rename", "sb_1", "--slug", "web"); code != ExitOK {
		t.Fatalf("rename --slug = %d stderr=%s", code, errS)
	}
	if (*patched)["slug"] != "web" || (*patched)["name"] != nil {
		t.Fatalf("rename --slug sent %v, want {slug:web}", *patched)
	}
	if code, _, _ := authedCLI(t, srv, "sandbox", "rename", "sb_1"); code != ExitUsage {
		t.Fatalf("rename with nothing to change = %d, want usage error", code)
	}
}
