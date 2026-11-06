package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// customerRoutes is what the API offers a customer, each with the command that
// reaches it. Run for real against a server that knows only that route, so a verb
// aiming at the wrong method or path fails here rather than on someone's first call.
// The verbs that predate this list have tests of their own.
var customerRoutes = []struct {
	route string
	args  []string
}{
	{"POST /v1/volumes", []string{"volume", "create", "data", "--size-mb", "256"}},
	{"GET /v1/volumes", []string{"volume", "list"}},
	{"GET /v1/volumes/{id}", []string{"volume", "get", "vol-1"}},
	{"DELETE /v1/volumes/{id}", []string{"volume", "rm", "vol-1", "--confirm"}},

	{"GET /v1/sandboxes/{id}/agent/status", []string{"agent", "status", "sb_1"}},
	{"GET /v1/sandboxes/{id}/agent/models", []string{"agent", "models", "sb_1"}},
	{"GET /v1/sandboxes/{id}/agent/budget", []string{"agent", "budget", "sb_1"}},
	{"PATCH /v1/sandboxes/{id}/agent/budget", []string{"agent", "budget", "sb_1", "--cap", "5"}},
	{"POST /v1/sandboxes/{id}/agent/turns", []string{"agent", "ask", "sb_1", "hello"}},
	{"GET /v1/sandboxes/{id}/agent/turns", []string{"agent", "turns", "sb_1"}},
	{"GET /v1/sandboxes/{id}/agent/turns/{turn}", []string{"agent", "turn", "sb_1", "t1"}},
	{"DELETE /v1/sandboxes/{id}/agent/turns/{turn}", []string{"agent", "dequeue", "sb_1", "t1", "--confirm"}},
	{"POST /v1/sandboxes/{id}/agent/turns/{turn}/interrupt", []string{"agent", "interrupt", "sb_1", "t1", "--grace", "2"}},
	{"GET /v1/sandboxes/{id}/agent/events", []string{"agent", "events", "sb_1"}},
	{"GET /v1/sandboxes/{id}/agent/threads", []string{"agent", "threads", "sb_1"}},
	{"GET /v1/sandboxes/{id}/agent/threads/{thread}", []string{"agent", "thread", "sb_1", "th1"}},
	{"GET /v1/sandboxes/{id}/agent/tokens", []string{"agent", "tokens", "sb_1"}},
	{"POST /v1/sandboxes/{id}/agent/tokens", []string{"agent", "token", "create", "sb_1", "--scope", "agent:read"}},
	{"DELETE /v1/sandboxes/{id}/agent/tokens/{tokenID}", []string{"agent", "token", "rm", "sb_1", "tok1", "--confirm"}},
	{"POST /v1/sandboxes/{id}/agent/tokens/{tokenID}/rotate", []string{"agent", "token", "rotate", "sb_1", "tok1"}},
	{"POST /v1/sandboxes/{id}/agent/share", []string{"agent", "share", "sb_1"}},
	{"GET /v1/sandboxes/{id}/agent/connect", []string{"agent", "connect", "sb_1"}},
	{"GET /v1/sandboxes/{id}/agent/isolation", []string{"agent", "isolation", "sb_1"}},
	{"GET /v1/sandboxes/{id}/agent/approvals", []string{"agent", "approvals", "sb_1"}},
	{"POST /v1/sandboxes/{id}/agent/approvals/{approvalID}", []string{"agent", "approve", "sb_1", "ap1", "--allow"}},
	{"GET /v1/sandboxes/{id}/agent/policy", []string{"agent", "policy", "sb_1"}},
	{"GET /v1/sandboxes/{id}/agent/webhooks", []string{"agent", "webhooks", "sb_1"}},
	{"POST /v1/sandboxes/{id}/agent/webhooks", []string{"agent", "webhook", "add", "sb_1", "https://example.test/h", "--event", "turn.done"}},
	{"DELETE /v1/sandboxes/{id}/agent/webhooks/{hookID}", []string{"agent", "webhook", "rm", "sb_1", "h1", "--confirm"}},
	{"POST /v1/sandboxes/{id}/agent/webhooks/{hookID}/test", []string{"agent", "webhook", "test", "sb_1", "h1"}},

	{"GET /v1/cron", []string{"cron", "list"}},
	{"POST /v1/cron", []string{"cron", "create", "sb_1", "0 3 * * *", "true"}},
	{"DELETE /v1/cron/{id}", []string{"cron", "rm", "c1", "--confirm"}},
	{"POST /v1/cron/{id}/run", []string{"cron", "run", "c1"}},
	{"POST /v1/cron/{id}/toggle", []string{"cron", "toggle", "c1", "--off"}},

	{"GET /v1/functions", []string{"function", "list"}},
	{"POST /v1/functions", []string{"function", "create", "fn", "--start", "serve"}},
	{"GET /v1/functions/{name}", []string{"function", "get", "fn"}},
	{"GET /v1/functions/{name}/code", []string{"function", "code", "fn"}},
	{"GET /v1/functions/{name}/logs", []string{"function", "logs", "fn"}},
	{"GET /v1/functions/{name}/metrics", []string{"function", "metrics", "fn"}},
	{"DELETE /v1/functions/{name}", []string{"function", "rm", "fn", "--confirm"}},

	{"GET /v1/projects", []string{"project", "list"}},
	{"POST /v1/projects", []string{"project", "create", "p"}},
	{"GET /v1/projects/{id}", []string{"project", "get", "pr1"}},
	{"DELETE /v1/projects/{id}", []string{"project", "rm", "pr1", "--confirm"}},
	{"GET /v1/projects/{id}/runs", []string{"project", "runs", "pr1"}},
	{"POST /v1/projects/{id}/runs", []string{"project", "run", "pr1"}},

	{"GET /v1/threads", []string{"thread", "list"}},
	{"POST /v1/threads", []string{"thread", "create", "--title", "t"}},
	{"GET /v1/threads/{id}", []string{"thread", "get", "th1"}},
	{"DELETE /v1/threads/{id}", []string{"thread", "rm", "th1", "--confirm"}},

	{"GET /v1/integrations", []string{"integrations"}},
	{"GET /v1/sandboxes/{id}/integrations", []string{"sandbox", "integrations", "sb_1"}},
	{"GET /v1/sandboxes/{id}/chat", []string{"chat", "sb_1"}},
	{"POST /v1/sandboxes/{id}/chat", []string{"chat", "sb_1", "hello"}},
	{"POST /v1/sandboxes/{id}/chat-link", []string{"chat-link", "sb_1", "--read-only"}},
	{"DELETE /v1/sandboxes/{id}/chat-link", []string{"chat-link", "rm", "sb_1", "--confirm"}},
	{"GET /v1/sandboxes/{id}/channels", []string{"channels", "sb_1"}},
	{"GET /v1/audit", []string{"audit"}},

	// No typed verb: each must have its reason in skippedRoutes.
	{"POST /v1/sandboxes/{id}/agent/queue/reorder", nil},
	{"GET /v1/sandboxes/{id}/agent/share/qr", nil},
	{"GET /v1/sandboxes/{id}/agent/webhooks/failures", nil},
	{"DELETE /v1/sandboxes/{id}/agent/policy/remembered", nil},
	{"GET /v1/sandboxes/{id}/agent/claim", nil},
	{"POST /v1/sandboxes/{id}/agent/responses", nil},
	{"GET /v1/sandboxes/{id}/agent/responses/{response}", nil},
	{"GET /v1/sandboxes/{id}/agent/responses/{response}/stream", nil},
	{"POST /v1/sandboxes/{id}/agent/responses/{response}/cancel", nil},
	{"PUT /v1/functions/{name}", nil},
	{"PATCH /v1/projects/{id}", nil},
	{"PATCH /v1/projects/{id}/runs/{runID}", nil},
	{"PATCH /v1/threads/{id}", nil},
	{"POST /v1/sandboxes/{id}/integrations", nil},
	{"/v1/members", nil},
	{"/v1/orgs", nil},
}

// skippedRoutes have no typed verb on purpose; each says why. `pilldrops api`
// still reaches the customer ones.
var skippedRoutes = map[string]string{
	"POST /v1/sandboxes/{id}/agent/queue/reorder":               "reachable through api",
	"GET /v1/sandboxes/{id}/agent/share/qr":                     "renders in a browser",
	"GET /v1/sandboxes/{id}/agent/webhooks/failures":            "reachable through api",
	"DELETE /v1/sandboxes/{id}/agent/policy/remembered":         "reachable through api",
	"GET /v1/sandboxes/{id}/agent/claim":                        "a browser redeems it with no credential",
	"POST /v1/sandboxes/{id}/agent/responses":                   "OpenAI-compatible adapter, for OpenAI clients",
	"GET /v1/sandboxes/{id}/agent/responses/{response}":         "OpenAI-compatible adapter",
	"GET /v1/sandboxes/{id}/agent/responses/{response}/stream":  "OpenAI-compatible adapter",
	"POST /v1/sandboxes/{id}/agent/responses/{response}/cancel": "OpenAI-compatible adapter",
	"PUT /v1/functions/{name}":                                  "reachable through api",
	"PATCH /v1/projects/{id}":                                   "reachable through api",
	"PATCH /v1/projects/{id}/runs/{runID}":                      "reachable through api",
	"PATCH /v1/threads/{id}":                                    "reachable through api: an opaque transcript store",
	"POST /v1/sandboxes/{id}/integrations":                      "sandbox integrations add|rm use it, after a read",
	"/v1/members":                                               "team verbs, on a session: TestTeamLoginStoresASessionAndUsesIt",
	"/v1/orgs":                                                  "team verbs, on a session: TestTeamLoginStoresASessionAndUsesIt",
}

func TestEveryCustomerRouteHasAVerbOrAReason(t *testing.T) {
	for _, tc := range customerRoutes {
		t.Run(tc.route, func(t *testing.T) {
			why, skipped := skippedRoutes[tc.route]
			if tc.args == nil {
				if !skipped {
					t.Fatal("no typed verb and no entry in skippedRoutes")
				}
				return
			}
			if skipped {
				t.Fatalf("has a verb and is also skipped (%s)", why)
			}
			hit := false
			mux := http.NewServeMux()
			mux.HandleFunc(tc.route, func(w http.ResponseWriter, _ *http.Request) {
				hit = true
				_, _ = w.Write([]byte(`{"status":"ok","ok":true}`))
			})
			// Resolving a slug on the way to the route is the one other call allowed.
			mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"id":"sb_1"}`))
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			if code, _, errS := authedCLI(t, srv, tc.args...); code != ExitOK || !hit {
				t.Fatalf("%v: exit %d, route hit %v, stderr %s", tc.args, code, hit, errS)
			}
		})
	}
	// A reason for a route nobody lists is a route nobody is checking.
	listed := map[string]bool{}
	for _, tc := range customerRoutes {
		listed[tc.route] = true
	}
	for route := range skippedRoutes {
		if !listed[route] {
			t.Errorf("%s is skipped but not in customerRoutes", route)
		}
	}
}

// A new group shipping with no route in the list would have nothing checking it.
func TestEveryNewGroupIsInTheRouteList(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range customerRoutes {
		if tc.args != nil {
			covered[tc.args[0]] = true
		}
	}
	for _, g := range []string{"volume", "agent", "cron", "function", "project", "thread", "integrations", "chat", "chat-link", "channels", "audit"} {
		if !covered[g] {
			t.Errorf("group %q has no route in customerRoutes", g)
		}
	}
}

// Without the flag nothing but the capabilities read is sent, on every destructive verb declared here.
func TestNewDestructiveVerbsRequireConfirm(t *testing.T) {
	for _, tc := range customerRoutes {
		if tc.args == nil || tc.args[len(tc.args)-1] != "--confirm" {
			continue
		}
		args := tc.args[:len(tc.args)-1]
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/capabilities" {
					calls++
				}
			}))
			defer srv.Close()
			code, _, errS := authedCLI(t, srv, args...)
			if code != ExitUsage || calls != 0 || !strings.Contains(errS, "--confirm") {
				t.Fatalf("exit %d, %d calls, stderr %q", code, calls, errS)
			}
		})
	}
}

// --json is one value on stdout, even for a route answering 204.
func TestVerbsPrintOneJSONValue(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		args       []string
		want       string
	}{
		{"list is unwrapped", `{"volumes":[{"id":"vol-1"}]}`, 200, []string{"volume", "list"}, `[{"id":"vol-1"}]`},
		{"null list", `{"threads":null}`, 200, []string{"thread", "list"}, `[]`},
		{"envelope kept", `{"turns":[],"served_from":"node"}`, 200, []string{"agent", "turns", "sb_1"}, `{"turns":[],"served_from":"node"}`},
		{"no content", ``, 204, []string{"project", "rm", "p", "--confirm"}, `{"status":"ok"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			code, out, errS := authedCLI(t, srv, append([]string{"--json"}, tc.args...)...)
			if code != ExitOK {
				t.Fatalf("exit %d stderr %s", code, errS)
			}
			var got, want any
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("stdout %q: %v", out, err)
			}
			_ = json.Unmarshal([]byte(tc.want), &want)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("stdout = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

// Every refusal keeps the contract: code and status on stderr, exit 1 or 3.
func TestVerbsKeepTheErrorContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"sandbox not found","code":"agent_not_supported"}`))
	}))
	defer srv.Close()
	code, out, errS := authedCLI(t, srv, "--json", "agent", "status", "sb_1")
	if code != ExitAPI || out != "" {
		t.Fatalf("exit %d stdout %q", code, out)
	}
	var failure map[string]any
	if err := json.Unmarshal([]byte(errS), &failure); err != nil {
		t.Fatalf("stderr %q: %v", errS, err)
	}
	if failure["code"] != "agent_not_supported" || failure["status"] != float64(404) {
		t.Fatalf("failure = %v", failure)
	}

	code, _, errS = runCLI(t, srv.URL, "volume", "list")
	if code != ExitAuth {
		t.Fatalf("logged out = %d stderr %s", code, errS)
	}
	if code, _, _ := runCLI(t, srv.URL, "volume", "frobnicate"); code != ExitUsage {
		t.Fatalf("unknown sub = %d, want usage even when logged out", code)
	}
	if code, _, _ := runCLI(t, srv.URL, "agent", "status"); code != ExitUsage {
		t.Fatalf("missing id = %d, want usage without a terminal", code)
	}
}
