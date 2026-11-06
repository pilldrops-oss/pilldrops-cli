package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// fakePortalAuth serves whoami for key "sfc_good" and 401 otherwise.
func fakePortalAuth(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/whoami" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") == "Bearer sfc_good" {
			w.Write([]byte(`{"owner":"u:user_1","email":"a@b.c"}`))
			return
		}
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"invalid api key"}`))
	}))
}

func TestLoginWithPastedKeyStoresAndVerifies(t *testing.T) {
	srv := fakePortalAuth(t)
	defer srv.Close()
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_KEY", "")
	t.Setenv("SANDFORGE_API_URL", "")

	var out, errb bytes.Buffer
	code := Run([]string{"login", "--api-url", srv.URL, "--key", "sfc_good"}, &out, &errb)
	if code != ExitOK {
		t.Fatalf("login = %d, stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "a@b.c") {
		t.Fatalf("stdout should show the signed-in email, got %q", out.String())
	}
	cfg, _ := LoadConfig()
	if cfg.Key != "sfc_good" || cfg.APIURL != srv.URL || cfg.Email != "a@b.c" {
		t.Fatalf("stored config = %+v", cfg)
	}

	out.Reset()
	if code := Run([]string{"whoami"}, &out, &errb); code != ExitOK {
		t.Fatalf("whoami = %d", code)
	}
	if !strings.Contains(out.String(), "u:user_1") {
		t.Fatalf("whoami out = %q", out.String())
	}

	if code := Run([]string{"logout"}, &out, &errb); code != ExitOK {
		t.Fatalf("logout = %d", code)
	}
	// Logout clears the credential but keeps the portal URL, so a follow-up
	// `login --key` doesn't need --api-url re-supplied.
	after, _ := LoadConfig()
	if after.Key != "" || after.Email != "" {
		t.Fatalf("credential survived logout: %+v", after)
	}
	if after.APIURL != srv.URL {
		t.Fatalf("logout wiped api_url: got %q, want %q", after.APIURL, srv.URL)
	}
	// And that follow-up login (no --api-url) must succeed off the retained URL.
	if code := Run([]string{"login", "--key", "sfc_good"}, &out, &errb); code != ExitOK {
		t.Fatalf("re-login without --api-url = %d, stderr=%s", code, errb.String())
	}
}

func TestLoginWithBadKeyFails(t *testing.T) {
	srv := fakePortalAuth(t)
	defer srv.Close()
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	var out, errb bytes.Buffer
	if code := Run([]string{"login", "--api-url", srv.URL, "--key", "sfc_bad"}, &out, &errb); code != ExitAuth {
		t.Fatalf("bad-key login = %d, want %d", code, ExitAuth)
	}
	if cfg, _ := LoadConfig(); cfg.Key != "" {
		t.Fatal("bad key was stored")
	}
}

// With nothing to ask and no key given, login must refuse at once: the old
// browser flow waited on a route the server no longer has.
func TestLoginWithoutKeyOffATerminalIsUsage(t *testing.T) {
	for _, args := range [][]string{{"login"}, {"--json", "login"}} {
		code, out, errS := runCLI(t, "http://unused.invalid", args...)
		if code != ExitUsage {
			t.Fatalf("%v = %d, want %d", args, code, ExitUsage)
		}
		if out != "" || !strings.Contains(errS, loginUsage) {
			t.Fatalf("%v stdout=%q stderr=%q, want only the usage line", args, out, errS)
		}
	}
}
