package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type teamHit struct{ method, path, auth, org, body string }

// teamPortal answers the device flow, approving on the second poll, and records team calls.
func teamPortal(t *testing.T, hits *[]teamHit) *httptest.Server {
	t.Helper()
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cli/device/start":
			_, _ = w.Write([]byte(`{"device_code":"dev1","user_code":"ABCD-EFGH","verification_uri":"http://p/dashboard/#/cli?code=ABCD-EFGH","expires_in":600,"interval":0}`))
		case "/cli/device/token":
			raw, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(raw), `"dev1"`) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"expired_token"}`))
				return
			}
			if polls.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"session_token":"pdcs.session","email":"a@example.com","expires_in":28800}`))
		default:
			raw, _ := io.ReadAll(r.Body)
			*hits = append(*hits, teamHit{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-PD-Org"), string(raw)})
			_, _ = w.Write([]byte(`{"orgs":[{"id":"o:1","name":"Acme","role":"owner"}],"members":[],"invites":[]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func teamCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("SANDFORGE_API_URL", "")
	t.Setenv("SANDFORGE_API_KEY", "")
	var out, errb strings.Builder
	code := Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestTeamLoginStoresASessionAndUsesIt(t *testing.T) {
	t.Setenv("SANDFORGE_CONFIG", t.TempDir()+"/config.json")
	var hits []teamHit
	srv := teamPortal(t, &hits)

	code, out, errS := teamCLI(t, "--json", "team", "login", "--portal", srv.URL)
	if code != ExitOK || !strings.Contains(out, "a@example.com") {
		t.Fatalf("login = %d %s %s", code, out, errS)
	}
	if !strings.Contains(errS, "ABCD-EFGH") || !strings.Contains(errS, "/dashboard/#/cli") {
		t.Fatalf("login did not show the code and link: %s", errS)
	}
	if strings.Contains(out+errS, "pdcs.session") {
		t.Fatalf("login printed the session: %s %s", out, errS)
	}
	cfg, err := LoadConfig()
	if err != nil || cfg.TeamSession != "pdcs.session" || cfg.TeamPortal != srv.URL || cfg.TeamEmail != "a@example.com" {
		t.Fatalf("config = %+v, %v", cfg, err)
	}

	for _, tc := range []struct {
		args               []string
		method, path, body string
		org                string
	}{
		{[]string{"team", "orgs"}, "GET", "/v1/orgs", "", ""},
		{[]string{"team", "org", "create", "Acme"}, "POST", "/v1/orgs", `"name":"Acme"`, ""},
		{[]string{"team", "members", "--org", "o:1"}, "GET", "/v1/members", "", "o:1"},
		{[]string{"team", "invite", "b@example.com", "--role", "admin", "--org", "o:1"}, "POST", "/v1/members", `"email":"b@example.com"`, "o:1"},
		{[]string{"team", "uninvite", "b@example.com", "--org", "o:1", "--confirm"}, "DELETE", "/v1/members/invites/b@example.com", "", "o:1"},
		{[]string{"team", "role", "user_2", "admin", "--org", "o:1"}, "PATCH", "/v1/members/user_2", `"role":"admin"`, "o:1"},
		{[]string{"team", "remove", "user_2", "--org", "o:1", "--confirm"}, "DELETE", "/v1/members/user_2", "", "o:1"},
	} {
		hits = nil
		code, _, errS := teamCLI(t, append([]string{"--json"}, tc.args...)...)
		if code != ExitOK {
			t.Errorf("%v = %d %s", tc.args, code, errS)
			continue
		}
		if len(hits) != 1 {
			t.Errorf("%v made %d calls", tc.args, len(hits))
			continue
		}
		got := hits[0]
		if got.method != tc.method || got.path != tc.path || got.auth != "Bearer pdcs.session" || got.org != tc.org || !strings.Contains(got.body, tc.body) {
			t.Errorf("%v sent %+v", tc.args, got)
		}
	}

	if code, _, _ := teamCLI(t, "--json", "team", "remove", "user_2"); code != ExitUsage {
		t.Errorf("remove without --confirm = %d", code)
	}
	if code, _, errS := teamCLI(t, "--json", "team", "logout"); code != ExitOK {
		t.Fatalf("logout = %d %s", code, errS)
	}
	if code, _, errS := teamCLI(t, "--json", "team", "orgs"); code == ExitOK || !strings.Contains(errS, "team login") {
		t.Errorf("after logout = %d %s", code, errS)
	}
}

func TestTeamLoginInTwoStepsForAnAgent(t *testing.T) {
	t.Setenv("SANDFORGE_CONFIG", t.TempDir()+"/config.json")
	var hits []teamHit
	srv := teamPortal(t, &hits)

	code, out, errS := teamCLI(t, "--json", "team", "login", "--portal", srv.URL, "--no-wait")
	if code != ExitOK {
		t.Fatalf("start = %d %s", code, errS)
	}
	var started map[string]any
	if err := json.Unmarshal([]byte(out), &started); err != nil || started["device_code"] != "dev1" || started["verification_uri"] == nil {
		t.Fatalf("--no-wait printed %s: %v", out, err)
	}
	if code, _, errS := teamCLI(t, "--json", "team", "login", "--portal", srv.URL, "--device-code", "dev1"); code != ExitOK {
		t.Fatalf("finish = %d %s", code, errS)
	}
	if cfg, _ := LoadConfig(); cfg.TeamSession != "pdcs.session" {
		t.Fatalf("session not stored: %+v", cfg)
	}
	if code, _, errS := teamCLI(t, "--json", "team", "login", "--portal", srv.URL, "--device-code", "wrong"); code == ExitOK || !strings.Contains(errS, "did not finish") {
		t.Fatalf("a wrong device code = %d %s", code, errS)
	}
}

func TestTeamLoginKeepsTheAPIKey(t *testing.T) {
	t.Setenv("SANDFORGE_CONFIG", t.TempDir()+"/config.json")
	if err := SaveConfig(Config{APIURL: "http://api", Key: "k1"}); err != nil {
		t.Fatal(err)
	}
	var hits []teamHit
	srv := teamPortal(t, &hits)
	if code, _, errS := teamCLI(t, "--json", "team", "login", "--portal", srv.URL); code != ExitOK {
		t.Fatalf("login = %d %s", code, errS)
	}
	teamCLI(t, "--json", "team", "logout")
	if cfg, _ := LoadConfig(); cfg.Key != "k1" || cfg.TeamSession != "" {
		t.Fatalf("config = %+v", cfg)
	}
}
