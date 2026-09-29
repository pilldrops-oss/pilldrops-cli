package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `fs ls <id> nope` exited 1 with "check the id with `pilldrops sandbox ls`" —
// advice about the one thing that had been right. The sandbox was found; the
// path was not. hint() sees only the status code, so a 404 from a filesystem
// verb has to carry its own remedy.
func TestAMissingPathIsNotReportedAsAMissingSandbox(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/sb_1/filesystem/list", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no such path in the sandbox: nope","code":"not_found"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cpEnv(t, srv.URL)

	var out, errb bytes.Buffer
	if code := Run([]string{"fs", "ls", "sb_1", "nope"}, &out, &errb); code != ExitAPI {
		t.Fatalf("fs ls on a missing path = %d, want %d", code, ExitAPI)
	}
	got := errb.String()
	if strings.Contains(got, "sandbox ls") {
		t.Errorf("a wrong path is still reported as a wrong sandbox id:\n%s", got)
	}
	if !strings.Contains(got, "path") {
		t.Errorf("the hint does not mention the path:\n%s", got)
	}
	// The platform's own code still has to reach the caller: a hint must not
	// cost the machine-readable half of the answer.
	if !strings.Contains(got, "not_found") {
		t.Errorf("the platform error code was dropped:\n%s", got)
	}
}
