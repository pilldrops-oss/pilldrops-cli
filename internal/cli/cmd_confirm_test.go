package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// destructivePortal answers the destructive routes and counts every call, so a
// test can assert the CLI did not reach the platform at all.
func destructivePortal(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	mux := http.NewServeMux()
	for _, pat := range []string{
		"DELETE /v1/sandboxes/sb_1",
		"DELETE /v1/sandboxes/sb_1/checkpoints/cp1",
		"DELETE /v1/sandboxes/sb_1/backups/bk1",
		"DELETE /v1/snapshots/snap1",
	} {
		mux.HandleFunc(pat, func(w http.ResponseWriter, r *http.Request) {
			calls++
			_, _ = w.Write([]byte(`{"status":"destroyed","deleted_versions":1}`))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &calls
}

// The point is not that --confirm parses: it is that without it NOTHING is
// sent. A flag that parses and deletes anyway would pass a parse-only test.
func TestDestructiveVerbsRequireConfirm(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"sandbox rm", []string{"sandbox", "rm", "sb_1"}},
		{"checkpoint rm", []string{"checkpoint", "rm", "sb_1", "cp1"}},
		{"backup rm", []string{"backup", "rm", "sb_1", "bk1"}},
		{"snapshot rm", []string{"snapshot", "rm", "snap1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, calls := destructivePortal(t)
			code, _, errS := authedCLI(t, srv, tc.args...)
			if code != ExitUsage {
				t.Fatalf("%v without --confirm = %d, want %d", tc.args, code, ExitUsage)
			}
			if *calls != 0 {
				t.Fatalf("%v without --confirm still called the platform %d time(s)", tc.args, *calls)
			}
			if !strings.Contains(errS, "--confirm") {
				t.Fatalf("%v refusal does not say how to proceed: %q", tc.args, errS)
			}
			// …and with it, exactly one request goes out.
			code, _, errS = authedCLI(t, srv, append(tc.args, "--confirm")...)
			if code != ExitOK {
				t.Fatalf("%v --confirm = %d stderr=%s", tc.args, code, errS)
			}
			if *calls != 1 {
				t.Fatalf("%v --confirm sent %d requests, want 1", tc.args, *calls)
			}
		})
	}
}

// -y is the same flag, anywhere in the line: a destructive verb must not have
// its own spelling.
func TestConfirmSpellingsAndPlacement(t *testing.T) {
	for _, args := range [][]string{
		{"sandbox", "rm", "-y", "sb_1"},
		{"sandbox", "rm", "sb_1", "--yes"},
		{"checkpoint", "rm", "-y", "sb_1", "cp1"},
	} {
		srv, calls := destructivePortal(t)
		if code, _, errS := authedCLI(t, srv, args...); code != ExitOK {
			t.Fatalf("%v = %d stderr=%s", args, code, errS)
		}
		if *calls != 1 {
			t.Fatalf("%v sent %d requests, want 1", args, *calls)
		}
	}
}
