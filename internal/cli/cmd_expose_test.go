package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// exposeAPI records the upsert body and answers like the control plane does.
func exposeAPI(t *testing.T, got *map[string]any, deleted *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/{id}/forwards", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, got)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"guest_port": (*got)["port"], "public_port": 41042,
			"tcp": (*got)["tcp"], "udp": (*got)["udp"],
			"protocols": protosOf(*got), "endpoint": "fwd.example.com:41042",
		})
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}/forwards", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"forwards": []map[string]any{{
			"guest_port": 5432, "public_port": 41042, "tcp": true, "udp": true,
			"protocols": []string{"tcp", "udp"}, "endpoint": "fwd.example.com:41042",
		}}})
	})
	mux.HandleFunc("DELETE /v1/sandboxes/{id}/forwards/{port}", func(w http.ResponseWriter, r *http.Request) {
		*deleted = r.PathValue("id") + ":" + r.PathValue("port")
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func protosOf(body map[string]any) []string {
	out := []string{}
	if body["tcp"] == true {
		out = append(out, "tcp")
	}
	if body["udp"] == true {
		out = append(out, "udp")
	}
	return out
}

func exposeEnv(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_URL", srv.URL)
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")
}

func TestExposeDefaultsToTCPAndPrintsTheEndpoint(t *testing.T) {
	var body map[string]any
	var deleted string
	exposeEnv(t, exposeAPI(t, &body, &deleted))

	var out, errb bytes.Buffer
	if code := Run([]string{"sandbox", "expose", "sb1", "5432"}, &out, &errb); code != ExitOK {
		t.Fatalf("expose = %d (stderr %q)", code, errb.String())
	}
	if body["tcp"] != true || body["udp"] != false || body["port"] != float64(5432) {
		t.Fatalf("upsert body = %v; naming no protocol must mean tcp", body)
	}
	if !strings.Contains(out.String(), "fwd.example.com:41042") {
		t.Errorf("output does not print the endpoint: %q", out.String())
	}

	// --udp alone means udp only; --tcp --udp means both.
	if code := Run([]string{"sandbox", "expose", "sb1", "5432", "--udp"}, &out, &errb); code != ExitOK {
		t.Fatalf("expose --udp = %d (stderr %q)", code, errb.String())
	}
	if body["tcp"] != false || body["udp"] != true {
		t.Fatalf("--udp body = %v", body)
	}
	if code := Run([]string{"sandbox", "expose", "sb1", "5432", "--tcp", "--udp"}, &out, &errb); code != ExitOK {
		t.Fatalf("expose --tcp --udp = %d", code)
	}
	if body["tcp"] != true || body["udp"] != true {
		t.Fatalf("--tcp --udp body = %v", body)
	}
}

func TestForwardsListsAndUnexposeNamesThePort(t *testing.T) {
	var body map[string]any
	var deleted string
	exposeEnv(t, exposeAPI(t, &body, &deleted))

	var out, errb bytes.Buffer
	if code := Run([]string{"sandbox", "forwards", "sb1"}, &out, &errb); code != ExitOK {
		t.Fatalf("forwards = %d (stderr %q)", code, errb.String())
	}
	for _, want := range []string{"fwd.example.com:41042", "5432", "tcp/udp"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("listing %q is missing %q", out.String(), want)
		}
	}

	out.Reset()
	if code := Run([]string{"sandbox", "unexpose", "sb1", "5432"}, &out, &errb); code != ExitOK {
		t.Fatalf("unexpose = %d (stderr %q)", code, errb.String())
	}
	if deleted != "sb1:5432" {
		t.Errorf("server saw DELETE for %q, want sb1:5432", deleted)
	}
}

// A bad port is a usage error before any request, so a typo cannot allocate a
// public port for something the caller did not mean.
func TestExposeRefusesABadPort(t *testing.T) {
	var body map[string]any
	var deleted string
	exposeEnv(t, exposeAPI(t, &body, &deleted))

	var out, errb bytes.Buffer
	for _, bad := range []string{"0", "70000", "http"} {
		if code := Run([]string{"sandbox", "expose", "sb1", bad}, &out, &errb); code != ExitUsage {
			t.Errorf("expose port %q = %d, want a usage error", bad, code)
		}
	}
	if body != nil {
		t.Errorf("a refused port still sent a request: %v", body)
	}
}
