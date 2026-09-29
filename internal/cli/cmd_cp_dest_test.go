package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `cp -r ./x sb:/root/` said "uploaded root/updir/" and exited 0, and the
// directory landed in /root/root/updir. `cp -r ./x sb:/tmp/dest` landed in
// /root/tmp/dest. The leading slash was stripped on its way to the untar, so
// every absolute destination was silently rewritten as home-relative — and
// reported as the write the caller asked for.
func TestAnAbsoluteDirectoryDestinationIsTheOneWrittenTo(t *testing.T) {
	for _, tc := range []struct{ dest, wantParent, wantSaid string }{
		{"/root/", "/root/", "/root/updir"},
		{"/tmp/dest", "/tmp/dest", "/tmp/dest/updir"},
		{"proj", "proj", "proj/updir"}, // relative stays home-relative
		{"", ".", "updir"},             // the home directory
	} {
		stored := map[string][]byte{}
		var execs []string
		srv := fakeFilesAndExec(t, stored, &execs, 0)
		cpEnv(t, srv.URL)
		dir := filepath.Join(t.TempDir(), "updir")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644); err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		if code := Run([]string{"cp", "-r", dir, "sb_1:" + tc.dest}, &out, &errb); code != ExitOK {
			t.Fatalf("cp -r to %q = %d: %s", tc.dest, code, errb.String())
		}
		srv.Close()
		if len(execs) != 1 {
			t.Fatalf("cp -r to %q ran %d exec calls, want the one untar", tc.dest, len(execs))
		}
		// The untar's -C is where the bytes actually land.
		if !strings.Contains(execs[0], `-C \"`+tc.wantParent+`\"`) &&
			!strings.Contains(execs[0], `-C \"`+tc.wantParent+`\"`) {
			t.Errorf("cp -r to %q untars into something else: %s", tc.dest, execs[0])
		}
		// And what it says it did has to be where it did it.
		if said := out.String(); !strings.Contains(said, tc.wantSaid) {
			t.Errorf("cp -r to %q reported %q, want it to name %s", tc.dest, strings.TrimSpace(said), tc.wantSaid)
		}
	}
}

// `cp f.txt sb:/tmp/` answered "invalid path (HTTP 503)" with the hint
// "server-side error — retry": a client input error reported as a server fault,
// with advice that could never work. A trailing slash is cp's own way of saying
// "into that directory", so the file keeps its name.
func TestAFileCopiedIntoADirectoryKeepsItsName(t *testing.T) {
	for _, tc := range []struct{ dest, want string }{
		{"/tmp/", "/tmp/f.txt"},
		{"", "f.txt"},                // the home directory
		{"~", "f.txt"},               //
		{"/tmp/other", "/tmp/other"}, // a name, not a directory: left alone
	} {
		stored := map[string][]byte{}
		var execs []string
		srv := fakeFilesAndExec(t, stored, &execs, 0)
		cpEnv(t, srv.URL)
		local := filepath.Join(t.TempDir(), "f.txt")
		if err := os.WriteFile(local, []byte("hello"), 0o644); err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		if code := Run([]string{"cp", local, "sb_1:" + tc.dest}, &out, &errb); code != ExitOK {
			t.Fatalf("cp to %q = %d: %s", tc.dest, code, errb.String())
		}
		srv.Close()
		if _, ok := stored[tc.want]; !ok {
			got := make([]string, 0, len(stored))
			for k := range stored {
				got = append(got, k)
			}
			t.Errorf("cp to %q wrote %v, want %q", tc.dest, got, tc.want)
		}
	}
}
