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

// A trailing --json used to be dropped (commands with no flag set) or rejected
// ("flag provided but not defined", the ones with one). Both are the position
// the flag falls into naturally once you are already typing the command.
//
// Asserting the output PARSES as JSON is the point: a run that merely accepted
// the flag and printed a table would pass an exit-code test.
func TestGlobalFlagsWorkAfterTheSubcommand(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"sandboxes":[{"id":"sb_1","template":"t","status":"running"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, args := range [][]string{
		{"--json", "sandbox", "ls"},
		{"sandbox", "ls", "--json"},
		{"sandbox", "--json", "ls"},
	} {
		code, out, errS := authedCLI(t, srv, args...)
		if code != ExitOK {
			t.Fatalf("%v = %d stderr=%s", args, code, errS)
		}
		var got []map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("%v printed non-JSON: %v\n%s", args, err, out)
		}
		if len(got) != 1 || got[0]["id"] != "sb_1" {
			t.Fatalf("%v = %v", args, got)
		}
	}
}

// exec passes everything after -- to the other side untouched. A global flag
// there belongs to the remote command, and must not be eaten on the way.
func TestGlobalFlagsStopAtTheSeparator(t *testing.T) {
	var sent string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/sb_1/exec", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Cmd string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent = body.Cmd
		_, _ = w.Write([]byte(`{"stdout":"hi\n","exit_code":0}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	code, out, errS := authedCLI(t, srv, "exec", "sb_1", "--", "mytool", "--json", "--quiet", "--api-url", "http://elsewhere")
	if code != ExitOK {
		t.Fatalf("exec = %d stderr=%s", code, errS)
	}
	if want := "mytool --json --quiet --api-url http://elsewhere"; sent != want {
		t.Fatalf("remote command = %q, want %q", sent, want)
	}
	// The remote --json must not have switched OUR output to JSON either.
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("a flag after -- was read as ours: %q", out)
	}
}

// login defines its own --api-url. Taking it globally must land in the same
// place, or a flag has been swallowed from under a command that owns it.
func TestLoginStillHonoursItsOwnAPIURL(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"org":"o1","principal":"key","email":"a@b.c"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("SANDFORGE_CONFIG", cfg)
	t.Setenv("SANDFORGE_API_URL", "")
	t.Setenv("PILLDROPS_API_URL", "")
	t.Setenv("SANDFORGE_API_KEY", "")
	t.Setenv("PILLDROPS_API_KEY", "")
	var out, errb bytes.Buffer
	if code := Run([]string{"login", "--key", "k1", "--api-url", srv.URL}, &out, &errb); code != ExitOK {
		t.Fatalf("login = %d stderr=%s", code, errb.String())
	}
	got, err := LoadConfig()
	if err != nil || got.APIURL != srv.URL {
		t.Fatalf("login stored api_url %q (err %v), want %q", got.APIURL, err, srv.URL)
	}
}
