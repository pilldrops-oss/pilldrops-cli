package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// env scope sends the new scope and nothing else: the value is not re-sent, so
// a scope change cannot rotate or truncate a credential.
func TestEnvScopeSendsOnlyTheScope(t *testing.T) {
	var body map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"name":"API_KEY","scope":"private"}`))
	}))
	defer srv.Close()

	code, out, errS := authedCLI(t, srv, "env", "scope", "API_KEY", "private")
	if code != ExitOK {
		t.Fatalf("env scope = %d stderr=%s", code, errS)
	}
	if path != "/v1/secrets/API_KEY/scope" {
		t.Errorf("posted to %q", path)
	}
	if body["scope"] != "private" || len(body) != 1 {
		t.Errorf("sent %v, want only the scope", body)
	}
	if !strings.Contains(out, "private") {
		t.Errorf("output = %q, want the new scope", out)
	}
}

func TestEnvListFiltersByScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"A","kind":"env","value":"1","scope":"all"},` +
			`{"name":"B","kind":"secret","last4":"abcd","scope":"private"}]`))
	}))
	defer srv.Close()

	// Count data rows, not substrings: "NAME" in the header contains an "A".
	names := func(out string) []string {
		var got []string
		for _, line := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
			got = append(got, strings.Fields(line)[0])
		}
		return got
	}

	code, out, _ := authedCLI(t, srv, "env", "list", "--scope", "private")
	if code != ExitOK {
		t.Fatalf("env list --scope = %d", code)
	}
	if got := names(out); len(got) != 1 || got[0] != "B" {
		t.Errorf("--scope private listed %v, want just B:\n%s", got, out)
	}
	if code, out, _ = authedCLI(t, srv, "env", "list"); code != ExitOK || len(names(out)) != 2 {
		t.Errorf("unfiltered list = %d, rows %v:\n%s", code, names(out), out)
	}
}
