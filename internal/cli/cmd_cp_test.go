package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeFilesAndExec records file uploads and exec bodies; storedBytesDelta lets
// a test make the server under-report the byte count.
func fakeFilesAndExec(t *testing.T, stored map[string][]byte, execs *[]string, storedBytesDelta int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/sb_1/files", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		stored[r.URL.Query().Get("path")] = b
		w.Write([]byte(`{"path":"x","bytes":` + strconv.Itoa(len(b)+storedBytesDelta) + `}`))
	})
	mux.HandleFunc("GET /v1/sandboxes/sb_1/files", func(w http.ResponseWriter, r *http.Request) {
		w.Write(stored[r.URL.Query().Get("path")])
	})
	mux.HandleFunc("POST /v1/sandboxes/sb_1/exec", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*execs = append(*execs, string(b))
		w.Write([]byte(`{"stdout":"FILE\n","exit_code":0}`))
	})
	return httptest.NewServer(mux)
}

func cpEnv(t *testing.T, url string) {
	t.Helper()
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_URL", url)
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")
}

func TestCpShortWriteIsAnError(t *testing.T) {
	stored := map[string][]byte{}
	var execs []string
	srv := fakeFilesAndExec(t, stored, &execs, -1)
	defer srv.Close()
	cpEnv(t, srv.URL)
	local := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(local, []byte("hello"), 0o644)
	var out, errb bytes.Buffer
	if code := Run([]string{"cp", local, "sb_1:f"}, &out, &errb); code == ExitOK {
		t.Fatal("a short write exited 0")
	}
	if s := errb.String(); !strings.Contains(s, "5") || !strings.Contains(s, "4") {
		t.Errorf("stderr must name both counts: %s", s)
	}
}

func TestCpStdinStdoutAndHome(t *testing.T) {
	stored := map[string][]byte{}
	var execs []string
	srv := fakeFilesAndExec(t, stored, &execs, 0)
	defer srv.Close()
	cpEnv(t, srv.URL)
	stdin = strings.NewReader("abc")
	defer func() { stdin = os.Stdin }()
	var out, errb bytes.Buffer
	if code := Run([]string{"cp", "-", "sb_1:~/x"}, &out, &errb); code != ExitOK {
		t.Fatalf("stdin upload = %d %s", code, errb.String())
	}
	if string(stored["x"]) != "abc" {
		t.Fatalf("stored = %v (want key x with body abc: ~/x resolves to home)", stored)
	}
	out.Reset()
	if code := Run([]string{"cp", "sb_1:~/x", "-"}, &out, &errb); code != ExitOK || out.String() != "abc" {
		t.Fatalf("stdout download = %d %q %s", code, out.String(), errb.String())
	}
}

func TestCpDirectoryNeedsRAndLandsUnderParent(t *testing.T) {
	stored := map[string][]byte{}
	var execs []string
	srv := fakeFilesAndExec(t, stored, &execs, 0)
	defer srv.Close()
	cpEnv(t, srv.URL)
	dir := filepath.Join(t.TempDir(), "proj")
	_ = os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "sub", "a.txt"), []byte("a"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644)

	var out, errb bytes.Buffer
	if code := Run([]string{"cp", dir, "sb_1:."}, &out, &errb); code != ExitUsage || !strings.Contains(errb.String(), "-r") {
		t.Fatalf("directory without -r = %d %q, want usage error naming -r", code, errb.String())
	}
	if code := Run([]string{"cp", "-r", dir, "sb_1:."}, &out, &errb); code != ExitOK {
		t.Fatalf("cp -r = %d %s", code, errb.String())
	}
	if len(execs) != 1 || !strings.Contains(execs[0], `-C \".\"`) {
		t.Errorf("untar must extract with the destination as parent: %v", execs)
	}
	var archive []byte
	for _, b := range stored {
		archive = b
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
		if hdr.Name != "proj" && !strings.HasPrefix(hdr.Name, "proj/") {
			t.Errorf("entry %q is not under proj/", hdr.Name)
		}
	}
	if len(names) != 4 { // proj, proj/b.txt, proj/sub, proj/sub/a.txt
		t.Errorf("entries = %v", names)
	}
}

func fakeFilesPortal(t *testing.T, stored map[string][]byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/sb_1/files", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		stored[r.URL.Query().Get("path")] = b
		w.Write([]byte(`{"path":"` + r.URL.Query().Get("path") + `","bytes":` + strconv.Itoa(len(b)) + `}`))
	})
	mux.HandleFunc("GET /v1/sandboxes/sb_1/files", func(w http.ResponseWriter, r *http.Request) {
		b, ok := stored[r.URL.Query().Get("path")]
		if !ok {
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"no such file"}`))
			return
		}
		w.Write(b)
	})
	return httptest.NewServer(mux)
}

func TestCpUploadAndDownload(t *testing.T) {
	stored := map[string][]byte{}
	srv := fakeFilesPortal(t, stored)
	defer srv.Close()
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_URL", srv.URL)
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")

	dir := t.TempDir()
	local := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(local, []byte("hello sandbox"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"cp", local, "sb_1:/app/hello.txt"}, &out, &errb); code != ExitOK {
		t.Fatalf("upload = %d stderr=%s", code, errb.String())
	}
	if string(stored["/app/hello.txt"]) != "hello sandbox" {
		t.Fatalf("stored = %q", stored["/app/hello.txt"])
	}

	dst := filepath.Join(dir, "back.txt")
	if code := Run([]string{"cp", "sb_1:/app/hello.txt", dst}, &out, &errb); code != ExitOK {
		t.Fatalf("download = %d stderr=%s", code, errb.String())
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "hello sandbox" {
		t.Fatalf("downloaded = %q", got)
	}
}

func TestCpUsageErrors(t *testing.T) {
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")
	t.Setenv("SANDFORGE_API_URL", "http://unused.invalid")
	var out, errb bytes.Buffer
	if code := Run([]string{"cp", "a.txt", "b.txt"}, &out, &errb); code != ExitUsage {
		t.Fatalf("local-to-local = %d, want %d", code, ExitUsage)
	}
	if code := Run([]string{"cp", "sb_1:/a", "sb_1:/b"}, &out, &errb); code != ExitUsage {
		t.Fatalf("sandbox-to-sandbox = %d, want %d", code, ExitUsage)
	}
}
