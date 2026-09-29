package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// perPortPortal serves the three routes the ports view reads: the sandbox (for
// the URL shape the deployment actually serves), its listeners, and the
// per-port token mint. sbURL is what this deployment puts in `url`.
func perPortPortal(t *testing.T, sbURL string, mint bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/sb_1", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{"id": "sb_1", "status": "running"}
		if sbURL != "" {
			body["url"] = sbURL
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("GET /v1/sandboxes/sb_1/ports", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ports":[{"port":3000,"listening":false},{"port":8080,"listening":true}]}`))
	})
	if mint {
		mux.HandleFunc("GET /v1/sandboxes/sb_1/preview-token", func(w http.ResponseWriter, r *http.Request) {
			// The token is bound to the port asked for; echoing it proves the
			// CLI minted one per port rather than reusing the sandbox's.
			_, _ = w.Write([]byte(`{"token":"tok-` + r.URL.Query().Get("port") + `","port":0}`))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestPortsPrintsThePerPortURLThePlatformServes(t *testing.T) {
	// The sandbox's own URL names 3000; the listener is on 8080. A CLI that
	// echoed `url` would print the wrong port and pass a weaker test.
	srv := perPortPortal(t, "https://3000-sb_1.example/", true)
	code, out, errS := authedCLI(t, srv, "sandbox", "ports", "sb_1")
	if code != ExitOK {
		t.Fatalf("ports = %d stderr=%s", code, errS)
	}
	if !strings.Contains(out, "https://8080-sb_1.example/?token=tok-8080") {
		t.Fatalf("ports out = %q, want the listener's own tokened link", out)
	}
	// Every probed port, with the answer that distinguishes them. Printing only
	// the listeners meant a sandbox whose server was not up yet produced no
	// output and exit 0 — indistinguishable from a broken command, and that is
	// precisely the moment you run this.
	for _, want := range []string{"PORT", "LISTENING", "URL", "3000", "no", "8080", "yes"} {
		if !strings.Contains(out, want) {
			t.Fatalf("ports out = %q, missing %q", out, want)
		}
	}
}

// A sandbox with nothing listening must say so rather than print nothing and
// exit 0, and must point at the way to get a link anyway.
func TestPortsWithNoListenerSaysSo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/sb_1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"sb_1","url":"https://8000-sb_1.example/"}`))
	})
	mux.HandleFunc("GET /v1/sandboxes/sb_1/ports", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ports":[{"port":3000,"listening":false}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	code, out, errS := authedCLI(t, srv, "sandbox", "ports", "sb_1")
	if code != ExitOK {
		t.Fatalf("ports = %d, want 0: nothing failed", code)
	}
	if strings.TrimSpace(errS) == "" {
		t.Fatalf("nothing was said about there being no listener; stdout was %q", out)
	}
	if !strings.Contains(errS, "open --port") {
		t.Errorf("the note does not point at the way to get a link anyway: %q", errS)
	}
}

func TestPortsPathFormDeployment(t *testing.T) {
	srv := perPortPortal(t, "http://portal.example/preview/sb_1/3000/", false)
	_, out, _ := authedCLI(t, srv, "sandbox", "ports", "sb_1")
	if !strings.Contains(out, "http://portal.example/preview/sb_1/8080/") {
		t.Fatalf("path-form deployment out = %q", out)
	}
	if strings.Contains(out, "token=") {
		t.Fatalf("no mint route, so no token may be claimed: %q", out)
	}
}

// Nothing is invented: a sandbox the platform gave no URL for still prints
// only the port, as it always did.
func TestPortsWithoutAPlatformURLPrintsNoLink(t *testing.T) {
	srv := perPortPortal(t, "", true)
	_, out, _ := authedCLI(t, srv, "sandbox", "ports", "sb_1")
	if strings.Contains(out, "http") {
		t.Fatalf("out = %q, want the ports with no link invented", out)
	}
	if !strings.Contains(out, "8080") {
		t.Fatalf("out = %q, want the listener listed", out)
	}
}

func TestPortsJSONCarriesTheURL(t *testing.T) {
	srv := perPortPortal(t, "https://3000-sb_1.example/", true)
	_, out, _ := authedCLI(t, srv, "--json", "sandbox", "ports", "sb_1")
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json emitted non-JSON: %v\n%s", err, out)
	}
	for _, p := range got {
		if p["port"] == float64(8080) && !strings.HasPrefix(p["url"].(string), "https://8080-sb_1.example/") {
			t.Fatalf("listener carries no per-port url: %v", p)
		}
		if p["port"] == float64(3000) && p["url"] != nil {
			t.Fatalf("a port with no listener got a url: %v", p)
		}
	}
}

func TestSandboxGetShowsThePerPortForm(t *testing.T) {
	srv := perPortPortal(t, "https://3000-sb_1.example/", true)
	_, out, _ := authedCLI(t, srv, "sandbox", "get", "sb_1")
	if !strings.Contains(out, "https://<port>-sb_1.example/") {
		t.Fatalf("get does not mention the per-port hostname: %q", out)
	}
}

func TestPerPortURLLeavesUnknownShapesAlone(t *testing.T) {
	for _, base := range []string{"", "https://myslug.example/", "not a url", "https://example.com/app"} {
		if got := perPortURL(base, "8080"); got != "" {
			t.Fatalf("perPortURL(%q) = %q, want \"\" (only the platform's own two forms are restated)", base, got)
		}
	}
}

// F3: `pause` means opposite things in this CLI and in boxd. Nothing is
// renamed — scripts depend on both names — but their cold verb must land on
// our cold verb rather than somewhere plausible and wrong.
func TestHibernateIsPauseAndStandbyIsSeparate(t *testing.T) {
	hit := ""
	mux := http.NewServeMux()
	for _, route := range []string{"pause", "standby"} {
		mux.HandleFunc("POST /v1/sandboxes/sb_1/"+route, func(w http.ResponseWriter, r *http.Request) {
			hit = strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/sb_1/")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		})
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	for _, tc := range []struct{ verb, want string }{
		{"pause", "pause"},
		{"hibernate", "pause"}, // the cold one, under the other tool's name
		{"standby", "standby"}, // the warm one, which is THEIR "pause"
	} {
		hit = ""
		if code, _, errS := authedCLI(t, srv, "sandbox", tc.verb, "sb_1"); code != ExitOK {
			t.Fatalf("sandbox %s = %d stderr=%s", tc.verb, code, errS)
		}
		if hit != tc.want {
			t.Fatalf("sandbox %s called /%s, want /%s", tc.verb, hit, tc.want)
		}
	}
}

// Help that describes one freeze without naming the other is how the collision
// bites; both entries must point at each other.
func TestFreezeHelpNamesTheOtherVerb(t *testing.T) {
	var b strings.Builder
	usage(&b)
	help := b.String()
	_, block, ok := strings.Cut(help, "sandbox standby <id>")
	if !ok {
		t.Fatal("help has no standby entry at all")
	}
	block, _, _ = strings.Cut(block, "sandbox refresh")
	// The two freezes are described together, each naming the other and the
	// alias, so the word "pause" can never be read on its own.
	for _, want := range []string{"RAM", "sandbox pause", "disk", "hibernate", "standby"} {
		if !strings.Contains(block, want) {
			t.Fatalf("the freeze entries never mention %q:\n%s", want, block)
		}
	}
}
