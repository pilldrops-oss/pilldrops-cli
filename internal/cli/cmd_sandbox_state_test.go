package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The list has always answered with `state` — the daemon's own field name, and
// what both list paths emit — while create and get-by-id add `status` as an
// alias on top. The table read only the alias, so `sandbox ls` printed an empty
// STATUS column for every sandbox it has ever listed, with no error anywhere.
func TestTheStatusColumnIsFilledFromWhicheverNameTheAPIUsed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sandboxes":[` +
			`{"id":"sb_1","template":"base","state":"running","visibility":"org"},` +
			`{"id":"sb_2","template":"base","status":"paused","visibility":"org"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cpEnv(t, srv.URL)

	var out, errb bytes.Buffer
	if code := Run([]string{"sandbox", "ls"}, &out, &errb); code != ExitOK {
		t.Fatalf("sandbox ls = %d: %s", code, errb.String())
	}
	for _, want := range []string{"running", "paused"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("STATUS column has no %q:\n%s", want, out.String())
		}
	}
}

// The help has always advertised `sandbox open <id> [--print]`. That order was
// a usage error: flag stops at the first non-flag argument, so the id was taken
// as the end of the flags and --print was never set.
func TestOpenTakesItsFlagOnEitherSideOfTheID(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/sb_1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"sb_1","url":"http://cp.example/preview/sb_1/8000/"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cpEnv(t, srv.URL)

	opened := 0
	openBrowser = func(string) error { opened++; return nil }

	for _, args := range [][]string{
		{"sandbox", "open", "sb_1", "--print"},
		{"sandbox", "open", "--print", "sb_1"},
	} {
		var out, errb bytes.Buffer
		if code := Run(args, &out, &errb); code != ExitOK {
			t.Fatalf("%v = %d: %s", args, code, errb.String())
		}
		if !strings.Contains(out.String(), "/preview/sb_1/8000/") {
			t.Errorf("%v printed %q", args, strings.TrimSpace(out.String()))
		}
	}
	if opened != 0 {
		t.Errorf("--print opened a browser %d times", opened)
	}
}
