package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// domainAPI is a portal whose one host goes active after `flips` polls, or
// never when flips < 0.
func domainAPI(t *testing.T, flips int, errText string) *httptest.Server {
	t.Helper()
	polls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/domains", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var in map[string]any
		_ = json.Unmarshal(b, &in)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"host": in["host"], "sandbox_id": in["sandbox_id"], "port": 8000, "status": "pending",
			"cname_target": "8000-sb1.pilldrops.com",
			"record":       in["host"].(string) + " CNAME 8000-sb1.pilldrops.com",
		})
	})
	mux.HandleFunc("GET /v1/domains", func(w http.ResponseWriter, r *http.Request) {
		polls++
		status, detail := "pending", errText
		if flips >= 0 && polls >= flips {
			status, detail = "active", ""
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"domains": []map[string]any{{
			"host": "app.example.com", "sandbox_id": "sb1", "port": 8000,
			"status": status, "error": detail,
			"record": "app.example.com CNAME 8000-sb1.pilldrops.com",
		}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func domainEnv(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_URL", srv.URL)
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")
	prevFor, prevEvery := domainPollFor, domainPollEvery
	domainPollFor, domainPollEvery = 2*time.Second, 10*time.Millisecond
	t.Cleanup(func() { domainPollFor, domainPollEvery = prevFor, prevEvery })
}

func TestDomainAddWaitsForTheRecord(t *testing.T) {
	domainEnv(t, domainAPI(t, 2, ""))

	var out, errb bytes.Buffer
	if code := Run([]string{"domain", "add", "app.example.com", "--sandbox", "sb1", "--confirm"}, &out, &errb); code != ExitOK {
		t.Fatalf("add = %d, want 0 (stderr %q)", code, errb.String())
	}
	if !strings.Contains(out.String(), "app.example.com CNAME 8000-sb1.pilldrops.com") {
		t.Errorf("output does not show the record to create: %q", out.String())
	}
	if !strings.Contains(out.String(), "is live") {
		t.Errorf("output does not confirm the host went live: %q", out.String())
	}
}

// A record that never appears must fail the command, with the resolver's own
// reason: a script that saw exit 0 would go on to point traffic at a dead host.
func TestDomainAddFailsWhenTheRecordNeverAppears(t *testing.T) {
	domainEnv(t, domainAPI(t, -1, "app.example.com points at 203.0.113.1, not at 8000-sb1.pilldrops.com"))

	var out, errb bytes.Buffer
	code := Run([]string{"domain", "add", "app.example.com", "--sandbox", "sb1", "--confirm"}, &out, &errb)
	if code == ExitOK {
		t.Fatalf("add = 0 for a host that never verified (stdout %q)", out.String())
	}
	if !strings.Contains(errb.String(), "points at 203.0.113.1") {
		t.Errorf("stderr %q does not carry the resolver's reason", errb.String())
	}
}

func TestDomainListAndRemove(t *testing.T) {
	deleted := ""
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/domains", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"domains":[{"host":"app.example.com","sandbox_id":"sb1","port":8000,"status":"active","error":""}]}`))
	})
	mux.HandleFunc("DELETE /v1/domains/{host}", func(w http.ResponseWriter, r *http.Request) {
		deleted = r.PathValue("host")
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	domainEnv(t, srv)

	var out, errb bytes.Buffer
	if code := Run([]string{"domain", "list"}, &out, &errb); code != ExitOK {
		t.Fatalf("list = %d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "app.example.com") || !strings.Contains(out.String(), "active") {
		t.Errorf("listing = %q", out.String())
	}
	out.Reset()
	if code := Run([]string{"domain", "rm", "app.example.com"}, &out, &errb); code != ExitOK {
		t.Fatalf("rm = %d %s", code, errb.String())
	}
	if deleted != "app.example.com" {
		t.Errorf("deleted %q", deleted)
	}
}
