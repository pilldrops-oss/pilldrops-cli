package mcp

import (
	"net/url"
	"regexp"
	"strconv"
)

// The customer API beyond sandboxes themselves: agents, cron, functions,
// projects, threads, webhooks, images, integrations and the audit log. Plain REST calls,
// so they are declared as a table. Maintained here and copied to the CLI's
// pkg/mcp, like platformtools.go.

// restTool declares one tool that is a single API call. Path segments written
// {arg} come from the argument of that name.
type restTool struct {
	name, desc, method, path string
	args                     map[string]any
	required                 []string
	bodyFields, queryFields  []string
	// confirm guards a call that cannot be undone behind confirm=true.
	confirm bool
	// failWhenNotOK marks a 200 whose body says "ok": false as a tool error: the
	// call worked, but what it reports on (a webhook endpoint) did not.
	failWhenNotOK bool
}

var pathArg = regexp.MustCompile(`\{(\w+)\}`)

func (rt restTool) run(a map[string]any, be Backend) map[string]any {
	if rt.confirm && !truthy(a, "confirm") {
		return toolError(rt.name + " cannot be undone — pass confirm=true to go ahead")
	}
	missing := ""
	path := pathArg.ReplaceAllStringFunc(rt.path, func(segment string) string {
		arg := segment[1 : len(segment)-1]
		if str(a, arg) == "" {
			missing = arg
		}
		return url.PathEscape(str(a, arg))
	})
	if missing != "" {
		return toolError(rt.name + " needs " + missing)
	}
	query := url.Values{}
	for _, k := range rt.queryFields {
		if n := num(a, k); n != 0 {
			query.Set(k, strconv.Itoa(n))
		}
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var body map[string]any
	for _, k := range rt.bodyFields {
		if v, set := a[k]; set {
			if body == nil {
				body = map[string]any{}
			}
			body[k] = v
		}
	}
	var res map[string]any
	if body == nil {
		res = call(be, rt.method, path, nil)
	} else {
		res = call(be, rt.method, path, body)
	}
	if out, _ := res["structuredContent"].(map[string]any); rt.failWhenNotOK && out["ok"] == false {
		res["isError"] = true
	}
	return res
}

func customerTools() []tool {
	withArgs := func(base map[string]any, extra map[string]any) map[string]any {
		props := map[string]any{}
		for k, v := range base {
			props[k] = v
		}
		for k, v := range extra {
			props[k] = v
		}
		return props
	}
	sandbox := map[string]any{"sandbox_id": prop("string", "sandbox id of an agent template")}
	turn := withArgs(sandbox, map[string]any{"turn_id": prop("string", "turn id from agent_ask or agent_list_turns")})
	confirm := prop("boolean", "must be true; without it nothing happens")
	byID := func(what string) map[string]any { return map[string]any{"id": prop("string", what+" id")} }

	table := []restTool{
		{name: "agent_ask", method: "POST", path: "/v1/sandboxes/{sandbox_id}/agent/turns",
			desc: "Give a running agent a task. Returns a turn at once and does not wait for it: poll " +
				"agent_get_turn until its state is final. Resumes a paused agent first, so it can take seconds. " +
				"mode decides what the agent may do alone: supervised asks before anything, guided before " +
				"risky actions, autonomous never. A guided or supervised turn can stop on an approval, see agent_list_approvals.",
			args: withArgs(sandbox, map[string]any{
				"message":  prop("string", "what to do"),
				"mode":     prop("string", "supervised, guided or autonomous; default the agent's own"),
				"thread":   prop("string", "latest (default), new, or a thread id"),
				"priority": prop("boolean", "jump the queue"),
			}),
			required: []string{"sandbox_id", "message"}, bodyFields: []string{"message", "mode", "thread", "priority"}},
		{name: "agent_status", method: "GET", path: "/v1/sandboxes/{sandbox_id}/agent/status",
			desc: "What an agent is doing now: whether it is ready, its current turn and its queue.",
			args: sandbox, required: []string{"sandbox_id"}},
		{name: "agent_list_turns", method: "GET", path: "/v1/sandboxes/{sandbox_id}/agent/turns",
			desc: "An agent's turns with the state, mode and thread of each.",
			args: sandbox, required: []string{"sandbox_id"}},
		{name: "agent_get_turn", method: "GET", path: "/v1/sandboxes/{sandbox_id}/agent/turns/{turn_id}",
			desc: "One turn in full: its state, reply, usage and any error. Poll it after agent_ask.",
			args: turn, required: []string{"sandbox_id", "turn_id"}},
		{name: "agent_interrupt", method: "POST", path: "/v1/sandboxes/{sandbox_id}/agent/turns/{turn_id}/interrupt",
			desc:     "Stop a running turn. grace_seconds lets the agent stop politely before it is killed.",
			args:     withArgs(turn, map[string]any{"grace_seconds": prop("number", "seconds to stop politely")}),
			required: []string{"sandbox_id", "turn_id"}, bodyFields: []string{"grace_seconds"}},
		{name: "agent_list_approvals", method: "GET", path: "/v1/sandboxes/{sandbox_id}/agent/approvals",
			desc: "Actions an agent is waiting on a decision for. Decide each with agent_decide_approval.",
			args: sandbox, required: []string{"sandbox_id"}},
		{name: "agent_decide_approval", method: "POST", path: "/v1/sandboxes/{sandbox_id}/agent/approvals/{approval_id}",
			desc: "Allow or deny an action an agent is waiting on. Read what it asks in agent_list_approvals first.",
			args: withArgs(sandbox, map[string]any{
				"approval_id": prop("string", "approval id"),
				"decision":    prop("string", "allow or deny"),
			}),
			required: []string{"sandbox_id", "approval_id", "decision"}, bodyFields: []string{"decision"}},

		{name: "list_cron_jobs", method: "GET", path: "/v1/cron", desc: "Scheduled jobs and when each runs next.",
			args: map[string]any{}},
		{name: "create_cron_job", method: "POST", path: "/v1/cron",
			desc: "Run a command in a sandbox on a schedule. With " +
				"agent_mode the command is a prompt to the sandbox's agent instead of a shell command.",
			args: map[string]any{
				"sandbox_id": prop("string", "sandbox id, not its name"),
				"schedule":   prop("string", "cron expression, e.g. 0 2 * * *"),
				"command":    prop("string", "command to run, or the prompt with agent_mode"),
				"name":       prop("string", "label for the job"),
				"timezone":   prop("string", "IANA zone the schedule is read in; default UTC"),
				"agent_mode": prop("string", "guided or autonomous"),
			},
			required:   []string{"sandbox_id", "schedule", "command"},
			bodyFields: []string{"sandbox_id", "schedule", "command", "name", "timezone", "agent_mode"}},
		{name: "run_cron_job", method: "POST", path: "/v1/cron/{id}/run", desc: "Run a scheduled job now.",
			args: byID("job"), required: []string{"id"}},
		{name: "toggle_cron_job", method: "POST", path: "/v1/cron/{id}/toggle", desc: "Turn a scheduled job on or off.",
			args:     withArgs(byID("job"), map[string]any{"enabled": prop("boolean", "true to run it on schedule")}),
			required: []string{"id", "enabled"}, bodyFields: []string{"enabled"}},
		{name: "delete_cron_job", method: "DELETE", path: "/v1/cron/{id}", desc: "Delete a scheduled job.",
			args: withArgs(byID("job"), map[string]any{"confirm": confirm}), required: []string{"id", "confirm"}, confirm: true},

		{name: "list_functions", method: "GET", path: "/v1/functions", desc: "Deployed functions with their replicas and invoke counts.",
			args: map[string]any{}},
		{name: "get_function", method: "GET", path: "/v1/functions/{name}", desc: "One function's configuration and URL.",
			args: map[string]any{"name": prop("string", "function name")}, required: []string{"name"}},
		{name: "create_function", method: "POST", path: "/v1/functions",
			desc: "Deploy a function: a command that serves HTTP on a port, scaled from zero by requests to its URL.",
			args: map[string]any{
				"name":         prop("string", "function name"),
				"start":        prop("string", "command that serves the function"),
				"template":     prop("string", "template id; default the server's first"),
				"port":         prop("integer", "port the command listens on"),
				"vcpu":         prop("integer", "vCPUs"),
				"memory_mb":    prop("integer", "memory in MB"),
				"max_replicas": prop("integer", "burst ceiling"),
				"env":          map[string]any{"type": "object", "description": "environment variables by name"},
				"files": map[string]any{"type": "array", "description": "source files to include",
					"items": obj(map[string]any{"path": prop("string", "path in the function"), "content": prop("string", "file text")}, "path", "content")},
			},
			required:   []string{"name", "start"},
			bodyFields: []string{"name", "start", "template", "port", "vcpu", "memory_mb", "max_replicas", "env", "files"}},
		{name: "get_function_logs", method: "GET", path: "/v1/functions/{name}/logs", desc: "A function's recent logs.",
			args:     map[string]any{"name": prop("string", "function name"), "limit": prop("integer", "lines to return")},
			required: []string{"name"}, queryFields: []string{"limit"}},
		{name: "delete_function", method: "DELETE", path: "/v1/functions/{name}", desc: "Delete a function and its replicas.",
			args:     map[string]any{"name": prop("string", "function name"), "confirm": confirm},
			required: []string{"name", "confirm"}, confirm: true},

		{name: "list_projects", method: "GET", path: "/v1/projects", desc: "Projects and their stored plans.", args: map[string]any{}},
		{name: "get_project", method: "GET", path: "/v1/projects/{id}", desc: "One project with its plan.",
			args: byID("project"), required: []string{"id"}},
		{name: "create_project", method: "POST", path: "/v1/projects", desc: "Create a project. The plan is stored as given.",
			args:     map[string]any{"name": prop("string", "project name"), "plan": prop("string", "the project's plan")},
			required: []string{"name"}, bodyFields: []string{"name", "plan"}},
		{name: "delete_project", method: "DELETE", path: "/v1/projects/{id}", desc: "Delete a project and its run history.",
			args: withArgs(byID("project"), map[string]any{"confirm": confirm}), required: []string{"id", "confirm"}, confirm: true},
		{name: "list_project_runs", method: "GET", path: "/v1/projects/{id}/runs", desc: "A project's runs.",
			args: byID("project"), required: []string{"id"}},
		{name: "start_project_run", method: "POST", path: "/v1/projects/{id}/runs",
			desc: "Record a run of a project: which sandbox carries each planned service.",
			args: withArgs(byID("project"), map[string]any{
				"services": map[string]any{"type": "array", "description": "planned services and their sandboxes",
					"items": obj(map[string]any{"service_id": prop("string", "planned service"), "sandbox_id": prop("string", "sandbox id")}, "service_id", "sandbox_id")},
			}),
			required: []string{"id"}, bodyFields: []string{"services"}},

		{name: "list_threads", method: "GET", path: "/v1/threads", desc: "Saved conversation threads.", args: map[string]any{}},
		{name: "get_thread", method: "GET", path: "/v1/threads/{id}", desc: "One saved thread with its messages.",
			args: byID("thread"), required: []string{"id"}},
		{name: "create_thread", method: "POST", path: "/v1/threads", desc: "Create a saved conversation thread.",
			args: map[string]any{"title": prop("string", "name for the thread")}, bodyFields: []string{"title"}},
		{name: "delete_thread", method: "DELETE", path: "/v1/threads/{id}", desc: "Delete a saved thread.",
			args: withArgs(byID("thread"), map[string]any{"confirm": confirm}), required: []string{"id", "confirm"}, confirm: true},

		{name: "list_webhooks", method: "GET", path: "/v1/webhooks",
			desc: "Webhooks that report sandbox lifecycle events for the whole organization, with the events one can subscribe to.",
			args: map[string]any{}},
		{name: "create_webhook", method: "POST", path: "/v1/webhooks",
			desc: "Send a signed request to a URL when a sandbox in this organization starts, ends, pauses or runs into trouble. " +
				"The reply holds the signing secret once; it cannot be read again.",
			args: map[string]any{
				"url": prop("string", "https URL reachable from the internet"),
				"events": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "event types to send, e.g. sandbox.created, sandbox.ended; list_webhooks names them all"},
			},
			required: []string{"url", "events"}, bodyFields: []string{"url", "events"}},
		{name: "test_webhook", method: "POST", path: "/v1/webhooks/{id}/test",
			desc: "Send one signed test request to a webhook and report how its endpoint answered.",
			args: byID("webhook"), required: []string{"id"}, failWhenNotOK: true},
		{name: "toggle_webhook", method: "POST", path: "/v1/webhooks/{id}/toggle", desc: "Turn a webhook on or off.",
			args:     withArgs(byID("webhook"), map[string]any{"enabled": prop("boolean", "true to deliver events")}),
			required: []string{"id", "enabled"}, bodyFields: []string{"enabled"}},
		{name: "delete_webhook", method: "DELETE", path: "/v1/webhooks/{id}", desc: "Delete a webhook and its signing secret.",
			args: withArgs(byID("webhook"), map[string]any{"confirm": confirm}), required: []string{"id", "confirm"}, confirm: true},
		{name: "list_webhook_deliveries", method: "GET", path: "/v1/webhooks/deliveries",
			desc: "Recent webhook deliveries, newest first, with how each endpoint answered.", args: map[string]any{}},

		{name: "list_images", method: "GET", path: "/v1/images", desc: "Images sandboxes can boot from, including your own custom ones.",
			args: map[string]any{}},
		{name: "list_integrations", method: "GET", path: "/v1/integrations", desc: "MCP servers available to attach to an agent.",
			args: map[string]any{}},
		{name: "list_audit_log", method: "GET", path: "/v1/audit", desc: "Who did what in this organization, newest first.",
			args: map[string]any{"limit": prop("integer", "entries to return")}, queryFields: []string{"limit"}},
	}
	tools := make([]tool, 0, len(table))
	for _, rt := range table {
		tools = append(tools, tool{name: rt.name, desc: rt.desc, schema: obj(rt.args, rt.required...), run: rt.run})
	}
	return tools
}
