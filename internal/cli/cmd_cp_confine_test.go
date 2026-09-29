package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// confiningPortal is a sandbox whose files API enforces the rule the platform
// states: a path that resolves outside the sandbox home is refused. It records
// what the upload and the exec were asked to do.
func confiningPortal(t *testing.T, wrote *[]string, ran *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/sb_1/files", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		*wrote = append(*wrote, p)
		if strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "/root/") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid path: file paths are taken relative to the sandbox home, and one that resolves outside it is refused"}`))
			return
		}
		n, _ := io.Copy(io.Discard, r.Body)
		fmt.Fprintf(w, `{"bytes":%d}`, n)
	})
	mux.HandleFunc("POST /v1/sandboxes/sb_1/exec", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		*ran = append(*ran, str(body["cmd"]))
		_, _ = w.Write([]byte(`{"exit_code":0}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func tinyDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

// `cp -r` must be refused wherever `cp` of one file is refused, and in the same
// words. Against the old code this fails on every assertion: the archive went to
// the home directory, so the files API never saw /etc, and the copy then landed
// there through /exec and reported "unpacked".
func TestRecursiveCopyIsConfinedLikeASingleFile(t *testing.T) {
	var wrote, ran []string
	srv := confiningPortal(t, &wrote, &ran)

	code, out, errS := authedCLI(t, srv, "cp", "-r", tinyDir(t), "sb_1:/etc")
	if code == ExitOK {
		t.Errorf("cp -r into /etc succeeded (%q) — the single-file path refuses the same "+
			"destination, so a test that checks the boundary with one file passes while this "+
			"walks straight through it", out)
	}
	if !strings.Contains(errS+out, "resolves outside it is refused") {
		t.Errorf("the refusal does not name the rule the file API states: %q", errS+out)
	}
	for _, p := range wrote {
		if !strings.HasPrefix(p, "/etc") {
			continue
		}
		return // the destination was shown to the files API, which is the fix
	}
	t.Errorf("the files API was never asked about the destination (it saw %v), so nothing "+
		"could have refused it", wrote)
}

// Nothing is created before the refusal: the mkdir -p that used to run first
// left the directory behind whatever happened next.
func TestARefusedRecursiveCopyCreatesNothing(t *testing.T) {
	var wrote, ran []string
	srv := confiningPortal(t, &wrote, &ran)
	_, _, _ = authedCLI(t, srv, "cp", "-r", tinyDir(t), "sb_1:/etc")
	for _, cmd := range ran {
		if strings.Contains(cmd, "mkdir") || strings.Contains(cmd, "tar") {
			t.Errorf("a refused copy still ran %q in the sandbox", cmd)
		}
	}
}

// The ordinary case still works, and the archive no longer litters the home
// directory of a copy going somewhere else.
func TestRecursiveCopyIntoTheHomeDirectoryStillWorks(t *testing.T) {
	var wrote, ran []string
	srv := confiningPortal(t, &wrote, &ran)

	code, out, errS := authedCLI(t, srv, "cp", "-r", tinyDir(t), "sb_1:work")
	if code != ExitOK {
		t.Fatalf("cp -r into a home-relative path = %d out=%q stderr=%q", code, out, errS)
	}
	if !strings.Contains(out, "unpacked") {
		t.Errorf("out = %q, want the unpacked report", out)
	}
	if len(wrote) != 1 || !strings.HasPrefix(wrote[0], "work/") {
		t.Errorf("the archive was written to %v, want it inside the destination", wrote)
	}
	if len(ran) != 1 || strings.Contains(ran[0], "mkdir") {
		t.Errorf("exec ran %v, want one untar and no mkdir", ran)
	}
}
