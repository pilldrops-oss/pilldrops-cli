package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTestPortal emulates create + exec for the `pilldrops test` flow: the
// script upload lands via base64 execs, and status polls walk queued →
// running → done GREEN.
func fakeTestPortal(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	polls := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Template    string `json:"template"`
			IdleTimeout int    `json:"idle_timeout"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Template != "mobile-test" {
			t.Errorf("create template = %q, want mobile-test", body.Template)
		}
		if body.IdleTimeout != -1 {
			t.Errorf("idle_timeout = %d, want -1 (pause kills the emulator)", body.IdleTimeout)
		}
		w.Write([]byte(`{"id":"sb_t1","template":"mobile-test","status":"running","url":"https://8765-sb_t1.example.com/"}`))
	})
	mux.HandleFunc("POST /v1/sandboxes/sb_t1/exec", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Cmd string `json:"cmd"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.Contains(body.Cmd, "status.json") {
			n := polls.Add(1)
			status := `{"state":"running","verdict":null,"steps":[{"n":"Wait for Android boot","s":"pass"},{"n":"Run /test/run.sh (screen recording on)","s":"run"}]}`
			if n >= 2 {
				status = `{"state":"done","verdict":"GREEN","exit":0,"steps":[{"n":"Wait for Android boot","s":"pass"},{"n":"Run /test/run.sh (screen recording on)","s":"pass"}],"videos":["pd-rec-1.mp4"]}`
			}
			fmt.Fprintf(w, `{"stdout":%q,"stderr":"","exit_code":0}`, status)
			return
		}
		w.Write([]byte(`{"stdout":"","stderr":"","exit_code":0}`))
	})
	return httptest.NewServer(mux), polls
}

func TestTestCommandRunsToGreen(t *testing.T) {
	srv, _ := fakeTestPortal(t)
	defer srv.Close()
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_URL", srv.URL)
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")
	old := testPollInterval
	testPollInterval = time.Millisecond
	defer func() { testPollInterval = old }()

	script := filepath.Join(t.TempDir(), "suite.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Run([]string{"test", script}, &out, &errb); code != ExitOK {
		t.Fatalf("test = %d (stderr %q)", code, errb.String())
	}
	for _, want := range []string{"sb_t1", "8765-sb_t1", "verdict: GREEN"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestTestCommandUsage(t *testing.T) {
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")
	t.Setenv("SANDFORGE_API_URL", "http://unused.invalid")
	var out, errb bytes.Buffer
	if code := Run([]string{"test"}, &out, &errb); code != ExitUsage {
		t.Fatalf("test without script = %d, want %d", code, ExitUsage)
	}
	if code := Run([]string{"test", "/does/not/exist.sh"}, &out, &errb); code != ExitUsage {
		t.Fatalf("missing script file = %d, want %d", code, ExitUsage)
	}
}
