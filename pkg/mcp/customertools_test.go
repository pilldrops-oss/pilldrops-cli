package mcp

import (
	"strings"
	"testing"
)

func TestCustomerToolsCallTheRouteTheyClaim(t *testing.T) {
	sb := map[string]any{"sandbox_id": "sb_1"}
	id := map[string]any{"id": "x/1"}
	for _, tc := range []struct {
		tool               string
		args               map[string]any
		method, path, body string
	}{
		{"agent_ask", map[string]any{"sandbox_id": "sb_1", "message": "hi", "mode": "guided"}, "POST", "/v1/sandboxes/sb_1/agent/turns", `{"message":"hi","mode":"guided"}`},
		{"agent_status", sb, "GET", "/v1/sandboxes/sb_1/agent/status", ""},
		{"agent_list_turns", sb, "GET", "/v1/sandboxes/sb_1/agent/turns", ""},
		{"agent_get_turn", map[string]any{"sandbox_id": "sb_1", "turn_id": "t1"}, "GET", "/v1/sandboxes/sb_1/agent/turns/t1", ""},
		{"agent_interrupt", map[string]any{"sandbox_id": "sb_1", "turn_id": "t1"}, "POST", "/v1/sandboxes/sb_1/agent/turns/t1/interrupt", ""},
		{"agent_list_approvals", sb, "GET", "/v1/sandboxes/sb_1/agent/approvals", ""},
		{"agent_decide_approval", map[string]any{"sandbox_id": "sb_1", "approval_id": "a1", "decision": "deny"}, "POST", "/v1/sandboxes/sb_1/agent/approvals/a1", `{"decision":"deny"}`},
		{"list_cron_jobs", nil, "GET", "/v1/cron", ""},
		{"create_cron_job", map[string]any{"sandbox_id": "sb_1", "schedule": "0 2 * * *", "command": "ls"}, "POST", "/v1/cron", `"schedule":"0 2 * * *"`},
		{"run_cron_job", id, "POST", "/v1/cron/x%2F1/run", ""},
		{"toggle_cron_job", map[string]any{"id": "j1", "enabled": false}, "POST", "/v1/cron/j1/toggle", `{"enabled":false}`},
		{"delete_cron_job", map[string]any{"id": "j1", "confirm": true}, "DELETE", "/v1/cron/j1", ""},
		{"list_webhooks", nil, "GET", "/v1/webhooks", ""},
		{"create_webhook", map[string]any{"url": "https://example.test/h", "events": []any{"sandbox.ended"}}, "POST", "/v1/webhooks", `"events":["sandbox.ended"]`},
		{"test_webhook", map[string]any{"id": "whk_1"}, "POST", "/v1/webhooks/whk_1/test", ""},
		{"toggle_webhook", map[string]any{"id": "whk_1", "enabled": false}, "POST", "/v1/webhooks/whk_1/toggle", `{"enabled":false}`},
		{"delete_webhook", map[string]any{"id": "whk_1", "confirm": true}, "DELETE", "/v1/webhooks/whk_1", ""},
		{"list_webhook_deliveries", nil, "GET", "/v1/webhooks/deliveries", ""},
		{"list_functions", nil, "GET", "/v1/functions", ""},
		{"get_function", map[string]any{"name": "f"}, "GET", "/v1/functions/f", ""},
		{"create_function", map[string]any{"name": "f", "start": "node a.js", "port": float64(8000)}, "POST", "/v1/functions", `"start":"node a.js"`},
		{"get_function_logs", map[string]any{"name": "f", "limit": float64(5)}, "GET", "/v1/functions/f/logs?limit=5", ""},
		{"delete_function", map[string]any{"name": "f", "confirm": true}, "DELETE", "/v1/functions/f", ""},
		{"list_projects", nil, "GET", "/v1/projects", ""},
		{"get_project", id, "GET", "/v1/projects/x%2F1", ""},
		{"create_project", map[string]any{"name": "p"}, "POST", "/v1/projects", `{"name":"p"}`},
		{"delete_project", map[string]any{"id": "p1", "confirm": true}, "DELETE", "/v1/projects/p1", ""},
		{"list_project_runs", id, "GET", "/v1/projects/x%2F1/runs", ""},
		{"start_project_run", map[string]any{"id": "p1", "services": []any{}}, "POST", "/v1/projects/p1/runs", `{"services":[]}`},
		{"list_threads", nil, "GET", "/v1/threads", ""},
		{"get_thread", id, "GET", "/v1/threads/x%2F1", ""},
		{"create_thread", map[string]any{"title": "t"}, "POST", "/v1/threads", `{"title":"t"}`},
		{"delete_thread", map[string]any{"id": "t1", "confirm": true}, "DELETE", "/v1/threads/t1", ""},
		{"list_images", nil, "GET", "/v1/images", ""},
		{"list_integrations", nil, "GET", "/v1/integrations", ""},
		{"list_audit_log", map[string]any{"limit": float64(3)}, "GET", "/v1/audit?limit=3", ""},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			be := &recordingBackend{}
			args := tc.args
			if args == nil {
				args = map[string]any{}
			}
			callTool(tc.tool, args, be.fn)
			if len(be.calls) != 1 {
				t.Fatalf("made %d backend calls, want 1: %v", len(be.calls), be.calls)
			}
			got := be.calls[0]
			if got.method != tc.method || got.path != tc.path {
				t.Errorf("called %s %s, want %s %s", got.method, got.path, tc.method, tc.path)
			}
			if tc.body == "" && got.body != "" {
				t.Errorf("sent a body the route does not read: %s", got.body)
			}
			if !strings.Contains(got.body, tc.body) {
				t.Errorf("body = %s, want it to contain %s", got.body, tc.body)
			}
		})
	}
}

func TestCustomerToolsRefuseWithoutWhatTheyNeed(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"delete_cron_job", map[string]any{"id": "j1"}},
		{"delete_webhook", map[string]any{"id": "whk_1"}},
		{"delete_function", map[string]any{"name": "f"}},
		{"delete_project", map[string]any{"id": "p1"}},
		{"delete_thread", map[string]any{"id": "t1"}},
		{"agent_status", map[string]any{}},
		{"delete_cron_job", map[string]any{"confirm": true}},
	} {
		be := &recordingBackend{}
		res := callTool(tc.tool, tc.args, be.fn)
		if len(be.calls) != 0 {
			t.Errorf("%s %v reached the API: %v", tc.tool, tc.args, be.calls)
		}
		if res["isError"] != true {
			t.Errorf("%s %v answered without an error: %v", tc.tool, tc.args, res)
		}
	}
}

func TestToolsFollowServerFeatures(t *testing.T) {
	be := &recordingBackend{}
	coreOnly := func(string) bool { return false }

	known := map[string]bool{}
	for _, tl := range toolList() {
		known[tl.name] = true
	}
	for feature, names := range optionalToolFeatures {
		for _, name := range names {
			if !known[name] {
				t.Errorf("feature %s names tool %q, which does not exist", feature, name)
			}
		}
	}

	listed := map[string]bool{}
	for _, d := range toolDefs(coreOnly) {
		listed[d["name"].(string)] = true
	}
	if listed["list_volumes"] || listed["agent_ask"] || !listed["create_sandbox"] {
		t.Errorf("core server lists %v", listed)
	}
	if ToolCountFor(coreOnly) != len(listed) || ToolCountFor(nil) != ToolCount() {
		t.Errorf("counts: %d filtered for %d listed, %d unfiltered for %d", ToolCountFor(coreOnly), len(listed), ToolCountFor(nil), ToolCount())
	}

	raw := HandleMessageFor([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_volumes"}}`), be.fn, coreOnly)
	if !strings.Contains(string(raw), "does not support list_volumes") || len(be.calls) != 0 {
		t.Errorf("hidden tool: %s, backend calls %d", raw, len(be.calls))
	}
}

// A probe that reached an endpoint which refused it is a 200 with ok=false; an
// agent reading only isError must still see that the webhook does not work.
func TestTestWebhookReportsAFailedProbeAsAnError(t *testing.T) {
	for resp, wantErr := range map[string]bool{
		`{"webhook_id":"whk_1","status":500,"ok":false,"error":"your endpoint answered Internal Server Error"}`: true,
		`{"webhook_id":"whk_1","status":200,"ok":true}`:                                                         false,
	} {
		v := rpc(t, (&fakeBackend{status: 200, resp: resp}).fn(), "tools/call",
			map[string]any{"name": "test_webhook", "arguments": map[string]any{"id": "whk_1"}})
		res := v["result"].(map[string]any)
		if got := res["isError"] == true; got != wantErr {
			t.Errorf("probe answer %s: isError=%v, want %v", resp, got, wantErr)
		}
	}
}
