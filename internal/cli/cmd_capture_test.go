package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// capturePortal serves the snapshot, checkpoint, backup and filesystem routes.
// gets counts every request so a refused verb can be shown to send none.
func capturePortal(t *testing.T, calls *atomic.Int32, seen map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	record := func(key string, w http.ResponseWriter, r *http.Request, body string) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		seen[key] = string(b)
		seen[key+"?"] = r.URL.RawQuery
		w.Write([]byte(body))
	}
	var polls atomic.Int32
	mux.HandleFunc("POST /v1/sandboxes/sb_1/snapshot", func(w http.ResponseWriter, r *http.Request) {
		record("snapshot.save", w, r, `{"name":"gold","version":1,"status":"building","build_id":"bld_1","snapshot_kind":"memory"}`)
	})
	mux.HandleFunc("GET /v1/snapshots/gold", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if polls.Add(1) < 2 {
			w.Write([]byte(`{"name":"gold","versions":[{"name":"gold","version":1,"status":"building","build_id":"bld_1"}]}`))
			return
		}
		w.Write([]byte(`{"name":"gold","versions":[{"name":"gold","version":1,"status":"ready","build_id":"bld_1","size":1048576,"vcpu":2,"mem_mb":1024}]}`))
	})
	mux.HandleFunc("DELETE /v1/snapshots/gold", func(w http.ResponseWriter, r *http.Request) {
		record("snapshot.rm", w, r, `{"name":"gold","deleted_versions":2}`)
	})
	mux.HandleFunc("POST /v1/sandboxes/sb_1/backups/bkp_1/restore", func(w http.ResponseWriter, r *http.Request) {
		record("backup.restore", w, r, `{"status":"running","id":"bkp_1","restore_ms":12}`)
	})
	mux.HandleFunc("POST /v1/sandboxes/sb_1/checkpoints/cp1/restore", func(w http.ResponseWriter, r *http.Request) {
		record("checkpoint.restore", w, r, `{"status":"running","name":"cp1","restore_ms":9}`)
	})
	mux.HandleFunc("GET /v1/sandboxes/sb_1/filesystem/list", func(w http.ResponseWriter, r *http.Request) {
		record("fs.ls", w, r, `{"entries":[{"name":"a.txt","size":6}]}`)
	})
	mux.HandleFunc("POST /v1/sandboxes/sb_1/filesystem/move", func(w http.ResponseWriter, r *http.Request) {
		record("fs.mv", w, r, `{"status":"ok"}`)
	})
	return httptest.NewServer(mux)
}

func TestSnapshotSaveWaitsUntilReady(t *testing.T) {
	var calls atomic.Int32
	seen := map[string]string{}
	srv := capturePortal(t, &calls, seen)
	defer srv.Close()
	snapshotPollEvery = time.Millisecond
	defer func() { snapshotPollEvery = 3 * time.Second }()

	code, out, errS := authedCLI(t, srv, "snapshot", "save", "sb_1", "gold")
	if code != ExitOK {
		t.Fatalf("save = %d stderr=%s", code, errS)
	}
	if !strings.Contains(out, "ready") || !strings.Contains(out, "1.0 MiB") {
		t.Fatalf("save should report the finished version, got %q", out)
	}
	if !strings.Contains(seen["snapshot.save"], `"name":"gold"`) {
		t.Fatalf("save body = %q", seen["snapshot.save"])
	}
}

// Every one of these overwrites or deletes something nothing restores. None of
// them may reach the API without the flag.
func TestDestructiveVerbsRefuseWithoutConfirm(t *testing.T) {
	for _, args := range [][]string{
		{"snapshot", "rm", "gold"},
		{"checkpoint", "restore", "sb_1", "cp1"},
		{"backup", "restore", "sb_1", "bkp_1"},
	} {
		var calls atomic.Int32
		srv := capturePortal(t, &calls, map[string]string{})
		code, _, errS := authedCLI(t, srv, args...)
		srv.Close()
		if code != ExitUsage {
			t.Errorf("%v = %d, want %d", args, code, ExitUsage)
		}
		if calls.Load() != 0 {
			t.Errorf("%v reached the API without --confirm", args)
		}
		if !strings.Contains(errS, "--confirm") {
			t.Errorf("%v should name the flag, got %q", args, errS)
		}
	}
}

// The API refuses an unconfirmed restore itself; --confirm must be carried into
// the body, not just checked here.
func TestBackupRestoreSendsConfirmTrue(t *testing.T) {
	var calls atomic.Int32
	seen := map[string]string{}
	srv := capturePortal(t, &calls, seen)
	defer srv.Close()
	code, _, errS := authedCLI(t, srv, "backup", "restore", "sb_1", "bkp_1", "--confirm")
	if code != ExitOK {
		t.Fatalf("restore = %d stderr=%s", code, errS)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(seen["backup.restore"]), &body); err != nil {
		t.Fatalf("body %q: %v", seen["backup.restore"], err)
	}
	if body["confirm"] != true {
		t.Fatalf("restore body = %v, want confirm true", body)
	}
}

// ~/ and relative paths mean the sandbox home, as `cp` already spells them.
func TestFSPathsAreSandboxHome(t *testing.T) {
	var calls atomic.Int32
	seen := map[string]string{}
	srv := capturePortal(t, &calls, seen)
	defer srv.Close()
	if code, _, errS := authedCLI(t, srv, "fs", "ls", "sb_1", "~/work"); code != ExitOK {
		t.Fatalf("ls = %d stderr=%s", code, errS)
	}
	if seen["fs.ls?"] != "path=work" {
		t.Fatalf("ls query = %q, want path=work", seen["fs.ls?"])
	}
	if code, _, errS := authedCLI(t, srv, "fs", "mv", "sb_1", "~/a", "b"); code != ExitOK {
		t.Fatalf("mv = %d stderr=%s", code, errS)
	}
	if !strings.Contains(seen["fs.mv"], `"src":"a"`) || !strings.Contains(seen["fs.mv"], `"dst":"b"`) {
		t.Fatalf("mv body = %q", seen["fs.mv"])
	}
}
