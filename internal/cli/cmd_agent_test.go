package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fastAgentPolling(t *testing.T) {
	t.Helper()
	poll, idle := agentPollEvery, agentEventsIdle
	agentPollEvery, agentEventsIdle = time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { agentPollEvery, agentEventsIdle = poll, idle })
}

func agentTurnServer(t *testing.T, finalTurn string, sent *map[string]any) *httptest.Server {
	t.Helper()
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/sb_1/agent/turns", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(sent)
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"turn_id":"t1","state":"queued"}`))
	})
	mux.HandleFunc("GET /v1/sandboxes/sb_1/agent/turns/t1", func(w http.ResponseWriter, _ *http.Request) {
		if polls.Add(1) < 3 {
			_, _ = w.Write([]byte(`{"id":"t1","state":"running"}`))
			return
		}
		_, _ = w.Write([]byte(finalTurn))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestAgentAskSendsTheHandlersFields(t *testing.T) {
	var sent map[string]any
	srv := agentTurnServer(t, `{}`, &sent)
	code, out, errS := authedCLI(t, srv, "--json", "agent", "ask", "sb_1", "fix it",
		"--mode", "guided", "--thread", "new", "--priority")
	if code != ExitOK {
		t.Fatalf("exit %d stderr %s", code, errS)
	}
	if sent["message"] != "fix it" || sent["mode"] != "guided" || sent["thread"] != "new" || sent["priority"] != true {
		t.Fatalf("body = %v", sent)
	}
	if !strings.Contains(out, `"turn_id"`) {
		t.Fatalf("stdout = %s", out)
	}
	if code, _, _ := authedCLI(t, srv, "agent", "ask", "sb_1", "x", "--mode", "yolo"); code != ExitUsage {
		t.Fatalf("bad mode = %d", code)
	}
}

func TestAgentAskWait(t *testing.T) {
	fastAgentPolling(t)
	for _, tc := range []struct {
		name, turn string
		want       int
	}{
		{"done", `{"id":"t1","state":"done","response":"the answer"}`, ExitOK},
		{"failed", `{"id":"t1","state":"failed","response":"the answer","error":"boom"}`, ExitAPI},
		{"interrupted", `{"id":"t1","state":"interrupted","response":"the answer"}`, ExitAPI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent map[string]any
			srv := agentTurnServer(t, tc.turn, &sent)
			code, out, errS := authedCLI(t, srv, "agent", "ask", "sb_1", "x", "--wait")
			if code != tc.want || !strings.Contains(out, "the answer") {
				t.Fatalf("exit %d stdout %q stderr %q", code, out, errS)
			}
			srv = agentTurnServer(t, tc.turn, &sent)
			code, out, _ = authedCLI(t, srv, "--json", "agent", "ask", "sb_1", "x", "--wait")
			var turn map[string]any
			if err := json.Unmarshal([]byte(out), &turn); err != nil || code != tc.want || turn["response"] != "the answer" {
				t.Fatalf("--json exit %d stdout %q err %v", code, out, err)
			}
		})
	}
}

// The stream never ends on its own, so a run without -f has to stop by itself.
func TestAgentEventsStopsWhenQuietAndIsOneJSONValue(t *testing.T) {
	fastAgentPolling(t)
	var query atomic.Value
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query.Store(r.URL.RawQuery)
		fmt.Fprint(w, ": keepalive\n\nid: 5\ndata: {\"seq\":5,\"type\":\"turn.done\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	code, out, errS := authedCLI(t, srv, "--json", "agent", "events", "sb_1", "--since", "4")
	if code != ExitOK {
		t.Fatalf("exit %d stderr %s", code, errS)
	}
	var events []map[string]any
	if err := json.Unmarshal([]byte(out), &events); err != nil || len(events) != 1 || events[0]["type"] != "turn.done" {
		t.Fatalf("stdout %q err %v", out, err)
	}
	if query.Load() != "since_seq=4" {
		t.Fatalf("query = %v", query.Load())
	}
	if code, _, _ := authedCLI(t, srv, "--json", "agent", "events", "sb_1", "-f"); code != ExitUsage {
		t.Fatalf("-f --json = %d, want usage: a stream is not one value", code)
	}
}

func TestAgentEventsRefusalIsAnAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"sandbox not found"}`))
	}))
	defer srv.Close()
	if code, _, errS := authedCLI(t, srv, "agent", "events", "sb_1"); code != ExitAPI || !strings.Contains(errS, "sandbox not found") {
		t.Fatalf("exit %d stderr %q", code, errS)
	}
}

func TestAgentApproveNeedsExactlyOneDecision(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	for _, args := range [][]string{{"agent", "approve", "sb_1", "a1"}, {"agent", "approve", "sb_1", "a1", "--allow", "--deny"}} {
		if code, _, _ := authedCLI(t, srv, args...); code != ExitUsage {
			t.Fatalf("%v = %d", args, code)
		}
	}
	if code, _, errS := authedCLI(t, srv, "agent", "approve", "sb_1", "a1", "--deny"); code != ExitOK || body["decision"] != "deny" {
		t.Fatalf("exit %d body %v stderr %s", code, body, errS)
	}
}

func TestAgentTokenAndWebhookBodies(t *testing.T) {
	var body map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.Method + " " + r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"ok":false}`))
	}))
	defer srv.Close()

	if code, _, _ := authedCLI(t, srv, "agent", "token", "create", "my-slug"); code != ExitUsage {
		t.Fatalf("no scope = %d", code)
	}
	code, _, errS := authedCLI(t, srv, "agent", "token", "create", "my-slug", "--scope", "agent:read", "--scope", "agent:write", "--label", "ci", "--ttl", "60")
	if code != ExitOK {
		t.Fatalf("exit %d stderr %s", code, errS)
	}
	if path != "POST /v1/sandboxes/my-slug/agent/tokens" {
		t.Fatalf("a slug must pass through like an id: %s", path)
	}
	scopes, _ := body["scopes"].([]any)
	if len(scopes) != 2 || body["label"] != "ci" || body["ttl_seconds"] != float64(60) {
		t.Fatalf("body = %v", body)
	}

	if code, _, _ := authedCLI(t, srv, "agent", "webhook", "add", "sb_1", "https://example.test"); code != ExitUsage {
		t.Fatalf("no event = %d", code)
	}
	if code, _, errS := authedCLI(t, srv, "agent", "webhook", "add", "sb_1", "https://example.test", "--event", "turn.done"); code != ExitOK || body["url"] != "https://example.test" {
		t.Fatalf("exit %d body %v stderr %s", code, body, errS)
	}
	// The API call worked; the customer's endpoint did not.
	if code, _, _ := authedCLI(t, srv, "agent", "webhook", "test", "sb_1", "h1"); code != ExitAPI {
		t.Fatalf("webhook test ok:false = %d", code)
	}
}

// Only a path segment gets its slug resolved by the control plane; a job is
// routed by the id in the body.
func TestCronCreateResolvesASlugInTheBody(t *testing.T) {
	var body map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/my-slug", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"sb_real"}`))
	})
	mux.HandleFunc("POST /v1/cron", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"id":"c1"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	code, _, errS := authedCLI(t, srv, "cron", "create", "my-slug", "0 3 * * *", "date", "--name", "nightly")
	if code != ExitOK || body["sandbox_id"] != "sb_real" || body["schedule"] != "0 3 * * *" || body["command"] != "date" || body["name"] != "nightly" {
		t.Fatalf("exit %d body %v stderr %s", code, body, errS)
	}
}

func TestCronRunFailedJobExitsOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"c1","status":"failed","exit_code":3,"output":"nope"}`))
	}))
	defer srv.Close()
	if code, _, _ := authedCLI(t, srv, "cron", "run", "c1"); code != ExitAPI {
		t.Fatalf("exit %d", code)
	}
}

func TestSandboxCreateVolumeFlag(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"id":"sb_1"}`))
	}))
	defer srv.Close()
	code, _, errS := authedCLI(t, srv, "sandbox", "create", "--volume", "vol-1:/data", "--volume", "vol-2:/a:b")
	if code != ExitOK {
		t.Fatalf("exit %d stderr %s", code, errS)
	}
	vols, _ := body["volumes"].([]any)
	if len(vols) != 2 || vols[0].(map[string]any)["id"] != "vol-1" || vols[1].(map[string]any)["path"] != "/a:b" {
		t.Fatalf("volumes = %v", body["volumes"])
	}
	if code, _, _ := authedCLI(t, srv, "sandbox", "create", "--volume", "vol-1"); code != ExitUsage {
		t.Fatalf("bad --volume = %d", code)
	}
}

func TestSandboxIntegrationsEditsTheWholeSet(t *testing.T) {
	var posted map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/sb_1/integrations", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"servers":["github","memory"]}`))
	})
	mux.HandleFunc("POST /v1/sandboxes/sb_1/integrations", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&posted)
		_, _ = w.Write([]byte(`{"servers":["ok"]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	code, out, errS := authedCLI(t, srv, "sandbox", "integrations", "sb_1")
	if code != ExitOK || !strings.Contains(out, "github\nmemory\n") {
		t.Fatalf("exit %d stdout %q stderr %s", code, out, errS)
	}
	if code, _, errS := authedCLI(t, srv, "sandbox", "integrations", "sb_1", "add", "slack", "--env", "SLACK_TEAM_ID=T1"); code != ExitOK {
		t.Fatalf("add = %d: %s", code, errS)
	}
	if got := fmt.Sprint(posted["servers"]); got != "[github memory slack]" {
		t.Fatalf("add posted %s", got)
	}
	if env, _ := posted["env"].(map[string]any); env["SLACK_TEAM_ID"] != "T1" {
		t.Fatalf("env = %v", posted["env"])
	}
	if code, _, errS := authedCLI(t, srv, "sandbox", "integrations", "sb_1", "rm", "github"); code != ExitOK {
		t.Fatalf("rm = %d: %s", code, errS)
	}
	if got := fmt.Sprint(posted["servers"]); got != "[memory]" {
		t.Fatalf("rm posted %s", got)
	}
	for _, args := range [][]string{{"sandbox", "integrations", "sb_1", "add"}, {"sandbox", "integrations", "sb_1", "replace", "x"}} {
		if code, _, _ := authedCLI(t, srv, args...); code != ExitUsage {
			t.Fatalf("%v = %d", args, code)
		}
	}
}

func TestChatSendsOrReads(t *testing.T) {
	var body map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/sb_1/chat", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"messages":[{"role":"user","text":"hello","at":1}]}`))
	})
	mux.HandleFunc("POST /v1/sandboxes/sb_1/chat", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"reply":"hi back","deprecation":{"sunset":"x"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if code, out, _ := authedCLI(t, srv, "chat", "sb_1"); code != ExitOK || !strings.Contains(out, "hello") {
		t.Fatalf("read: exit %d stdout %q", code, out)
	}
	code, out, _ := authedCLI(t, srv, "chat", "sb_1", "hi", "--autonomous")
	if code != ExitOK || strings.TrimSpace(out) != "hi back" || body["autonomous"] != true || body["message"] != "hi" {
		t.Fatalf("send: exit %d stdout %q body %v", code, out, body)
	}
}

func TestAPICommand(t *testing.T) {
	var method, ctype, sent, query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, ctype, query = r.Method, r.Header.Get("Content-Type"), r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		sent = string(b)
		switch r.URL.Path {
		case "/v1/empty":
			w.WriteHeader(204)
		case "/v1/text":
			_, _ = w.Write([]byte("plain"))
		case "/v1/gone":
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"nope","code":"not_found"}`))
		case "/v1/denied":
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"no"}`))
		default:
			_, _ = w.Write([]byte(`{"a":1}`))
		}
	}))
	defer srv.Close()

	code, out, _ := authedCLI(t, srv, "api", "get", "/v1/x?limit=2")
	if code != ExitOK || out != `{"a":1}` || method != "GET" || query != "limit=2" {
		t.Fatalf("get: exit %d stdout %q %s %s", code, out, method, query)
	}
	code, _, _ = authedCLI(t, srv, "api", "POST", "/v1/x", "--data", `{"n":1}`)
	if code != ExitOK || sent != `{"n":1}` || ctype != "application/json" {
		t.Fatalf("post: exit %d body %q type %q", code, sent, ctype)
	}
	stdin = strings.NewReader(`{"piped":true}`)
	defer func() { stdin = os.Stdin }()
	if code, _, _ := authedCLI(t, srv, "api", "POST", "--data", "-", "/v1/x"); code != ExitOK || sent != `{"piped":true}` {
		t.Fatalf("stdin: exit %d body %q", code, sent)
	}
	for path, want := range map[string]string{"/v1/empty": `{"status":"ok"}`, "/v1/text": `{"body":"plain"}`} {
		code, out, _ := authedCLI(t, srv, "--json", "api", "GET", path)
		var got, wanted any
		_ = json.Unmarshal([]byte(out), &got)
		_ = json.Unmarshal([]byte(want), &wanted)
		if code != ExitOK || fmt.Sprint(got) != fmt.Sprint(wanted) {
			t.Fatalf("%s: exit %d stdout %q", path, code, out)
		}
	}
	if code, _, errS := authedCLI(t, srv, "--json", "api", "GET", "/v1/gone"); code != ExitAPI || !strings.Contains(errS, "not_found") {
		t.Fatalf("404: exit %d stderr %q", code, errS)
	}
	if code, _, _ := authedCLI(t, srv, "api", "GET", "/v1/denied"); code != ExitAuth {
		t.Fatalf("401: exit %d", code)
	}
}

func TestAPIRefusesWhatIsNotV1(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer srv.Close()
	for _, args := range [][]string{
		{"api", "GET", "/dashboard"},
		{"api", "GET", "v1/x"},
		{"api", "GET", "/v1/../dashboard"},
		{"api", "GET", "/v1/%2e%2e/dashboard"},
		{"api", "GET", "/v1x/y"},
		{"api", "GET", "https://elsewhere.test/v1/x"},
		{"api", "TRACE", "/v1/x"},
		{"api", "POST", "/v1/x", "--data", "not json"},
		{"api", "GET"},
	} {
		if code, _, _ := authedCLI(t, srv, args...); code != ExitUsage {
			t.Errorf("%v = %d, want %d", args, code, ExitUsage)
		}
	}
	if calls != 0 {
		t.Fatalf("%d requests sent", calls)
	}
}
