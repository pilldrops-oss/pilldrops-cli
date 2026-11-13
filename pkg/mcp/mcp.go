// Package mcp implements a Model Context Protocol server over the platform's
// /v1 API, so AI agents can drive sandboxes as native tools instead of
// shelling out to the CLI.
//
// Two transports share one protocol core:
//   - Streamable HTTP, for a server that mounts HTTPServer: stateless, one JSON
//     response per POST, auth via the standard Authorization header. Works with
//     Claude Code / Cursor / any client that supports custom headers.
//   - stdio (`pilldrops mcp` in internal/cli): newline-delimited JSON-RPC using
//     the CLI's stored login.
//
// Deliberately stdlib-only and stateless: no sessions, no server-push SSE. A
// tool call is a request/response pair, which is all sandbox operations need.
package mcp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ServerVersion is reported in the initialize handshake. Keep in step with
// internal/cli.Version when releasing.
const ServerVersion = "1.11.1"

// protocolVersions we accept, newest first; initialize echoes the client's
// version when supported, else offers our newest.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Backend performs one platform API call on behalf of a tool. status is the
// HTTP status (0 when err != nil), body the raw response. Implementations:
// in-process router dispatch (hosted) and remote HTTP client (CLI stdio).
type Backend func(method, path string, body []byte, contentType string) (status int, respBody []byte, err error)

// ---- JSON-RPC plumbing ------------------------------------------------------

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func reply(id json.RawMessage, result any) []byte {
	b, _ := json.Marshal(response{JSONRPC: "2.0", ID: rawID(id), Result: result})
	return b
}

func replyErr(id json.RawMessage, code int, msg string) []byte {
	b, _ := json.Marshal(response{JSONRPC: "2.0", ID: rawID(id), Error: &rpcError{Code: code, Message: msg}})
	return b
}

// rawID preserves the caller's id (number or string) without re-typing it.
func rawID(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	return json.RawMessage(id)
}

// HandleMessage processes one JSON-RPC message (or a batch array) and returns
// the serialized response, or nil for notifications.
func HandleMessage(raw []byte, be Backend) []byte {
	return HandleMessageFor(raw, be, nil)
}

// HandleMessageFor serves only the tools whose feature group supported allows,
// so an agent never tries what its server lacks. A nil supported serves all.
func HandleMessageFor(raw []byte, be Backend, supported func(feature string) bool) []byte {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		var msgs []json.RawMessage
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return replyErr(nil, -32700, "parse error: "+err.Error())
		}
		var out []json.RawMessage
		for _, m := range msgs {
			if r := handleOne(m, be, supported); r != nil {
				out = append(out, r)
			}
		}
		if len(out) == 0 {
			return nil
		}
		b, _ := json.Marshal(out)
		return b
	}
	return handleOne(raw, be, supported)
}

func handleOne(raw []byte, be Backend, supported func(string) bool) []byte {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return replyErr(nil, -32700, "parse error: "+err.Error())
	}
	if strings.HasPrefix(req.Method, "notifications/") {
		return nil
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		ver := protocolVersions[0]
		for _, v := range protocolVersions {
			if p.ProtocolVersion == v {
				ver = v
				break
			}
		}
		return reply(req.ID, map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "pilldrops", "version": ServerVersion},
			"instructions": "PillDrops gives agents disposable cloud sandboxes. Typical flow: " +
				"create_sandbox (template python-3.11) → write_file / run_command → read output → " +
				"destroy_sandbox. App servers: create_sandbox with start+ports (the app MUST bind " +
				"the injected $PORT env var) → the returned url is a public preview. Debug with " +
				"get_logs / get_metrics; past sandboxes (with cost) via the CLI `pilldrops history`. " +
				"Before anything risky, save_checkpoint and roll back with restore_sandbox; create_backup " +
				"outlives the sandbox, and save_snapshot turns a prepared sandbox into a template others boot from.",
		})
	case "ping":
		return reply(req.ID, map[string]any{})
	case "tools/list":
		return reply(req.ID, map[string]any{"tools": toolDefs(supported)})
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return replyErr(req.ID, -32602, "invalid params: "+err.Error())
		}
		if !toolServed(p.Name, supported) {
			return reply(req.ID, toolError("this server does not support "+p.Name+" (tools/list shows the catalog)"))
		}
		return reply(req.ID, callTool(p.Name, p.Arguments, be))
	default:
		return replyErr(req.ID, -32601, "method not found: "+req.Method)
	}
}

// ---- tool results -----------------------------------------------------------

// toolText wraps a successful payload per the MCP tool-result shape. JSON
// payloads also ride in structuredContent for clients that consume it.
func toolText(body []byte) map[string]any {
	res := map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(body)}},
	}
	var v any
	if json.Unmarshal(body, &v) == nil {
		if _, isObj := v.(map[string]any); isObj {
			res["structuredContent"] = v
		}
	}
	return res
}

func toolError(msg string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	}
}

// toolTextWithHint is toolText plus an agent-facing hint, carried as a SECOND
// text content block rather than appended into the first — the first block
// must stay the verbatim JSON payload (some clients parse content[0].text as
// JSON directly, and appending text would break that). structuredContent is
// likewise left untouched: it mirrors the platform's response object
// verbatim, with no synthetic "hint" key injected.
func toolTextWithHint(body []byte, hint string) map[string]any {
	res := toolText(body)
	if hint == "" {
		return res
	}
	if content, ok := res["content"].([]map[string]any); ok {
		res["content"] = append(content, map[string]any{"type": "text", "text": hint})
	}
	return res
}

// apiCall runs a backend call and normalizes errors into agent-actionable text.
func apiCall(be Backend, method, path string, body []byte, ctype string) ([]byte, string) {
	status, resp, err := be(method, path, body, ctype)
	if err != nil {
		return nil, "network error calling the PillDrops API: " + err.Error() +
			" — check connectivity (the CLI equivalent is `pilldrops doctor`)"
	}
	if status < 200 || status > 299 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(resp, &e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("HTTP %d", status)
		}
		hint := ""
		switch {
		case status == 401:
			hint = " — missing/invalid API key. HTTP transport: send 'Authorization: Bearer <key>' " +
				"(dashboard → API keys). stdio transport: run `pilldrops login --key <key>` first."
		case status == 404:
			hint = " — sandbox gone or id wrong; list_sandboxes shows active ones."
		case status == 402 || status == 403:
			hint = " — plan/limit issue; get_usage shows consumption vs caps."
		case status >= 500:
			hint = " — server-side; retry once before giving up."
		}
		return nil, fmt.Sprintf("%s (HTTP %d)%s", e.Error, status, hint)
	}
	return resp, ""
}

// parsePreviewURL extracts (id, port) from either preview URL shape the
// platform emits:
//
//	subdomain style: https://<port>-<id>.<domain>/...
//	path style:      <base>/preview/<id>/<port>/...
//
// It never fetches anything — the caller-supplied URL is only ever used to
// pull these two identifiers out; the actual request goes through the
// Backend, never to a caller-controlled host (that's the SSRF fix: a status
// URL is a locator for (id, port), not a fetchable address in its own
// right).
func parsePreviewURL(u *url.URL) (id, port string, ok bool) {
	// Path style: .../preview/<id>/<port>/...
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, s := range segs {
		if s == "preview" && i+2 < len(segs) {
			candID, candPort := segs[i+1], segs[i+2]
			if candID != "" && isDigits(candPort) {
				return candID, candPort, true
			}
		}
	}
	// Subdomain style: <port>-<id>.<domain> — split the first host label on
	// its first '-'; the id itself may contain further '-' or '_'.
	first, _, _ := strings.Cut(u.Hostname(), ".")
	if dash := strings.Index(first, "-"); dash > 0 {
		candPort, candID := first[:dash], first[dash+1:]
		if candID != "" && isDigits(candPort) {
			return candID, candPort, true
		}
	}
	return "", "", false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ---- tools ------------------------------------------------------------------

type tool struct {
	name, desc string
	schema     map[string]any
	run        func(a map[string]any, be Backend) map[string]any
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func prop(t, desc string) map[string]any { return map[string]any{"type": t, "description": desc} }

func str(a map[string]any, k string) string {
	s, _ := a[k].(string)
	return s
}

// num reads an integer argument, 0 if absent or unparseable. Strings are
// accepted because LLM clients routinely send "8080" for an integer field.
func num(a map[string]any, k string) int {
	switch v := a[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return int(i)
		}
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return i
		}
	}
	return 0
}

// jsonBody builds the request body from selected args, mapping tool argument
// names to API field names and skipping zero values.
func toolList() []tool {
	simpleGet := func(pathf string) func(map[string]any, Backend) map[string]any {
		return func(a map[string]any, be Backend) map[string]any {
			p := pathf
			if strings.Contains(pathf, "%s") {
				p = fmt.Sprintf(pathf, url.PathEscape(str(a, "sandbox_id")))
			}
			body, errText := apiCall(be, "GET", p, nil, "")
			if errText != "" {
				return toolError(errText)
			}
			return toolText(body)
		}
	}
	simplePost := func(pathf string) func(map[string]any, Backend) map[string]any {
		return func(a map[string]any, be Backend) map[string]any {
			body, errText := apiCall(be, "POST", fmt.Sprintf(pathf, url.PathEscape(str(a, "sandbox_id"))), nil, "")
			if errText != "" {
				return toolError(errText)
			}
			return toolText(body)
		}
	}
	idSchema := obj(map[string]any{"sandbox_id": prop("string", "sandbox id, e.g. sb_ab12cd")}, "sandbox_id")

	return append([]tool{
		{
			name:   "list_templates",
			desc:   "List available sandbox templates (python-3.11, node, playwright, …) with their default resources.",
			schema: obj(map[string]any{}),
			run:    simpleGet("/v1/templates"),
		},
		{
			name: "create_sandbox",
			desc: "Create an isolated cloud sandbox. Returns {id, status, url?}. For app servers pass start " +
				"(long-lived command) and ports — the app MUST listen on the injected $PORT env var, and the " +
				"returned url is a public preview. Destroy sandboxes when done (usage is metered per second).",
			schema: obj(map[string]any{
				"template":        prop("string", "template id (see list_templates); default python-3.11"),
				"image":           prop("string", "custom docker image instead of a template"),
				"start":           prop("string", "long-lived app/server command; makes the sandbox serve a preview URL"),
				"ports":           map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "preview ports (app must bind $PORT)"},
				"env":             map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "environment variables, KEY=VAL"},
				"vcpu":            prop("integer", "vCPUs (0 = template default)"),
				"memory_mb":       prop("integer", "memory MB (0 = template default)"),
				"storage_mb":      prop("integer", "disk MB (0 = template default)"),
				"timeout_seconds": prop("integer", "sandbox lifetime (0 = plan default)"),
				"idle_seconds":    prop("integer", "idle auto-pause; -1 = never pause (always-on servers)"),
				"runtime":         prop("string", "pin isolation runtime: firecracker | docker (omit = auto)"),
				"public":          prop("boolean", "make the preview URL reachable without a token (shareable link); default false = private"),
				"slug":            prop("string", "DNS label this sandbox is hosted and resolved under; also its name on a network tag (<slug>.pd)"),
				"networks": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "opt-in network tags. Sandboxes cannot reach each other by default; two in the same org " +
						"on the same node reach each other only if they share a tag, and resolve each other as <slug>.pd"},
				"volumes": map[string]any{"type": "array", "description": "standalone volumes to attach (see create_volume)",
					"items": obj(map[string]any{
						"id":   prop("string", "volume id"),
						"path": prop("string", "where to mount it, e.g. /data"),
					}, "id", "path")},
				"secret_refs": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "vault secrets (see list_secrets) to put in the sandbox's environment by name"},
				"proxied_secrets": map[string]any{"type": "array",
					"description": "vault secrets the sandbox may use but never sees: it gets NAME=pd-proxied and " +
						"NAME_PROXY_URL, and the node adds the secret to calls made through that URL to host. Point " +
						"the SDK's base URL at it, e.g. OPENAI_BASE_URL=$OPENAI_API_KEY_PROXY_URL/v1",
					"items": obj(map[string]any{
						"name":   prop("string", "vault secret name"),
						"host":   prop("string", "the API host the secret is for, e.g. api.openai.com"),
						"header": prop("string", "header to set (default Authorization)"),
						"format": prop("string", "value with {} for the secret (default \"Bearer {}\"; Anthropic: \"{}\")"),
					}, "name", "host")},
			}),
			run: func(a map[string]any, be Backend) map[string]any {
				// client:"mcp" tags this create as agent-originated for attribution.
				req := map[string]any{"client": "mcp"}
				if v := str(a, "template"); v != "" {
					req["template"] = v
				}
				for argName, apiName := range map[string]string{"image": "image", "start": "start", "runtime": "runtime", "slug": "slug"} {
					if v := str(a, argName); v != "" {
						req[apiName] = v
					}
				}
				for argName, apiName := range map[string]string{"vcpu": "vcpu", "memory_mb": "memory_mb",
					"storage_mb": "storage_mb", "timeout_seconds": "timeout", "idle_seconds": "idle_timeout"} {
					if v := num(a, argName); v != 0 {
						req[apiName] = v
					}
				}
				if v, ok := a["ports"].([]any); ok && len(v) > 0 {
					req["ports"] = v
				}
				if v, ok := a["env"].([]any); ok && len(v) > 0 {
					req["env"] = v
				}
				if v, ok := a["networks"].([]any); ok && len(v) > 0 {
					req["networks"] = v
				}
				// Preview visibility: private by default; only opt in to public.
				if v, ok := a["public"].(bool); ok && v {
					req["public"] = true
				}
				if v, ok := a["volumes"].([]any); ok && len(v) > 0 {
					req["volumes"] = v
				}
				var refs []any
				if v, ok := a["secret_refs"].([]any); ok {
					refs = append(refs, v...)
				}
				if v, ok := a["proxied_secrets"].([]any); ok {
					for _, p := range v {
						ps, _ := p.(map[string]any)
						proxy := map[string]any{"host": str(ps, "host")}
						for _, k := range []string{"header", "format"} {
							if s := str(ps, k); s != "" {
								proxy[k] = s
							}
						}
						refs = append(refs, map[string]any{"name": str(ps, "name"), "proxy": proxy})
					}
				}
				if len(refs) > 0 {
					req["secret_refs"] = refs
				}
				if req["template"] == nil && req["image"] == nil {
					req["template"] = "python-3.11"
				}
				body, _ := json.Marshal(req)
				resp, errText := apiCall(be, "POST", "/v1/sandboxes", body, "application/json")
				if errText != "" {
					return toolError(errText)
				}
				return toolText(resp)
			},
		},
		{
			name: "set_networks",
			desc: "Replace a sandbox's network tags. Sandboxes cannot reach each other by default; two in the " +
				"same org on the same node reach each other, on any port, only while they share a tag, and " +
				"resolve each other as <slug>.pd. Takes effect at once, no restart. An empty list closes the " +
				"sandbox off again. Same node only: a peer elsewhere neither resolves nor connects.",
			schema: obj(map[string]any{
				"sandbox_id": prop("string", "sandbox id or slug"),
				"networks": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "the tags the sandbox should be on; [] removes it from every network"},
			}, "sandbox_id"),
			run: func(a map[string]any, be Backend) map[string]any {
				nets := []any{}
				if v, ok := a["networks"].([]any); ok {
					nets = v
				}
				body, _ := json.Marshal(map[string]any{"networks": nets})
				resp, errText := apiCall(be, "PATCH", "/v1/sandboxes/"+url.PathEscape(str(a, "sandbox_id")), body, "application/json")
				if errText != "" {
					return toolError(errText)
				}
				return toolText(resp)
			},
		},
		{
			name: "set_egress",
			desc: "Replace a running or paused sandbox's outbound network rules. Entries are hostnames " +
				"(\"*.example.com\" for subdomains), IPv4 addresses or CIDRs; deny [\"0.0.0.0/0\"] blocks " +
				"everything not allowed. The org's policy is the ceiling: rules can narrow it, never widen it. " +
				"Takes effect at once and cuts open connections. Omit both lists to return to the default.",
			schema: obj(map[string]any{
				"sandbox_id": prop("string", "sandbox id or slug"),
				"allow":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"deny":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			}, "sandbox_id"),
			run: func(a map[string]any, be Backend) map[string]any {
				var network any
				allow, hasAllow := a["allow"].([]any)
				deny, hasDeny := a["deny"].([]any)
				if hasAllow || hasDeny {
					network = map[string]any{"allow": nonNil(allow), "deny": nonNil(deny)}
				}
				body, _ := json.Marshal(map[string]any{"network": network})
				resp, errText := apiCall(be, "PATCH", "/v1/sandboxes/"+url.PathEscape(str(a, "sandbox_id")), body, "application/json")
				if errText != "" {
					return toolError(errText)
				}
				return toolText(resp)
			},
		},
		{
			name:   "list_sandboxes",
			desc:   "List active sandboxes: id, template, status.",
			schema: obj(map[string]any{}),
			run:    simpleGet("/v1/sandboxes"),
		},
		{
			name:   "get_sandbox",
			desc:   "Inspect one sandbox: status, resources, preview url when it serves an app.",
			schema: idSchema,
			run:    simpleGet("/v1/sandboxes/%s"),
		},
		{
			name: "run_command",
			desc: "Run a shell command in the sandbox and get {stdout, stderr, exit_code}. Use " +
				"bash -c '…' style strings for pipelines. Large output is truncated — write to a file " +
				"and read_file it instead.",
			schema: obj(map[string]any{
				"sandbox_id":      prop("string", "sandbox id"),
				"cmd":             prop("string", "shell command to run"),
				"cwd":             prop("string", "working directory (default workspace root)"),
				"timeout_seconds": prop("integer", "command timeout (default 60)"),
			}, "sandbox_id", "cmd"),
			run: func(a map[string]any, be Backend) map[string]any {
				req := map[string]any{"cmd": str(a, "cmd")}
				if v := str(a, "cwd"); v != "" {
					req["cwd"] = v
				}
				if v := num(a, "timeout_seconds"); v != 0 {
					req["timeout"] = v
				}
				body, _ := json.Marshal(req)
				resp, errText := apiCall(be, "POST", "/v1/sandboxes/"+url.PathEscape(str(a, "sandbox_id"))+"/exec", body, "application/json")
				if errText != "" {
					return toolError(errText)
				}
				return toolText(resp)
			},
		},
		{
			name: "start_server",
			desc: "Register + launch a persistent server command in the sandbox (survives idle-pause/resume, " +
				"unlike run_command). The app must bind $PORT.",
			schema: obj(map[string]any{
				"sandbox_id": prop("string", "sandbox id"),
				"cmd":        prop("string", "server command, e.g. `python /app/main.py`"),
				"port":       prop("integer", "port the app listens on (0 = the sandbox default)"),
			}, "sandbox_id", "cmd"),
			run: func(a map[string]any, be Backend) map[string]any {
				body, _ := json.Marshal(map[string]any{"cmd": str(a, "cmd"), "port": num(a, "port")})
				resp, errText := apiCall(be, "POST", "/v1/sandboxes/"+url.PathEscape(str(a, "sandbox_id"))+"/start", body, "application/json")
				if errText != "" {
					return toolError(errText)
				}
				return toolText(resp)
			},
		},
		{
			name: "write_file",
			desc: "Write text content to a file in the sandbox workspace (path is relative to the workspace root; parent dirs are created).",
			schema: obj(map[string]any{
				"sandbox_id": prop("string", "sandbox id"),
				"path":       prop("string", "file path relative to the workspace, e.g. app/main.py"),
				"content":    prop("string", "full file content"),
			}, "sandbox_id", "path", "content"),
			run: func(a map[string]any, be Backend) map[string]any {
				p := "/v1/sandboxes/" + url.PathEscape(str(a, "sandbox_id")) + "/files?path=" + url.QueryEscape(strings.TrimPrefix(str(a, "path"), "/"))
				_, errText := apiCall(be, "POST", p, []byte(str(a, "content")), "application/octet-stream")
				if errText != "" {
					return toolError(errText)
				}
				return toolText([]byte(`{"written":"` + str(a, "path") + `"}`))
			},
		},
		{
			name: "read_file",
			desc: "Read a file from the sandbox workspace and return its content as text.",
			schema: obj(map[string]any{
				"sandbox_id": prop("string", "sandbox id"),
				"path":       prop("string", "file path relative to the workspace"),
			}, "sandbox_id", "path"),
			run: func(a map[string]any, be Backend) map[string]any {
				p := "/v1/sandboxes/" + url.PathEscape(str(a, "sandbox_id")) + "/files?path=" + url.QueryEscape(strings.TrimPrefix(str(a, "path"), "/"))
				resp, errText := apiCall(be, "GET", p, nil, "")
				if errText != "" {
					return toolError(errText)
				}
				return map[string]any{"content": []map[string]any{{"type": "text", "text": string(resp)}}}
			},
		},
		{
			name: "get_logs",
			desc: "Fetch recent sandbox logs (start command + service output) — the first stop when an app misbehaves or a preview 502s.",
			schema: obj(map[string]any{
				"sandbox_id": prop("string", "sandbox id"),
				"limit":      prop("integer", "max lines (default 500)"),
			}, "sandbox_id"),
			run: func(a map[string]any, be Backend) map[string]any {
				limit := num(a, "limit")
				if limit == 0 {
					limit = 500
				}
				resp, errText := apiCall(be, "GET", fmt.Sprintf("/v1/sandboxes/%s/logs?limit=%d", url.PathEscape(str(a, "sandbox_id")), limit), nil, "")
				if errText != "" {
					return toolError(errText)
				}
				return toolText(resp)
			},
		},
		{
			name:   "get_metrics",
			desc:   "Live CPU/memory of a sandbox — spot OOMs and busy loops.",
			schema: idSchema,
			run:    simpleGet("/v1/sandboxes/%s/metrics"),
		},
		{
			name:   "pause_sandbox",
			desc:   "Pause a sandbox: its memory is written to disk and freed, state is kept. resume_sandbox wakes it.",
			schema: idSchema,
			run:    simplePost("/v1/sandboxes/%s/pause"),
		},
		{
			name: "fork_sandbox",
			desc: "Fork a sandbox: a running copy with the same disk, memory and processes, under a new id. " +
				"Use it to try something risky on a copy, run several attempts from one state, or reproduce a bug without touching the original.",
			schema: idSchema,
			run:    simplePost("/v1/sandboxes/%s/fork"),
		},
		{
			name:   "resume_sandbox",
			desc:   "Resume a paused sandbox (also relaunches its registered start_server command).",
			schema: idSchema,
			run:    simplePost("/v1/sandboxes/%s/resume"),
		},
		{
			name: "get_events",
			desc: "Recent lifecycle events for the org, newest first: created/paused/resumed/destroyed and " +
				"`oom` (a process was out-of-memory-killed inside a sandbox). Watch this to notice a runaway " +
				"process getting reaped. Pass sandbox_id to filter to one sandbox.",
			schema: obj(map[string]any{"sandbox_id": prop("string", "optional: only this sandbox's events")}),
			run: func(a map[string]any, be Backend) map[string]any {
				body, errText := apiCall(be, "GET", "/v1/events?limit=200", nil, "")
				if errText != "" {
					return toolError(errText)
				}
				sid := str(a, "sandbox_id")
				if sid == "" {
					return toolText(body)
				}
				var resp struct {
					Events []map[string]any `json:"events"`
				}
				_ = json.Unmarshal(body, &resp)
				filtered := make([]map[string]any, 0, len(resp.Events))
				for _, e := range resp.Events {
					if s, _ := e["sandbox_id"].(string); s == sid {
						filtered = append(filtered, e)
					}
				}
				out, _ := json.Marshal(map[string]any{"events": filtered})
				return toolText(out)
			},
		},
		{
			name: "wait_ready",
			desc: "Block until the sandbox's app answers on a port (or timeout), by probing 127.0.0.1:<port> " +
				"from inside the sandbox. Returns {ready, port, code}. Use after start_server, before hitting the preview url.",
			schema: obj(map[string]any{
				"sandbox_id":      prop("string", "sandbox id"),
				"port":            prop("integer", "app port (default 8000)"),
				"timeout_seconds": prop("integer", "max seconds to wait (default 30)"),
			}, "sandbox_id"),
			run: func(a map[string]any, be Backend) map[string]any {
				sid := url.PathEscape(str(a, "sandbox_id"))
				port := num(a, "port")
				if port == 0 {
					port = 8000
				}
				budget := num(a, "timeout_seconds")
				if budget == 0 {
					budget = 30
				}
				probe, _ := json.Marshal(map[string]any{
					"cmd":     fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' 127.0.0.1:%d 2>/dev/null || echo 000", port),
					"timeout": 5,
				})
				for i := 0; i < budget; i++ {
					resp, errText := apiCall(be, "POST", "/v1/sandboxes/"+sid+"/exec", probe, "application/json")
					if errText == "" {
						var r struct {
							Stdout string `json:"stdout"`
						}
						_ = json.Unmarshal(resp, &r)
						code := strings.TrimSpace(r.Stdout)
						if len(code) == 3 && (code[0] == '2' || code[0] == '3') {
							out, _ := json.Marshal(map[string]any{"ready": true, "port": port, "code": code})
							return toolText(out)
						}
					}
					time.Sleep(time.Second)
				}
				out, _ := json.Marshal(map[string]any{"ready": false, "port": port})
				return toolText(out)
			},
		},
		{
			name:   "destroy_sandbox",
			desc:   "Destroy a sandbox. Its record, logs and usage remain in history.",
			schema: idSchema,
			run: func(a map[string]any, be Backend) map[string]any {
				id := str(a, "sandbox_id")
				if id == "" {
					// Without this gate an empty id dispatches DELETE /v1/sandboxes/
					// and surfaces a misleading 404 (or a bogus success).
					return toolError(`destroy_sandbox requires a "sandbox_id" — the id returned by create_sandbox`)
				}
				resp, errText := apiCall(be, "DELETE", "/v1/sandboxes/"+url.PathEscape(id), nil, "")
				if errText != "" {
					return toolError(errText)
				}
				if len(resp) == 0 {
					resp = []byte(`{"destroyed":true}`)
				}
				return toolText(resp)
			},
		},
		{
			name:   "get_usage",
			desc:   "Account usage and charges — current consumption vs plan limits.",
			schema: obj(map[string]any{}),
			run:    simpleGet("/v1/usage"),
		},
	}, append(platformTools(), customerTools()...)...)
}

func toolDefs(supported func(string) bool) []map[string]any {
	var defs []map[string]any
	for _, t := range toolList() {
		if !toolServed(t.name, supported) {
			continue
		}
		defs = append(defs, map[string]any{
			"name": t.name, "description": t.desc, "inputSchema": t.schema,
		})
	}
	return defs
}

func callTool(name string, args map[string]any, be Backend) map[string]any {
	if args == nil {
		args = map[string]any{}
	}
	for _, t := range toolList() {
		if t.name == name {
			return t.run(args, be)
		}
	}
	return toolError("unknown tool: " + name + " (tools/list shows the catalog)")
}

// optionalToolFeatures ties tools to the feature group that serves them; a tool
// matching none is core.
var optionalToolFeatures = map[string][]string{
	"control-plane": {"add_domain", "create_project", "delete_project", "delete_secret", "delete_snapshot", "get_project", "list_domains", "list_project_runs", "list_projects", "list_secrets", "list_snapshots", "put_secret", "remove_domain", "save_snapshot", "start_project_run"},
	"agent":         {"agent_ask", "agent_decide_approval", "agent_get_turn", "agent_interrupt", "agent_list_approvals", "agent_list_turns", "agent_status"},
	"cron":          {"create_cron_job", "delete_cron_job", "list_cron_jobs", "run_cron_job", "toggle_cron_job"},
	"function":      {"create_function", "delete_function", "get_function", "get_function_logs", "list_functions"},
	"integrations":  {"list_integrations"},
	"thread":        {"create_thread", "delete_thread", "get_thread", "list_threads"},
	"volume":        {"create_volume", "delete_volume", "list_volumes"},
	"webhook":       {"create_webhook", "delete_webhook", "list_webhook_deliveries", "list_webhooks", "test_webhook", "toggle_webhook"},
}

func toolServed(name string, supported func(string) bool) bool {
	if supported == nil {
		return true
	}
	for feature, names := range optionalToolFeatures {
		if slices.Contains(names, name) {
			return supported(feature)
		}
	}
	return true
}

// ToolCount is how many tools this server serves. Exported so the CLI's agent
// catalog can state the number instead of carrying a hand-typed one, which is
// how it came to advertise 18 against 41.
func ToolCount() int { return len(toolList()) }

// ToolCountFor is ToolCount limited as HandleMessageFor limits the catalog.
func ToolCountFor(supported func(string) bool) int {
	n := 0
	for _, t := range toolList() {
		if toolServed(t.name, supported) {
			n++
		}
	}
	return n
}

// nonNil keeps an absent list from encoding as null, which the API reads as "no rules".
func nonNil(v []any) []any {
	if v == nil {
		return []any{}
	}
	return v
}
