package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

func volumeVerbs() []apiVerb {
	return []apiVerb{
		{sub: "create", args: "[name] --size-mb N", optional: 1, method: "POST", path: "/v1/volumes", flags: func(fs *flag.FlagSet) func(*verbCall) error {
			size := fs.Int("size-mb", 0, "size in MB (the server's limits apply)")
			return func(v *verbCall) error {
				if *size <= 0 {
					return errors.New("--size-mb is required")
				}
				body := map[string]any{"size_mb": *size}
				if len(v.pos) == 1 {
					body["name"] = v.pos[0]
				}
				v.body = body
				return nil
			}
		}},
		{sub: "list", method: "GET", path: "/v1/volumes", rows: "volumes", cols: []string{"id", "name", "size_mb", "node_id", "attached_to"}},
		{sub: "get", args: "<id>", method: "GET", path: "/v1/volumes/%s"},
		{sub: "rm", args: "<id> --confirm", method: "DELETE", path: "/v1/volumes/%s", confirm: true},
	}
}

func cronVerbs() []apiVerb {
	return []apiVerb{
		{sub: "list", method: "GET", path: "/v1/cron", rows: "jobs", cols: []string{"id", "name", "sandbox_id", "schedule", "enabled", "next_run_at", "command"}},
		{sub: "create", args: "<sandbox> <schedule> <command> [--name N] [--timezone Z] [--agent-mode guided|autonomous]", method: "POST", path: "/v1/cron", flags: func(fs *flag.FlagSet) func(*verbCall) error {
			name := fs.String("name", "", "label for the job")
			zone := fs.String("timezone", "", "IANA zone the schedule is read in; default UTC")
			mode := fs.String("agent-mode", "", "run the command as an agent prompt: guided or autonomous")
			return func(v *verbCall) error {
				// The control plane routes a job by the id in the body, and only
				// path segments get their slug resolved.
				body := map[string]any{"sandbox_id": canonicalSandboxID(v.cl, v.pos[0]), "schedule": v.pos[1], "command": v.pos[2]}
				for field, val := range map[string]string{"name": *name, "timezone": *zone, "agent_mode": *mode} {
					if val != "" {
						body[field] = val
					}
				}
				v.body = body
				return nil
			}
		}},
		{sub: "rm", args: "<id> --confirm", method: "DELETE", path: "/v1/cron/%s", confirm: true},
		// A job that ran and failed is a 200 that says so in status.
		{sub: "run", args: "<id>", method: "POST", path: "/v1/cron/%s/run", failed: statusNot("ok"), flags: func(*flag.FlagSet) func(*verbCall) error {
			return func(v *verbCall) error {
				slow(v.cl)
				return nil
			}
		}},
		{sub: "toggle", args: "<id> --on|--off", method: "POST", path: "/v1/cron/%s/toggle", flags: func(fs *flag.FlagSet) func(*verbCall) error {
			enabled := onOff(fs, "on", "off", "enable or disable the job")
			return func(v *verbCall) error {
				on, err := enabled()
				v.body = map[string]any{"enabled": on}
				return err
			}
		}},
	}
}

func webhookVerbs() []apiVerb {
	return []apiVerb{
		{sub: "list", method: "GET", path: "/v1/webhooks", rows: "webhooks", cols: []string{"id", "url", "events", "enabled", "last_status", "consecutive_failures"}},
		{sub: "add", args: "<url> --event E ...", method: "POST", path: "/v1/webhooks", flags: webhookFlags(0)},
		{sub: "rm", args: "<id> --confirm", method: "DELETE", path: "/v1/webhooks/%s", confirm: true},
		{sub: "test", args: "<id>", method: "POST", path: "/v1/webhooks/%s/test",
			failed: func(obj map[string]any) bool { return obj["ok"] != true }},
		{sub: "toggle", args: "<id> --on|--off", method: "POST", path: "/v1/webhooks/%s/toggle", flags: func(fs *flag.FlagSet) func(*verbCall) error {
			enabled := onOff(fs, "on", "off", "deliver events or hold them")
			return func(v *verbCall) error {
				on, err := enabled()
				v.body = map[string]any{"enabled": on}
				return err
			}
		}},
		{sub: "deliveries", method: "GET", path: "/v1/webhooks/deliveries", rows: "deliveries", cols: []string{"id", "webhook_id", "event_type", "sandbox_id", "state", "attempts", "last_status", "created_at"}},
	}
}

func functionVerbs() []apiVerb {
	return []apiVerb{
		{sub: "list", method: "GET", path: "/v1/functions", rows: "functions", cols: []string{"name", "template", "start", "port", "replicas", "invokes"}},
		{sub: "create", args: "<name> --start CMD [--template T] [--port N] [--vcpu N] [--mem MB] [--max-replicas N] [--env K=V ...] [--file local[:remote] ...]", method: "POST", path: "/v1/functions", flags: functionCreateFlags},
		{sub: "get", args: "<name>", method: "GET", path: "/v1/functions/%s"},
		{sub: "code", args: "<name>", method: "GET", path: "/v1/functions/%s/code", rows: "files", text: func(v *verbCall, obj map[string]any) {
			files, _ := obj["files"].([]any)
			for _, f := range files {
				file, _ := f.(map[string]any)
				fmt.Fprintf(v.c.stdout, "==> %s\n%s\n", str(file["path"]), str(file["content"]))
			}
		}},
		{sub: "logs", args: "<name>", method: "GET", path: "/v1/functions/%s/logs?limit=500", text: func(v *verbCall, obj map[string]any) {
			printField("lines")(v, obj)
			if note := str(obj["note"]); note != "" && !v.c.quiet {
				fmt.Fprintln(v.c.stderr, "pilldrops: "+note)
			}
		}},
		{sub: "metrics", args: "<name>", method: "GET", path: "/v1/functions/%s/metrics"},
		{sub: "rm", args: "<name> --confirm", method: "DELETE", path: "/v1/functions/%s", confirm: true},
	}
}

func functionCreateFlags(fs *flag.FlagSet) func(*verbCall) error {
	start := fs.String("start", "", "command that serves the function")
	template := fs.String("template", "", "template id; default the server's first")
	port := fs.Int("port", 0, "port the command listens on")
	vcpu := fs.Int("vcpu", 0, "vCPUs (0 = template default)")
	mem := fs.Int("mem", 0, "memory MB (0 = template default)")
	replicas := fs.Int("max-replicas", 0, "burst ceiling (0 = default)")
	var env, files stringList
	fs.Var(&env, "env", "environment variable KEY=VAL, repeatable")
	fs.Var(&files, "file", "local file to include as local[:remote], repeatable")
	return func(v *verbCall) error {
		vars, err := envMap(env)
		if err != nil {
			return err
		}
		specs := []map[string]string{}
		for _, f := range files {
			spec, err := readFileSpec(f)
			if err != nil {
				return err
			}
			specs = append(specs, spec)
		}
		v.body = map[string]any{
			"name": v.pos[0], "template": *template, "start": *start, "port": *port,
			"vcpu": *vcpu, "memory_mb": *mem, "max_replicas": *replicas, "env": vars, "files": specs,
		}
		return nil
	}
}

func projectVerbs() []apiVerb {
	return []apiVerb{
		{sub: "list", method: "GET", path: "/v1/projects", rows: "projects", cols: []string{"id", "name", "created_at", "last_run_at"}},
		{sub: "create", args: "<name> [--plan TEXT]", method: "POST", path: "/v1/projects", flags: func(fs *flag.FlagSet) func(*verbCall) error {
			plan := fs.String("plan", "", "the project's plan, stored as given")
			return func(v *verbCall) error {
				v.body = map[string]any{"name": v.pos[0], "plan": *plan}
				return nil
			}
		}},
		{sub: "get", args: "<id>", method: "GET", path: "/v1/projects/%s"},
		{sub: "rm", args: "<id> --confirm", method: "DELETE", path: "/v1/projects/%s", confirm: true},
		{sub: "runs", args: "<id>", method: "GET", path: "/v1/projects/%s/runs", rows: "runs", cols: []string{"id", "started_at", "ended_at", "services"}},
		{sub: "run", args: "<id> [--service service:sandbox ...]", method: "POST", path: "/v1/projects/%s/runs", flags: func(fs *flag.FlagSet) func(*verbCall) error {
			var services stringList
			fs.Var(&services, "service", "planned service and the sandbox carrying it, repeatable")
			return func(v *verbCall) error {
				rows := []map[string]string{}
				for _, s := range services {
					service, sandbox, ok := strings.Cut(s, ":")
					if !ok || service == "" || sandbox == "" {
						return fmt.Errorf("--service %q is not service:sandbox", s)
					}
					rows = append(rows, map[string]string{"service_id": service, "sandbox_id": canonicalSandboxID(v.cl, sandbox)})
				}
				v.body = map[string]any{"services": rows}
				return nil
			}
		}},
	}
}

func threadVerbs() []apiVerb {
	return []apiVerb{
		{sub: "list", method: "GET", path: "/v1/threads", rows: "threads", cols: []string{"id", "title", "updated_at"}},
		{sub: "create", args: "[--title T]", method: "POST", path: "/v1/threads", flags: func(fs *flag.FlagSet) func(*verbCall) error {
			title := fs.String("title", "", "name for the thread")
			return func(v *verbCall) error {
				v.body = map[string]any{"title": *title}
				return nil
			}
		}},
		{sub: "get", args: "<id>", method: "GET", path: "/v1/threads/%s"},
		{sub: "rm", args: "<id> --confirm", method: "DELETE", path: "/v1/threads/%s", confirm: true},
	}
}

func integrationVerbs() []apiVerb {
	return []apiVerb{{method: "GET", path: "/v1/integrations", rows: "servers", envelope: true,
		cols: []string{"id", "name", "category", "env_keys"}}}
}

func chatVerbs() []apiVerb {
	return []apiVerb{{args: "<id> [message] [--autonomous] [--limit N]", optional: 1, method: "GET", path: "/v1/sandboxes/%s/chat",
		rows: "messages", cols: []string{"role", "text", "at"}, flags: func(fs *flag.FlagSet) func(*verbCall) error {
			autonomous := fs.Bool("autonomous", false, "grant the agent its dangerous flags for this turn")
			limit := fs.Int("limit", 0, "transcript entries to show")
			return func(v *verbCall) error {
				if len(v.pos) == 1 {
					v.path = withQuery(v.path, "limit", *limit)
					return nil
				}
				// The server holds this request open for the whole turn.
				slow(v.cl)
				v.method, v.rows, v.text = "POST", "", printField("reply")
				v.body = map[string]any{"message": v.pos[1], "autonomous": *autonomous}
				return nil
			}
		}}}
}

func chatLinkVerbs() []apiVerb {
	return []apiVerb{
		{args: "<id> [--read-only] [--autonomous]", method: "POST", path: "/v1/sandboxes/%s/chat-link", flags: func(fs *flag.FlagSet) func(*verbCall) error {
			readOnly := fs.Bool("read-only", false, "the link may report but never write or execute")
			autonomous := fs.Bool("autonomous", false, "grant the agent its dangerous flags on this link")
			return func(v *verbCall) error {
				v.body = map[string]any{"read_only": *readOnly, "autonomous": *autonomous}
				return nil
			}
		}},
		{sub: "rm", args: "<id> --confirm", method: "DELETE", path: "/v1/sandboxes/%s/chat-link", confirm: true},
	}
}

func channelVerbs() []apiVerb {
	return []apiVerb{{args: "<id>", method: "GET", path: "/v1/sandboxes/%s/channels", rows: "channels", envelope: true,
		cols: []string{"id", "name", "available", "linked", "reason"}}}
}

func auditVerbs() []apiVerb {
	return []apiVerb{{args: "[--limit N]", method: "GET", path: "/v1/audit", rows: "entries", cols: []string{"at", "action", "target", "key_id"},
		flags: func(fs *flag.FlagSet) func(*verbCall) error {
			limit := fs.Int("limit", 0, "entries to show")
			return func(v *verbCall) error {
				v.path = withQuery(v.path, "limit", *limit)
				return nil
			}
		}}}
}

const sandboxIntegrationsUsage = "usage: pilldrops sandbox integrations <id> [add|rm <server>... [--env K=V ...]]"

// sandboxIntegrations reads or edits one sandbox's MCP servers. The route only
// replaces the whole set, so add and rm read it first.
func sandboxIntegrations(c *cmdCtx, cl *Client, args []string) int {
	fs := flag.NewFlagSet("sandbox integrations", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	var envFlag stringList
	fs.Var(&envFlag, "env", "credential KEY=VAL, repeatable")
	pos, err := parseMixed(fs, args)
	if err != nil {
		fmt.Fprintln(c.stderr, sandboxIntegrationsUsage)
		return ExitUsage
	}
	pos = c.wantID("sandbox integrations", pos)
	if len(pos) == 0 || len(pos) == 2 || (len(pos) > 2 && pos[1] != "add" && pos[1] != "rm") {
		fmt.Fprintln(c.stderr, sandboxIntegrationsUsage)
		return ExitUsage
	}
	env, err := envMap(envFlag)
	if err != nil {
		fmt.Fprintf(c.stderr, "pilldrops sandbox integrations: %v\n", err)
		return ExitUsage
	}
	path := "/v1/sandboxes/" + url.PathEscape(pos[0]) + "/integrations"
	call := &verbCall{c: c, cl: cl, apiVerb: apiVerb{text: printField("servers")}}
	raw, err := call.send("GET", path, nil)
	if err != nil {
		return c.fail(err)
	}
	if len(pos) == 1 {
		return call.respond(raw)
	}
	var current struct {
		Servers []string `json:"servers"`
	}
	if err := json.Unmarshal(raw, &current); err != nil {
		return c.fail(fmt.Errorf("unreadable integrations: %w", err))
	}
	servers := []string{}
	for _, s := range current.Servers {
		if !slices.Contains(pos[2:], s) {
			servers = append(servers, s)
		}
	}
	if pos[1] == "add" {
		servers = append(servers, pos[2:]...)
	}
	body := map[string]any{"servers": servers}
	if env != nil {
		body["env"] = env
	}
	raw, err = call.send("POST", path, body)
	if err != nil {
		return c.fail(err)
	}
	return call.respond(raw)
}
