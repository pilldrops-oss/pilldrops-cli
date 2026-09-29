package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// recordingExecPortal keeps the last exec body a test sent.
func recordingExecPortal(t *testing.T, got *map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/sb_1/exec", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = nil
		_ = json.Unmarshal(b, got)
		_, _ = w.Write([]byte(`{"stdout":"ok","exit_code":0}`))
	})
	return httptest.NewServer(mux)
}

// `sleep 60 | pilldrops exec <sb> -- echo E` exited 124 on a real node: the
// client drained stdin to EOF before sending the request, and a pipe nobody is
// writing to does not reach EOF until the writer goes. Every CI step, pipeline
// wrapper and `ssh host 'pilldrops exec …'` without -n hung the same way, on a
// command that wanted no input at all.
//
// The real pipe is the test. A strings.Reader reaches EOF immediately, which is
// exactly the case that never broke.
func TestAPipeWithNoWriterDoesNotHoldExec(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Held open and never written to, like the `sleep 60` end of the pipeline.
	defer w.Close()
	defer r.Close()

	srv := fakeExecPortal(t)
	defer srv.Close()
	cpEnv(t, srv.URL)

	stdin = r
	defer func() { stdin = os.Stdin }()

	done := make(chan int, 1)
	go func() {
		var out, errb bytes.Buffer
		done <- Run([]string{"exec", "sb_1", "--", "echo", "hi"}, &out, &errb)
	}()
	select {
	case code := <-done:
		if code != ExitOK {
			t.Fatalf("exec = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exec is still waiting on a pipe that will never close; the command was never sent")
	}
}

// --stdin is how a caller says the command wants the input. Nothing on this
// side can tell a slow writer from one that will never write, so the rule is
// that the caller says.
func TestStdinFlagStillPipesInput(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	go func() {
		_, _ = w.WriteString(`{"a":1}`)
		w.Close()
	}()

	var got map[string]any
	srv := recordingExecPortal(t, &got)
	defer srv.Close()
	cpEnv(t, srv.URL)

	stdin = r
	defer func() { stdin = os.Stdin }()
	var out, errb bytes.Buffer
	if code := Run([]string{"exec", "sb_1", "--stdin", "--", "cat"}, &out, &errb); code != ExitOK {
		t.Fatalf("exec --stdin = %d: %s", code, errb.String())
	}
	if _, ok := got["stdin"]; !ok {
		t.Fatalf("--stdin sent no input: %v", got)
	}
}

// A redirect from a file is bounded: it ends on its own, so it needs no flag
// and `pilldrops exec <sb> -- cat < in.txt` keeps working as it always has.
func TestARedirectedFileIsStillReadWithoutTheFlag(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "in")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("from a file"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var got map[string]any
	srv := recordingExecPortal(t, &got)
	defer srv.Close()
	cpEnv(t, srv.URL)

	stdin = f
	defer func() { stdin = os.Stdin }()
	var out, errb bytes.Buffer
	if code := Run([]string{"exec", "sb_1", "--", "cat"}, &out, &errb); code != ExitOK {
		t.Fatalf("exec = %d: %s", code, errb.String())
	}
	if _, ok := got["stdin"]; !ok {
		t.Fatalf("a redirected file was not sent: %v", got)
	}
}
