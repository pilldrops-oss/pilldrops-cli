package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// runCLI invokes Run with isolated config/env and returns exit code + streams.
func runCLI(t *testing.T, srvURL string, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_KEY", "")
	t.Setenv("SANDFORGE_API_URL", srvURL)
	var out, errb bytes.Buffer
	code := Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestUnknownCommandIsUsageError(t *testing.T) {
	code, _, errS := runCLI(t, "", "frobnicate")
	if code != ExitUsage {
		t.Fatalf("code = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(errS, "usage") {
		t.Fatalf("stderr should show usage, got %q", errS)
	}
}

func TestNoArgsShowsUsage(t *testing.T) {
	if code, _, _ := runCLI(t, ""); code != ExitUsage {
		t.Fatalf("bare invocation = %d, want %d", code, ExitUsage)
	}
}

func TestAuthCommandsWithoutKeyExitAuth(t *testing.T) {
	code, _, errS := runCLI(t, "http://unused.invalid", "sandbox", "ls")
	if code != ExitAuth {
		t.Fatalf("code = %d, want %d (auth)", code, ExitAuth)
	}
	if !strings.Contains(errS, "pilldrops login") {
		t.Fatalf("stderr should hint at login, got %q", errS)
	}
}

func TestServer401MapsToExitAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer srv.Close()
	t.Setenv("SANDFORGE_API_KEY", "sfc_stale")
	var out, errb bytes.Buffer
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_URL", srv.URL)
	if code := Run([]string{"sandbox", "ls"}, &out, &errb); code != ExitAuth {
		t.Fatalf("code = %d, want %d", code, ExitAuth)
	}
}
