package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tarNames(t *testing.T, gzipped []byte) map[string]tar.Header {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(gzipped))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]tar.Header{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names[h.Name] = *h
	}
}

func dockerDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Dockerfile", "src/app.py"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The context the server receives, read back from the request itself.
func TestImageBuildSendsDockerfileAtTheRoot(t *testing.T) {
	dir := dockerDir(t)
	if err := os.Symlink("src/app.py", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	var sent []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"ok":true,"tag":"t","nodes":[{}]}`))
	}))
	defer srv.Close()

	code, _, errS := authedCLI(t, srv, "image", "build", dir, "-t", "n")
	if code != ExitOK {
		t.Fatalf("build = %d stderr=%s", code, errS)
	}
	names := tarNames(t, sent)
	for _, want := range []string{"Dockerfile", "src/app.py"} {
		if _, ok := names[want]; !ok {
			t.Errorf("no %q in the context: %v", want, names)
		}
	}
	if _, nested := names["proj/Dockerfile"]; nested {
		t.Error("the context is nested under the directory's own name")
	}
	if l := names["link"]; l.Linkname != "src/app.py" {
		t.Errorf("symlink target = %q", l.Linkname)
	}
}

// cp needs the opposite shape, and scp semantics depend on it.
func TestCpTarStaysNested(t *testing.T) {
	data, err := tarDir(dockerDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tarNames(t, data)["proj/Dockerfile"]; !ok {
		t.Fatalf("cp's tar lost its top-level directory")
	}
}

func TestImageBuildFailureShowsTheReason(t *testing.T) {
	report := `{"ok":false,"tag":"t","log":"step 1\nno such file","build":{"node":"n1","ok":false,"error":"exit status 1"},"publish":{"error":"not attempted"}}`
	for _, status := range []int{422, 502, 200} { // 200 is what an older server answers
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(report))
		}))
		dir := dockerDir(t)

		code, out, errS := authedCLI(t, srv, "image", "build", dir, "-t", "n")
		if code != ExitAPI || out != "" {
			t.Errorf("%d: exit %d stdout %q", status, code, out)
		}
		for _, want := range []string{"no such file", "exit status 1"} {
			if !strings.Contains(errS, want) {
				t.Errorf("%d: stderr %q lacks %q", status, errS, want)
			}
		}

		code, out, _ = authedCLI(t, srv, "--json", "image", "build", dir, "-t", "n")
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Errorf("%d: --json stdout %q: %v", status, out, err)
		}
		if code != ExitAPI || got["ok"] != false || got["log"] == nil {
			t.Errorf("%d: --json exit %d body %v", status, code, got)
		}
		srv.Close()
	}
}

// The top-level error the server is about to add outranks the steps.
func TestImageBuildReasonOrder(t *testing.T) {
	r := imageBuildResult{Error: "top", Build: buildStep{Error: "build"}, Nodes: []buildStep{{Error: "node"}}}
	if r.reason() != "top" {
		t.Fatal(r.reason())
	}
	if got := (imageBuildResult{Nodes: []buildStep{{Error: "node"}}}).reason(); got != "node" {
		t.Fatal(got)
	}
}

// A refusal that is not a build report keeps the ordinary contract.
func TestImageBuildOtherRefusalsStayErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"invalid API key","code":"unauthorized"}`))
	}))
	defer srv.Close()
	code, out, errS := authedCLI(t, srv, "--json", "image", "build", dockerDir(t), "-t", "n")
	if code != ExitAuth || out != "" || !strings.Contains(errS, "unauthorized") {
		t.Fatalf("exit %d stdout %q stderr %q", code, out, errS)
	}
}
