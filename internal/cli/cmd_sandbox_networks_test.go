package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// networksPortal records what the CLI sends, which is the whole point: a PATCH
// that replaces must not arrive as an add, and an unset --networks must not
// arrive as an empty list (the API reads [] as "leave every network").
func networksPortal(t *testing.T, seen *map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		*seen = body
		w.WriteHeader(201)
		w.Write([]byte(`{"id":"sb_1","template":"base","status":"running"}`))
	})
	mux.HandleFunc("PATCH /v1/sandboxes/sb_1", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		*seen = body
		w.Write([]byte(`{"id":"sb_1","status":"running","private_ip":"10.128.0.6"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestCreateSendsNetworksOnlyWhenAsked(t *testing.T) {
	var seen map[string]any
	srv := networksPortal(t, &seen)

	if code, _, errS := authedCLI(t, srv, "sandbox", "create", "--template", "base"); code != ExitOK {
		t.Fatalf("create = %d %s", code, errS)
	}
	if _, ok := seen["networks"]; ok {
		t.Fatalf("a create without --networks sent %v; absent and [] differ to the API", seen["networks"])
	}

	if code, _, errS := authedCLI(t, srv, "sandbox", "create", "--template", "base",
		"--networks", "web, db,", "--slug", "api"); code != ExitOK {
		t.Fatalf("create = %d %s", code, errS)
	}
	got, _ := seen["networks"].([]any)
	if len(got) != 2 || got[0] != "web" || got[1] != "db" {
		t.Fatalf("networks = %v, want [web db] with the blank from the trailing comma dropped", seen["networks"])
	}
	if seen["slug"] != "api" {
		t.Fatalf("slug = %v", seen["slug"])
	}
}

func TestSandboxNetworksReplacesAndClears(t *testing.T) {
	var seen map[string]any
	srv := networksPortal(t, &seen)

	code, out, errS := authedCLI(t, srv, "sandbox", "networks", "sb_1", "web,db")
	if code != ExitOK {
		t.Fatalf("networks = %d %s", code, errS)
	}
	got, _ := seen["networks"].([]any)
	if len(got) != 2 {
		t.Fatalf("PATCH body = %v, want both tags", seen["networks"])
	}
	if !strings.Contains(out, "10.128.0.6") {
		t.Errorf("output %q does not show the address peers reach it on", out)
	}

	if code, out, errS := authedCLI(t, srv, "sandbox", "networks", "sb_1", "--clear"); code != ExitOK {
		t.Fatalf("--clear = %d %s", code, errS)
	} else if !strings.Contains(out, "no network") {
		t.Errorf("--clear said %q", out)
	}
	if got, _ := seen["networks"].([]any); len(got) != 0 {
		t.Fatalf("--clear sent %v, want an empty list", seen["networks"])
	}
	if _, ok := seen["networks"]; !ok {
		t.Fatal("--clear sent no networks field at all, so the API would leave the membership alone")
	}

	// No tags and no --clear is a usage error, not a silent clear: the two are
	// one keystroke apart and one of them empties a network.
	if code, _, _ := authedCLI(t, srv, "sandbox", "networks", "sb_1"); code != ExitUsage {
		t.Fatalf("bare `networks <id>` returned %d, want a usage error", code)
	}
}
