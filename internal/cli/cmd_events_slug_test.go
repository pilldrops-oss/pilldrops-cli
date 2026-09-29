package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// eventsPortal serves the events feed plus the two routes a slug resolves
// through. Events carry only the canonical id, as the platform's records do.
func eventsPortal(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"events":[
			{"at":1700000000,"sandbox_id":"sb_1","event":"created","template":"base"},
			{"at":1700000100,"sandbox_id":"sb_other","event":"created","template":"base"}]}`))
	})
	mux.HandleFunc("GET /v1/sandboxes/my-api", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"sb_1","slug":"my-api","status":"running"}`))
	})
	mux.HandleFunc("GET /v1/history/gone-api", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"sb_1","slug":"gone-api"}`))
	})
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// The help says a slug works wherever an id does. Against the old code this
// fails: the filter compared the slug to the record's canonical sandbox_id,
// matched nothing, and printed a bare header row with exit 0 — which reads as
// "this sandbox did nothing".
func TestEventsAcceptsASlug(t *testing.T) {
	srv := eventsPortal(t)
	for _, name := range []string{"my-api", "gone-api", "sb_1"} {
		code, out, errS := authedCLI(t, srv, "events", name)
		if code != ExitOK {
			t.Fatalf("events %s = %d stderr=%s", name, code, errS)
		}
		if !strings.Contains(out, "created") {
			t.Errorf("events %s printed no events: %q", name, out)
		}
		if strings.Contains(out, "sb_other") {
			t.Errorf("events %s leaked another sandbox's events: %q", name, out)
		}
	}
}

// A filter that matched nothing must say so rather than print a header and exit 0.
func TestEventsWithNoMatchSaysSo(t *testing.T) {
	srv := eventsPortal(t)
	code, _, errS := authedCLI(t, srv, "events", "sb_nothing")
	if code != ExitOK {
		t.Fatalf("events = %d, want 0: nothing failed", code)
	}
	if !strings.Contains(errS, "no events for sb_nothing") {
		t.Errorf("an empty result said nothing; stderr = %q", errS)
	}
}
