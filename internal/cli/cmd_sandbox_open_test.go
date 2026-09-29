package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func openPortal(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	// The sandbox's own url is the app's port; viewer_url and desktop_url are the
	// screen. A real desktop sandbox publishes 6080 as its OWN url (the template's
	// port IS the desktop port), so nothing needs those two fields to find a
	// screen — and preferring them handed out a 6080 link on every other sandbox,
	// where nothing is listening there at all.
	mux.HandleFunc("GET /v1/sandboxes/sb_desk", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"sb_desk","status":"running","url":"https://8080-sb_desk.example/","viewer_url":"https://6080-sb_desk.example/vnc.html"}`))
	})
	mux.HandleFunc("GET /v1/sandboxes/sb_app/preview-token", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"token":"tok-` + r.URL.Query().Get("port") + `","port":0}`))
	})
	mux.HandleFunc("GET /v1/sandboxes/sb_nodesk", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"sb_nodesk","status":"running","url":"https://8080-sb_nodesk.example/","desktop_url":"https://6080-sb_nodesk.example/vnc.html","has_desktop":true}`))
	})
	mux.HandleFunc("GET /v1/sandboxes/sb_app", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"sb_app","status":"running","url":"https://8080-sb_app.example/?ptok=t"}`))
	})
	mux.HandleFunc("GET /v1/sandboxes/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"sandbox not found"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// `open` hands back a link to the sandbox's OWN published port, with a token
// minted for that port. Against the old code this fails on the first assertion:
// it preferred viewer_url and then desktop_url, so it opened port 6080 — the
// desktop, which the base template never starts — and the token on that link is
// port-bound, so it could not be re-pointed at the app either.
func TestSandboxOpenPrintsTheAppsOwnPortAndLaunchesBrowser(t *testing.T) {
	srv := openPortal(t)
	var launched []string
	old := openBrowser
	openBrowser = func(u string) error { launched = append(launched, u); return nil }
	t.Cleanup(func() { openBrowser = old })

	code, out, errS := authedCLI(t, srv, "sandbox", "open", "sb_desk")
	if code != ExitOK {
		t.Fatalf("open = %d stderr=%s", code, errS)
	}
	if strings.TrimSpace(out) != "https://8080-sb_desk.example/" {
		t.Fatalf("printed %q, want the sandbox's own published port", out)
	}
	if len(launched) != 1 || launched[0] != "https://8080-sb_desk.example/" {
		t.Fatalf("browser launched with %v", launched)
	}

	launched = nil
	code, out, _ = authedCLI(t, srv, "sandbox", "open", "--print", "sb_nodesk")
	if code != ExitOK || strings.TrimSpace(out) != "https://8080-sb_nodesk.example/" {
		t.Fatalf("open = %d out=%q, want the app preview, not a desktop that never starts", code, out)
	}
	if len(launched) != 0 {
		t.Fatalf("--print must not launch a browser, got %v", launched)
	}
}

// The verb that was missing: point `open` at the port you started something on
// and get a link whose token is minted for THAT port.
func TestSandboxOpenPortMintsForThatPort(t *testing.T) {
	srv := openPortal(t)
	old := openBrowser
	openBrowser = func(string) error { return nil }
	t.Cleanup(func() { openBrowser = old })

	code, out, errS := authedCLI(t, srv, "sandbox", "open", "sb_app", "--print", "--port", "3000")
	if code != ExitOK {
		t.Fatalf("open --port = %d stderr=%s", code, errS)
	}
	if got := strings.TrimSpace(out); got != "https://3000-sb_app.example/?token=tok-3000" {
		t.Fatalf("open --port 3000 = %q, want a link on 3000 carrying a token minted for 3000", got)
	}

	// Default: the sandbox's own port, and its own token. Not the port of
	// whichever link the platform happened to return for something else.
	_, out, _ = authedCLI(t, srv, "sandbox", "open", "--print", "sb_app")
	if strings.TrimSpace(out) != "https://8080-sb_app.example/?token=tok-8080" {
		t.Fatalf("open with no --port = %q, want the published port with its own token", out)
	}

	if code, _, _ := authedCLI(t, srv, "sandbox", "open", "sb_app", "--port", "70000"); code != ExitUsage {
		t.Fatalf("open --port 70000 = %d, want a usage error", code)
	}
}

func TestSandboxOpenMissingSandboxFailsWithServerMessage(t *testing.T) {
	srv := openPortal(t)
	launched := 0
	old := openBrowser
	openBrowser = func(string) error { launched++; return nil }
	t.Cleanup(func() { openBrowser = old })

	code, _, errS := authedCLI(t, srv, "sandbox", "open", "sb_nope")
	if code == ExitOK {
		t.Fatal("open of a missing sandbox exited 0")
	}
	if !strings.Contains(errS, "sandbox not found") {
		t.Fatalf("stderr = %q, want the server's message", errS)
	}
	if launched != 0 {
		t.Fatal("opened a browser for a missing sandbox")
	}
}

func TestSandboxOpenUsage(t *testing.T) {
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")
	t.Setenv("SANDFORGE_API_URL", "http://unused.invalid")
	var out, errb bytes.Buffer
	if code := Run([]string{"sandbox", "open"}, &out, &errb); code != ExitUsage {
		t.Fatalf("open without id = %d, want %d", code, ExitUsage)
	}
}
