package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLogsTail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"lines":["l1","l2"],"truncated":false}`))
	}))
	defer srv.Close()
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_URL", srv.URL)
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")

	var out, errb bytes.Buffer
	if code := Run([]string{"logs", "sb_1"}, &out, &errb); code != ExitOK {
		t.Fatalf("logs = %d", code)
	}
	if out.String() != "l1\nl2\n" {
		t.Fatalf("out = %q", out.String())
	}
}

func TestLogsFollowPrintsOnlyNewLines(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Write([]byte(`{"lines":["l1"],"truncated":false}`))
		case 2:
			w.Write([]byte(`{"lines":["l1","l2","l3"],"truncated":false}`))
		default:
			w.Write([]byte(`{"lines":["l1","l2","l3"],"truncated":false}`))
		}
	}))
	defer srv.Close()
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_URL", srv.URL)
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")
	t.Setenv("SANDFORGE_LOGS_POLL_MS", "10")
	t.Setenv("SANDFORGE_LOGS_MAX_POLLS", "3") // test hook: stop after 3 polls

	var out, errb bytes.Buffer
	if code := Run([]string{"logs", "sb_1", "-f"}, &out, &errb); code != ExitOK {
		t.Fatalf("logs -f = %d stderr=%s", code, errb.String())
	}
	if out.String() != "l1\nl2\nl3\n" {
		t.Fatalf("follow out = %q, want each line once", out.String())
	}
	if calls.Load() != 3 {
		t.Fatalf("polls = %d, want 3", calls.Load())
	}
}

func TestLogsMissingIDUsage(t *testing.T) {
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")
	t.Setenv("SANDFORGE_API_URL", "http://unused.invalid")
	var out, errb bytes.Buffer
	if code := Run([]string{"logs"}, &out, &errb); code != ExitUsage {
		t.Fatalf("logs without id = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(errb.String(), "usage") {
		t.Fatalf("stderr = %q", errb.String())
	}
}
