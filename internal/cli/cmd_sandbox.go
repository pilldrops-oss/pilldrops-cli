package cli

import (
	"flag"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// stringList is a repeatable string flag (--env A=1 --env B=2).
type stringList []string

func (s *stringList) String() string { return fmt.Sprint([]string(*s)) }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// intList is a repeatable int flag (--port 3000 --port 8080).
type intList []int

func (s *intList) String() string { return fmt.Sprint([]int(*s)) }
func (s *intList) Set(v string) error {
	n, err := strconv.Atoi(v)
	if err != nil {
		return err
	}
	*s = append(*s, n)
	return nil
}

// knownSandboxSub is the set of valid `sandbox` subcommands. Validated before
// building the client so an unknown subcommand is a usage error (exit 2) even
// when the user is not logged in.
var knownSandboxSub = map[string]bool{
	"create": true, "ls": true, "get": true, "open": true,
	"rm": true, "pause": true, "resume": true,
	"expose": true, "forwards": true, "unexpose": true,
	"fork": true, "share": true, "unshare": true,
	"ports": true, "set-port": true, "rename": true, "networks": true,
	"standby": true, "refresh": true,
	// hibernate is pause under the name the other tool gives the same act —
	// writing memory to disk. Their `pause` is our `standby`, so a migrating
	// user's `pause` cannot be rescued without breaking every script that
	// already types it; `hibernate` at least lands their cold verb on ours,
	// and it is the word our own set-hibernate timer already uses.
	"hibernate":   true,
	"set-timeout": true, "set-idle": true, "set-hibernate": true,
}

// sandboxSubUsage is the full subcommand list, in the order the top-level help
// introduces them. Keep it in step with knownSandboxSub above.
const sandboxSubUsage = "create|ls|get|open|rename|rm|pause|hibernate|standby|resume|refresh|fork|share|unshare|ports|set-port|set-timeout|set-idle|set-hibernate|expose|forwards|unexpose|networks"

// sandboxVisibility flips a sandbox between private and org-visible. Sharing
// removes the agent logins inside it first and prints what went; unsharing
// restores nothing, so it says so.
func sandboxVisibility(c *cmdCtx, cl *Client, sub string, args []string) int {
	if len(args) != 1 {
		fmt.Fprintf(c.stderr, "usage: pilldrops sandbox %s <id>\n", sub)
		return ExitUsage
	}
	want := "org"
	if sub == "unshare" {
		want = "private"
	}
	var out map[string]any
	if err := cl.JSON("POST", "/v1/sandboxes/"+args[0]+"/visibility",
		map[string]any{"visibility": want}, &out); err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, out)
		return ExitOK
	}
	fmt.Fprintf(c.stdout, "%s\t%s\n", args[0], str(out["visibility"]))
	if wiped, ok := out["wiped"].([]any); ok {
		for _, p := range wiped {
			fmt.Fprintf(c.stdout, "  wiped %s\n", str(p))
		}
		fmt.Fprintln(c.stdout, "  whoever was logged in there must log in again; nothing is kept to restore")
	}
	if skipped := str(out["wipe_skipped"]); skipped != "" {
		fmt.Fprintf(c.stderr, "  WARNING: the agent logins were NOT removed: %s\n", skipped)
	}
	if out["relogin"] == true {
		fmt.Fprintln(c.stdout, "  private again; the logins removed when it was shared are not coming back")
	}
	return ExitOK
}

func cmdSandbox(c *cmdCtx, args []string) int {
	// Taken out before the positionals are read, so --confirm works wherever
	// the user put it — the same handling checkpoint and backup give it.
	args, confirmed := takeConfirm(args)
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "usage: pilldrops sandbox "+sandboxSubUsage)
		return ExitUsage
	}
	sub, subArgs := args[0], args[1:]
	if !knownSandboxSub[sub] {
		fmt.Fprintf(c.stderr, "pilldrops sandbox: unknown subcommand %q\n", sub)
		return ExitUsage
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}

	// id-taking one-shot verbs share a tiny runner.
	idVerb := func(method, pathFmt string) int {
		subArgs = c.wantID("sandbox "+sub, subArgs)
		if len(subArgs) != 1 {
			fmt.Fprintf(c.stderr, "usage: pilldrops sandbox %s <id>\n", sub)
			return ExitUsage
		}
		var out map[string]any
		if err := cl.JSON(method, fmt.Sprintf(pathFmt, subArgs[0]), nil, &out); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, out)
		} else {
			fmt.Fprintln(c.stdout, sandboxState(out))
		}
		return ExitOK
	}

	switch sub {
	case "expose":
		return sandboxExpose(c, cl, subArgs)
	case "forwards":
		return sandboxForwards(c, cl, subArgs)
	case "unexpose":
		return sandboxUnexpose(c, cl, subArgs)
	case "create":
		fs := flag.NewFlagSet("create", flag.ContinueOnError)
		fs.SetOutput(c.stderr)
		tmpl := fs.String("template", "", "template id (see `pilldrops templates`)")
		snapshot := fs.String("from-snapshot", "", "wake from a saved snapshot instead of booting a template (`pilldrops snapshot list`); it carries its own template, size and runtime")
		image := fs.String("image", "", "custom container image (e.g. ghcr.io/you/app:tag) instead of a template")
		start := fs.String("start", "", "long-lived start command (server/app) — its port becomes a preview URL")
		vcpu := fs.Int("vcpu", 0, "vCPUs (0 = template default)")
		mem := fs.Int("mem", 0, "memory MB (0 = template default)")
		storage := fs.Int("storage", 0, "disk MB (0 = template default)")
		timeout := fs.Int("timeout", 0, "lifetime seconds (0 = plan default)")
		idle := fs.Int("idle", 0, "idle auto-pause seconds (0 = default ~5m, -1 = never / always-on)")
		runtimePin := fs.String("runtime", "", "pin the node runtime: firecracker (microVM isolation) | docker (container); omit for any")
		public := fs.Bool("public", false, "make the preview URL reachable without a token (shareable link); default: token-gated")
		private := fs.Bool("private", false, "only your key (and operators) may see this sandbox; default: everyone in your org")
		slug := fs.String("slug", "", "DNS label the sandbox is hosted under (https://<slug>.<domain>/) and accepted wherever an id is; unique")
		networks := fs.String("networks", "", "comma-separated network tags to join; two sandboxes reach each other only while they share one")
		var ports intList
		fs.Var(&ports, "port", "published app port for preview, repeatable (your app must bind $PORT); omit to use the default app port")
		var env stringList
		fs.Var(&env, "env", "environment variable KEY=VAL, repeatable")
		if err := fs.Parse(subArgs); err != nil {
			return ExitUsage
		}
		// client tags this create as agent-originated (CLI) for creation attribution.
		req := map[string]any{"template": *tmpl, "vcpu": *vcpu, "memory_mb": *mem, "timeout": *timeout, "client": "cli"}
		if *idle != 0 {
			req["idle_timeout"] = *idle
		}
		if *runtimePin != "" {
			req["runtime"] = *runtimePin
		}
		if *image != "" {
			req["image"] = *image
		}
		if *snapshot != "" {
			// The snapshot carries the template it was captured from, and
			// naming both is a 400 — so --template is simply not sent.
			req["snapshot"] = *snapshot
		}
		if *start != "" {
			req["start"] = *start
		}
		if *storage > 0 {
			req["storage_mb"] = *storage
		}
		if len(ports) > 0 {
			req["ports"] = []int(ports)
		}
		if len(env) > 0 {
			req["env"] = []string(env)
		}
		if *public {
			req["public"] = true // opt-in: preview URL reachable without a token
		}
		if *private {
			// Who may reach the API for this sandbox; --public is about the preview URL.
			req["visibility"] = "private"
		}
		if *slug != "" {
			req["slug"] = *slug
		}
		// Absent and [] mean the same thing to the API (on no network), so an
		// unset flag sends nothing rather than an empty list.
		if tags := splitTags(*networks); len(tags) > 0 {
			req["networks"] = tags
		}
		var out map[string]any
		if err := cl.JSON("POST", "/v1/sandboxes", req, &out); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, out)
		} else {
			fmt.Fprintf(c.stdout, "%s\t%s\t%s\n", str(out["id"]), str(out["template"]), sandboxState(out))
			if u := str(out["url"]); u != "" {
				fmt.Fprintf(c.stdout, "preview: %s\n", u)
				// The preview only reaches an app bound to the injected $PORT. A
				// hardcoded port 502s — the single most common preview mistake.
				fmt.Fprintln(c.stdout, "  (your app must listen on $PORT, which the sandbox injects)")
			}
		}
		return ExitOK

	case "ls":
		var out struct {
			Sandboxes []map[string]any `json:"sandboxes"`
		}
		if err := cl.JSON("GET", "/v1/sandboxes", nil, &out); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, out.Sandboxes)
			return ExitOK
		}
		rows := [][]string{{"ID", "TEMPLATE", "STATUS", "VISIBILITY"}}
		for _, sb := range out.Sandboxes {
			rows = append(rows, []string{str(sb["id"]), str(sb["template"]), sandboxState(sb), str(sb["visibility"])})
		}
		table(c.stdout, rows)
		return ExitOK

	case "get":
		subArgs = c.wantID("sandbox get", subArgs)
		if len(subArgs) != 1 {
			fmt.Fprintln(c.stderr, "usage: pilldrops sandbox get <id>")
			return ExitUsage
		}
		var out map[string]any
		if err := cl.JSON("GET", "/v1/sandboxes/"+subArgs[0], nil, &out); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, out)
		} else {
			rows := [][]string{}
			for _, k := range []string{"id", "slug", "template", "status", "visibility", "created_by",
				"created_at", "expires_at", "url", "networks", "private_ip", "node"} {
				if v, ok := out[k]; ok {
					rows = append(rows, []string{k, str(v)})
				}
			}
			table(c.stdout, rows)
			// `url` is one port. Every port has its own hostname, and nobody
			// knew, so the pattern is spelled out next to the one URL we print.
			if t := perPortURL(firstNonEmpty(out, "url", "app_url"), "<port>"); t != "" {
				fmt.Fprintf(c.stdout, "per-port\t%s\n", t)
				fmt.Fprintf(c.stdout, "\t`pilldrops sandbox ports %s` lists the ports with a listener, with a link each\n", subArgs[0])
			}
		}
		return ExitOK

	case "open":
		fs := flag.NewFlagSet("open", flag.ContinueOnError)
		fs.SetOutput(c.stderr)
		printOnly := fs.Bool("print", false, "print the viewer URL without opening a browser")
		// The missing verb. A preview token is bound to ONE port, so a link the
		// platform handed back for some other port cannot be re-pointed at the
		// server you just started — which left `preview-token?port=N`, named in
		// no help text, as the only way to reach your own app.
		port := fs.Int("port", 0, "open this port of the sandbox (default: the sandbox's published port)")
		// Either order. flag stops at the first non-flag argument, so
		// `open <id> --print` — the order the help has always advertised — used
		// to be a usage error with the flag silently unset. rename solves it the
		// same way, by parsing past a leading id.
		flagArgs, openArgs := subArgs, []string(nil)
		if len(subArgs) > 0 && !strings.HasPrefix(subArgs[0], "-") {
			flagArgs, openArgs = subArgs[1:], subArgs[:1]
		}
		if err := fs.Parse(flagArgs); err != nil {
			fmt.Fprintln(c.stderr, openUsage)
			return ExitUsage
		}
		openArgs = c.wantID("sandbox open", append(openArgs, fs.Args()...))
		if len(openArgs) != 1 {
			fmt.Fprintln(c.stderr, openUsage)
			return ExitUsage
		}
		if *port < 0 || *port > 65535 {
			fmt.Fprintln(c.stderr, "pilldrops sandbox open: --port must be 1-65535")
			return ExitUsage
		}
		var out map[string]any
		if err := cl.JSON("GET", "/v1/sandboxes/"+openArgs[0], nil, &out); err != nil {
			return c.fail(err)
		}
		u := openURL(cl, openArgs[0], out, *port)
		if u == "" {
			if *port > 0 {
				fmt.Fprintf(c.stderr, "sandbox has no preview URL to point at port %d "+
					"(it has no published port at all)\n", *port)
			} else {
				fmt.Fprintln(c.stderr, "sandbox has no preview URL (no published port)")
			}
			return ExitAPI
		}
		if c.jsonOut {
			printJSON(c.stdout, map[string]any{"id": openArgs[0], "url": u})
		} else {
			fmt.Fprintln(c.stdout, u)
		}
		if !*printOnly {
			if err := openBrowser(u); err != nil {
				fmt.Fprintf(c.stderr, "could not open a browser: %v\n", err)
			}
		}
		return ExitOK

	case "share", "unshare":
		return sandboxVisibility(c, cl, sub, subArgs)

	case "fork":
		fs := flag.NewFlagSet("fork", flag.ContinueOnError)
		fs.SetOutput(c.stderr)
		name := fs.String("name", "", "label for the copy")
		timeout := fs.Int("timeout", 0, "lifetime seconds (0 = node default, -1 = persistent)")
		shared := fs.Bool("shared", false, "make the copy visible to the whole org; default private to you")
		if err := fs.Parse(subArgs); err != nil || fs.NArg() != 1 {
			fmt.Fprintln(c.stderr, "usage: pilldrops sandbox fork [--name <label>] [--timeout <s>] [--shared] <id>")
			return ExitUsage
		}
		req := map[string]any{"name": *name, "timeout": *timeout}
		if *shared {
			// Only sent when asked: the node's default for a fork is private.
			req["visibility"] = "org"
		}
		var out map[string]any
		if err := cl.JSON("POST", "/v1/sandboxes/"+fs.Arg(0)+"/fork", req, &out); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, out)
		} else {
			fmt.Fprintf(c.stdout, "%s\t%s\t%s\n", str(out["id"]), sandboxState(out), str(out["visibility"]))
		}
		return ExitOK

	case "ports":
		subArgs = c.wantID("sandbox ports", subArgs)
		if len(subArgs) != 1 {
			fmt.Fprintln(c.stderr, "usage: pilldrops sandbox ports <id>")
			return ExitUsage
		}
		var out struct {
			Ports []struct {
				Port      int  `json:"port"`
				Listening bool `json:"listening"`
				// Added here, not by the platform: /ports reports listeners only.
				URL string `json:"url,omitempty"`
			} `json:"ports"`
		}
		if err := cl.JSON("GET", "/v1/sandboxes/"+subArgs[0]+"/ports", nil, &out); err != nil {
			return c.fail(err)
		}
		// The platform serves a hostname per port and nothing ever printed it,
		// so the feature was invisible. The shape is taken from the sandbox's
		// own preview URL — see perPortURL — and the token that opens a private
		// preview is minted by the platform, not guessed.
		var sb map[string]any
		_ = cl.JSON("GET", "/v1/sandboxes/"+subArgs[0], nil, &sb) //nolint:errcheck // best-effort: no base URL just means no per-port link
		base := firstNonEmpty(sb, "url", "app_url", "viewer_url")
		for i, p := range out.Ports {
			if !p.Listening {
				continue
			}
			if u := perPortURL(base, strconv.Itoa(p.Port)); u != "" {
				out.Ports[i].URL = u + previewTokenQuery(cl, subArgs[0], p.Port)
			}
		}
		if c.jsonOut {
			printJSON(c.stdout, out.Ports)
			return ExitOK
		}
		// Every probed port, not only the listeners. A run that printed nothing
		// and exited 0 was the same output as a broken command, and it is what
		// you get on a sandbox whose server is not up yet — which is exactly
		// when you are running this. The LISTENING column is what distinguishes
		// "probed, nothing there" from "not probed".
		rows, listening := [][]string{{"PORT", "LISTENING", "URL"}}, false
		for _, p := range out.Ports {
			rows = append(rows, []string{strconv.Itoa(p.Port), yesNo(p.Listening), p.URL})
			listening = listening || p.Listening
		}
		if len(rows) > 1 {
			table(c.stdout, rows)
		}
		if !listening {
			// Not an error: nothing failed, the sandbox simply has no server up.
			// Said on stderr so a pipeline still reads a clean (empty) table.
			fmt.Fprintf(c.stderr, "no port has a listener yet — the platform probes a fixed "+
				"candidate list, so start your server and re-run, or ask for any port with "+
				"`pilldrops sandbox open --port <n> %s`\n", subArgs[0])
		}
		return ExitOK

	case "set-port":
		if len(subArgs) != 2 {
			fmt.Fprintln(c.stderr, "usage: pilldrops sandbox set-port <id> <port>")
			return ExitUsage
		}
		port, err := strconv.Atoi(subArgs[1])
		if err != nil || port < 1 || port > 65535 {
			fmt.Fprintln(c.stderr, "pilldrops sandbox set-port: port must be 1-65535")
			return ExitUsage
		}
		return patchSandbox(c, cl, subArgs[0], map[string]any{"port": port})

	case "rename":
		fs := flag.NewFlagSet("rename", flag.ContinueOnError)
		fs.SetOutput(c.stderr)
		name := fs.String("name", "", "human label (empty string clears it)")
		slug := fs.String("slug", "", "DNS label for https://<slug>.<domain>/ (empty string clears it)")
		// The id comes first; flag stops at the first non-flag argument.
		if len(subArgs) == 0 {
			fmt.Fprintln(c.stderr, "usage: pilldrops sandbox rename <id> [--name <label>] [--slug <slug>]")
			return ExitUsage
		}
		if err := fs.Parse(subArgs[1:]); err != nil || fs.NArg() != 0 {
			fmt.Fprintln(c.stderr, "usage: pilldrops sandbox rename <id> [--name <label>] [--slug <slug>]")
			return ExitUsage
		}
		patch := map[string]any{}
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "name":
				patch["name"] = *name
			case "slug":
				patch["slug"] = *slug
			}
		})
		if len(patch) == 0 {
			fmt.Fprintln(c.stderr, "pilldrops sandbox rename: nothing to change (pass --name or --slug)")
			return ExitUsage
		}
		return patchSandbox(c, cl, subArgs[0], patch)

	case "networks":
		// Replaces, never adds: the API's PATCH is a replacement, and a CLI that
		// read as cumulative would quietly keep a network the user dropped.
		fs := flag.NewFlagSet("networks", flag.ContinueOnError)
		fs.SetOutput(c.stderr)
		clear := fs.Bool("clear", false, "remove the sandbox from every network")
		rest := subArgs
		for i, a := range subArgs {
			if strings.HasPrefix(a, "-") {
				if err := fs.Parse(subArgs[i:]); err != nil {
					return ExitUsage
				}
				rest = subArgs[:i]
				break
			}
		}
		if len(rest) < 1 || (len(rest) < 2 && !*clear) {
			fmt.Fprintln(c.stderr, "usage: pilldrops sandbox networks <id> <a,b> | <id> --clear")
			return ExitUsage
		}
		tags := []string{}
		if !*clear {
			tags = splitTags(rest[1])
		}
		var out map[string]any
		if err := cl.JSON("PATCH", "/v1/sandboxes/"+rest[0], map[string]any{"networks": tags}, &out); err != nil {
			return c.fail(err)
		}
		switch {
		case c.jsonOut:
			printJSON(c.stdout, out)
		case len(tags) == 0:
			fmt.Fprintf(c.stdout, "%s is on no network\n", str(out["id"]))
		default:
			fmt.Fprintf(c.stdout, "%s\t%s\t%s\n", str(out["id"]), strings.Join(tags, ","), str(out["private_ip"]))
		}
		return ExitOK

	case "set-timeout":
		// Seconds of hard lifetime from now; -1 asks for a persistent
		// workstation. The plan may clamp it, so the new expiry is read back.
		secs, ok := secondsArg(c, sub, subArgs)
		if !ok {
			return ExitUsage
		}
		if err := cl.JSON("POST", "/v1/sandboxes/"+subArgs[0]+"/timeout",
			map[string]any{"seconds": secs}, nil); err != nil {
			return c.fail(err)
		}
		var sb map[string]any
		if err := cl.JSON("GET", "/v1/sandboxes/"+subArgs[0], nil, &sb); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, map[string]any{"id": subArgs[0], "expires_at": sb["expires_at"]})
		} else if !c.quiet {
			// The route answers {"status":"ok"} whatever it granted, so the
			// expiry is the only place a clamp is visible.
			fmt.Fprintf(c.stdout, "%s\texpires_at %s\n", subArgs[0], str(sb["expires_at"]))
		}
		return ExitOK

	case "set-idle", "set-hibernate":
		// idle → standby (frozen in RAM), hibernate → pause (written to disk).
		// -1 never, 0 the platform default; the node clamps a short value.
		secs, ok := secondsArg(c, sub, subArgs)
		if !ok {
			return ExitUsage
		}
		field := "idle_timeout"
		if sub == "set-hibernate" {
			field = "hibernate_timeout"
		}
		var out map[string]any
		if err := cl.JSON("PATCH", "/v1/sandboxes/"+subArgs[0], map[string]any{field: secs}, &out); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, out)
		} else if !c.quiet {
			fmt.Fprintf(c.stdout, "%s\t%s %s\n", str(out["id"]), field, str(out[field]))
		}
		return ExitOK

	case "rm":
		// The machine and its disk go, and nothing brings them back: the same
		// rule snapshot rm, checkpoint restore and backup restore already
		// apply. WHICH sandbox may be asked for on a terminal; WHETHER to
		// destroy it may not — see the note on takeConfirm.
		subArgs = c.wantID("sandbox rm", subArgs)
		if len(subArgs) != 1 {
			fmt.Fprintln(c.stderr, "usage: pilldrops sandbox rm <id> --confirm")
			return ExitUsage
		}
		if !confirmed {
			fmt.Fprintf(c.stderr, "pilldrops sandbox rm: %s and everything on its disk are destroyed, and nothing restores them — re-run: pilldrops sandbox rm %s --confirm\n", subArgs[0], subArgs[0])
			return ExitUsage
		}
		return idVerb("DELETE", "/v1/sandboxes/%s")
	case "pause", "hibernate":
		// Cold: memory is written to disk and freed. NOT the other tool's
		// `pause`, which is warm — that one is our `standby`, below.
		return idVerb("POST", "/v1/sandboxes/%s/pause")
	case "resume":
		return idVerb("POST", "/v1/sandboxes/%s/resume")
	case "standby":
		// Warm: RAM, processes and sockets are held, so it still bills memory.
		return idVerb("POST", "/v1/sandboxes/%s/standby")
	case "refresh":
		return idVerb("POST", "/v1/sandboxes/%s/refresh")
	}
	return ExitUsage // unreachable: knownSandboxSub gates the switch
}

// secondsArg parses the shared "<id> <seconds>" head of the timer verbs.
func secondsArg(c *cmdCtx, sub string, args []string) (int, bool) {
	if len(args) != 2 {
		fmt.Fprintf(c.stderr, "usage: pilldrops sandbox %s <id> <seconds>\n", sub)
		return 0, false
	}
	n, err := strconv.Atoi(args[1])
	if err != nil {
		fmt.Fprintf(c.stderr, "pilldrops sandbox %s: seconds must be a number (-1 = never)\n", sub)
		return 0, false
	}
	return n, true
}

// splitTags parses a comma-separated tag list, dropping empties so a trailing
// comma is not sent as a tag the API then refuses.
func splitTags(s string) []string {
	out := []string{}
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// patchSandbox sends PATCH /v1/sandboxes/{id} and prints the updated sandbox.
func patchSandbox(c *cmdCtx, cl *Client, id string, patch map[string]any) int {
	var out map[string]any
	if err := cl.JSON("PATCH", "/v1/sandboxes/"+id, patch, &out); err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, out)
		return ExitOK
	}
	fmt.Fprintf(c.stdout, "%s\t%s\t%s\n", str(out["id"]), str(out["name"]), sandboxState(out))
	if u := str(out["url"]); u != "" {
		fmt.Fprintf(c.stdout, "preview: %s\n", u)
	}
	return ExitOK
}

// previewPathRe matches the path-form preview URL, the shape a deployment with
// no sandbox domain serves.
var previewPathRe = regexp.MustCompile(`^/preview/([^/]+)/[0-9]+/?`)

// perPortURL restates a sandbox's own preview URL at another port.
//
// The platform has always served a hostname per port — the wildcard ingress
// parses <port>-<id>.<domain> (internal/orchestrator/ingress.go, appPreviewOrigin)
// and the same pair is what appURL/previewURL compose — plus the
// /preview/<id>/<port>/ path form where no sandbox domain is configured. Only
// the port is moved here: base is whichever form the platform itself handed
// back in `url`, so no hostname is invented. "" when base is neither form
// (a sandbox with no published port has no URL to restate).
func perPortURL(base, port string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return ""
	}
	u.RawQuery, u.Fragment = "", ""
	if m := previewPathRe.FindStringSubmatch(u.Path); m != nil {
		u.Path = "/preview/" + m[1] + "/" + port + "/"
		return u.String()
	}
	label, rest, ok := strings.Cut(u.Hostname(), "-")
	// A slug host (<slug>.<domain>) has no port label and is not restated:
	// one slug points at one port, by design.
	if !ok || label == "" || strings.TrimLeft(label, "0123456789") != "" {
		return ""
	}
	u.Host = port + "-" + rest
	if p := u.Port(); p != "" {
		u.Host += ":" + p
	}
	u.Path = "/"
	return u.String()
}

const openUsage = "usage: pilldrops sandbox open [--print] [--port <n>] <id>"

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// openURL is the link `sandbox open` hands over: one that actually opens.
//
// port 0 means the sandbox's own published port, which is the one the platform
// composed `url` on. Whatever the port, the token is minted for THAT port —
// a preview token is port-bound, so the link the platform happened to return is
// not re-pointable and `set-port` could not change what `open` returned.
//
// `url` first, not `viewer_url`/`desktop_url` first. A desktop sandbox's own
// `url` is already the desktop port (the template publishes 6080), so putting
// the desktop first only ever mattered for sandboxes that do not have one — and
// there it handed out a 6080 link to a port nothing was listening on, which is
// what made `open` useless for the app you had just started. The pre-tokened
// links stay as the fallback for a deployment whose mint route is absent.
func openURL(cl *Client, id string, sb map[string]any, port int) string {
	base := firstNonEmpty(sb, "url", "app_url", "viewer_url", "desktop_url")
	if base == "" {
		return ""
	}
	if port == 0 {
		if u, err := url.Parse(base); err == nil {
			port = previewPortOf(u)
		}
	}
	if u := perPortURL(base, strconv.Itoa(port)); u != "" {
		return u + previewTokenQuery(cl, id, port)
	}
	// Neither preview form — a slug host, say — so the port cannot be moved.
	return firstNonEmpty(sb, "viewer_url", "url", "app_url")
}

// previewTokenQuery asks the platform for the browser token that opens one
// port, and renders it as the query the preview gate checks. Best effort: a
// deployment without the mint route, or a caller it refuses, gets the bare
// URL — which is exactly what a public preview needs anyway.
func previewTokenQuery(cl *Client, id string, port int) string {
	var out struct {
		Token string `json:"token"`
	}
	if err := cl.JSON("GET", fmt.Sprintf("/v1/sandboxes/%s/preview-token?port=%d", id, port), nil, &out); err != nil || out.Token == "" {
		return ""
	}
	return "?token=" + url.QueryEscape(out.Token)
}

// firstNonEmpty returns the first of keys that the map has a non-empty value
// for. Which key carries a URL depends on which tier answered.
func firstNonEmpty(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := str(m[k]); v != "" {
			return v
		}
	}
	return ""
}

// sandboxState is where a sandbox record keeps its lifecycle state. The API
// answers with `state` — the daemon's own field name, and what both list paths
// emit — while create and get-by-id add `status` as an alias on top. Reading
// only the alias is why `sandbox ls` printed an empty STATUS column for every
// row it has ever listed.
func sandboxState(m map[string]any) string { return firstNonEmpty(m, "state", "status") }

// str renders a JSON map value for table cells.
func str(v any) string {
	if v == nil {
		return ""
	}
	// encoding/json decodes every number as float64, so a whole one renders as
	// 1.79014642e+09 in a table unless it is put back. Timestamps, ports and
	// byte counts all come through here.
	if f, ok := v.(float64); ok && f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return fmt.Sprint(v)
}
