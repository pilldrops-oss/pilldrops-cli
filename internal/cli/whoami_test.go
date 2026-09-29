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

// fakePlatform serves what this platform serves: GET /v1/whoami, and nothing
// at /v1/auth/whoami. Against it, `login --key` used to 404 and store nothing.
func fakePlatform(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/whoami" {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"not found","code":"not_found"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer sf_good" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"invalid API key","code":"unauthorized"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
}

func TestLoginAgainstPlatformWhoami(t *testing.T) {
	srv := fakePlatform(t, `{"org":"acme","principal":"key"}`)
	defer srv.Close()
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_KEY", "")
	t.Setenv("SANDFORGE_API_URL", "")

	var out, errb bytes.Buffer
	if code := Run([]string{"login", "--api-url", srv.URL, "--key", "sf_good"}, &out, &errb); code != ExitOK {
		t.Fatalf("login = %d, stderr = %s", code, errb.String())
	}
	// The failure this fixes: the key was never stored, because the validating
	// call 404'd before SaveConfig.
	cfg, _ := LoadConfig()
	if cfg.Key != "sf_good" || cfg.APIURL != srv.URL {
		t.Fatalf("login stored %+v — the key must be on disk", cfg)
	}

	out.Reset()
	if code := Run([]string{"--json", "whoami"}, &out, &errb); code != ExitOK {
		t.Fatalf("whoami = %d, stderr = %s", code, errb.String())
	}
	var v map[string]any
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("whoami --json: %v\n%s", err, out.String())
	}
	if v["principal"] != "key" || v["org"] != "acme" {
		t.Fatalf("whoami named %v, want the platform's principal and org", v)
	}
}

// A sandbox's own token names the sandbox, not a person.
func TestWhoamiNamesTheSandboxPrincipal(t *testing.T) {
	srv := fakePlatform(t, `{"org":"acme","principal":"sandbox","sandbox_id":"sb_ab12cd"}`)
	defer srv.Close()
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_URL", srv.URL)
	t.Setenv("PILLDROPS_API_KEY", "sf_good")

	var out, errb bytes.Buffer
	if code := Run([]string{"whoami"}, &out, &errb); code != ExitOK {
		t.Fatalf("whoami = %d, stderr = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "sandbox sb_ab12cd") {
		t.Fatalf("whoami said %q, want the sandbox principal", out.String())
	}
}

// The hosted service still answers only the older path, and one binary talks to
// both — so a 404 there falls through to it.
func TestWhoamiFallsBackToLegacyPath(t *testing.T) {
	srv := fakePortalAuth(t) // serves /v1/auth/whoami only
	defer srv.Close()
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_URL", srv.URL)
	t.Setenv("PILLDROPS_API_KEY", "sfc_good")

	var out, errb bytes.Buffer
	if code := Run([]string{"whoami"}, &out, &errb); code != ExitOK {
		t.Fatalf("whoami = %d, stderr = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "a@b.c") {
		t.Fatalf("whoami said %q, want the hosted service's email", out.String())
	}
}
