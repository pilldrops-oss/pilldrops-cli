package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAnIDThatEscapingLeavesAsAPathStepIsRefused(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/capabilities" {
			w.Write([]byte(`{"features":["webhook","agent"]}`))
			return
		}
		calls.Add(1)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	for _, args := range [][]string{
		{"webhook", "rm", "..", "--confirm"},
		{"webhook", "rm", ".", "--confirm"},
		{"webhook", "rm", "", "--confirm"},
		{"webhook", "test", ".."},
		{"agent", "webhook", "rm", "sb_1", "..", "--confirm"},
	} {
		calls.Store(0)
		code, _, errS := authedCLI(t, srv, args...)
		if code != ExitUsage || calls.Load() != 0 {
			t.Errorf("%q: exit %d with %d API calls, want %d and none", args, code, calls.Load(), ExitUsage)
		}
		if !strings.Contains(errS, "not a valid id") {
			t.Errorf("%q: stderr %q does not say why", args, errS)
		}
	}
	if code, _, errS := authedCLI(t, srv, "webhook", "rm", "a..b", "--confirm"); code != ExitOK {
		t.Errorf("an id containing dots was refused: %d %s", code, errS)
	}
}
