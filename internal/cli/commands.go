package cli

import (
	"fmt"
	"io"
	"strings"
)

// command is one top-level verb. This table is the only place a verb is
// declared: Run dispatches from it, `pilldrops help` prints it, and
// `pilldrops ai` — the catalog agents read — is rendered from it. Before, the
// same list was typed in three places and the catalog had drifted far enough to
// advertise commands that no longer existed and miss ones that did.
type command struct {
	name    string
	aliases []string
	run     func(*cmdCtx, []string) int

	// subs are the sub-verbs this command's own switch accepts. Listed here
	// because nothing can read a switch; they are what the catalog's machine
	// -readable command list is built from.
	subs []string

	// help is the block printed under this verb by both `help` and `ai`.
	// Lines after the first are indented to the description column.
	help string

	// example is one runnable invocation, executed against a live control
	// plane by TestCatalogExamplesRun (build tag `live`). "$SB" is replaced
	// with a sandbox created for the run. An example nobody runs rots into
	// something that reads well and 404s.
	example string

	// humanOnly keeps a verb in `help` and out of the agent catalog.
	humanOnly bool

	// feature is the optional group GET /v1/capabilities must list for this verb
	// to appear and run. Empty means every server has it.
	feature string

	// optional are the parts of a core verb that need a group of their own.
	optional []optionalPart
}

// optionalPart adds to a core verb's sub-verbs and help when its feature exists.
type optionalPart struct {
	feature, sub, help string
}

// needs marks a table entry as belonging to an optional group.
func (c command) needs(feature string) command {
	c.feature = feature
	return c
}

// commandTable is a function, not a var: `ai` is itself a command, so the
// table refers to cmdAI which renders the table, and a package-level var
// would be an initialisation cycle.
func commandTable() []command {
	return []command{
		{
			name: "login", run: cmdLogin,
			help: `login [--key K] [--api-url URL]   verify and store a credential. On a terminal, with
                       no --key, it asks for the server and the key (echo off);
                       anywhere else --key is required, or it exits 2`,
			example: `pilldrops --json login --key $PILLDROPS_API_KEY`,
		},
		{
			name: "logout", run: cmdLogout,
			help:    `logout                 remove the stored credential`,
			example: `pilldrops --json whoami`, // logout would strip the credential the rest of the run needs
		},
		{
			name: "whoami", run: cmdWhoami,
			help: `whoami                 the principal this credential is: a key, an operator, or a
                       sandbox acting for itself`,
			example: `pilldrops --json whoami`,
		},
		{
			name: "keys", run: cmdKeys, subs: []string{"create", "list", "rm"},
			help: `keys create <name> | list | rm <id>   API keys for this org. create prints the
                       only copy of the secret; there are no scopes and no expiry`,
			example: `pilldrops --json keys list`,
		},
		{
			name: "capabilities", aliases: []string{"caps"}, run: cmdCapabilities,
			help: `capabilities           what this deployment can actually do (tag_networks,
                       egress_domains, checkpoint, standby, …) — ask before
                       requesting something it would refuse with a 501`,
			example: `pilldrops --json capabilities`,
		},
		{
			name: "limits", run: cmdLimits,
			help:    `limits                 the plan ceilings this credential is subject to`,
			example: `pilldrops --json limits`,
		},
		{
			name: "templates", run: cmdTemplates,
			help:    `templates              list available templates`,
			example: `pilldrops --json templates`,
		},
		{
			name: "sandbox", run: cmdSandbox,
			subs: []string{"create", "ls", "get", "rm", "pause", "resume", "fork", "open", "ports",
				"set-port", "rename", "expose", "forwards", "unexpose", "networks", "standby",
				"refresh", "set-timeout", "set-idle", "set-hibernate", "share", "unshare"},
			optional: []optionalPart{
				{feature: "volume", help: `sandbox create --volume <id>:<path> (xN)   attach a volume`},
				{feature: "integrations", sub: "integrations", help: `sandbox integrations <id> [add|rm <server>... [--env K=V ...]]   its MCP servers. The
                       API replaces the whole set and keeps no credentials, so
                       pass --env again for every server that still needs one`},
			},
			help: `sandbox create|ls|get|rm|pause|resume|fork
    create flags: --template --from-snapshot --image --start --port (xN) --env (xN)
                  --vcpu --mem --storage --timeout --idle --slug --networks
sandbox open <id> [--print] [--port <n>]   open the sandbox's app in a browser; --port opens any port you started a server on (flags in either order)
sandbox ports <id>     every port the platform probes, which are listening, and an HTTPS link for each
                       (https://<port>-<id>.<domain>/); set-port <id> <port>
                       moves the preview that url and open point at
sandbox rename <id> [--name <label>] [--slug <slug>]  a slug is a DNS label:
                       https://<slug>.<domain>/, and it is accepted wherever an <id> is
sandbox expose <id> <port> [--tcp] [--udp]   raw public TCP/UDP endpoint;
                       sandbox forwards <id> lists them; unexpose <id> <port> removes one
sandbox networks <id> <a,b> | <id> --clear   replace the network tags it joins;
                       two sandboxes reach each other only while they share one
sandbox standby <id>   the WARM freeze: RAM, processes and sockets are held and
                       it wakes in ms, so its memory stays allocated. The cold one
                       is sandbox pause
sandbox pause <id>     the COLD freeze (alias: hibernate): memory is written to
                       disk and freed, so it holds no RAM and waking
                       is a restore. The warm one is sandbox standby.
                       NB other tools use "pause" for the warm freeze — if you
                       meant that one, you want standby
sandbox resume <id>    brings either one back
sandbox refresh <id>   keep-alive: re-stamp the lifetime and the idle clock
sandbox set-timeout <id> <secs>   hard lifetime from now (-1 = persistent, plan
                       permitting); set-idle <id> <secs> is the standby timer
                       and set-hibernate <id> <secs> the pause-to-disk one
                       (-1 = never, 0 = platform default)
sandbox share|unshare <id>    org-visible or private; share wipes the agent
                       logins inside first, and unshare does not restore them
                       (a public link to an app on this machine: pilldrops share <port>)
sandbox rm <id> --confirm     destroy it and its disk; --confirm is required
                       and is never prompted for, as on every destructive verb.
                       A missing <id> IS asked for, on a terminal only`,
			example: `pilldrops --json sandbox get $SB`,
		},
		{
			name: "exec", run: cmdExec,
			help: `exec <id> [--env K=V]... [--stdin] [--detach] -- <cmd...>
                       run a command (CLI exits with its exit code; --detach
                       backgrounds it, prints pid). --stdin (-i) pipes this
                       command's input to it — without it a pipe is not read,
                       so a pipeline or CI step never waits on one that has
                       nothing to send`,
			example: `pilldrops --json exec $SB -- echo catalog-example`,
		},
		{
			name: "connect", aliases: []string{"ssh"}, run: cmdConnect,
			help: `connect <id>           interactive shell (a real PTY: editors, pagers, REPLs and
                       installers work, unlike exec which buffers one command).
                       ssh is an alias. --print emits the websocket URL instead`,
			// A session blocks forever; --print exercises the whole lookup —
			// GET the sandbox, find terminal_url, build the websocket URL —
			// without holding the sweep open.
			example: `pilldrops --json connect $SB --print`,
		},
		{
			name: "share", run: cmdShare, humanOnly: true,
			help: `share <port> [--ttl 8h]   a public URL for an app on this machine, until you press
                       Ctrl-C. Anyone with the link can open it. Not the same as
                       sandbox share <id>, which sets a sandbox's visibility`,
			example: `pilldrops --json version`, // it runs until stopped, so the sweep cannot hold it
		},
		{
			name: "shell", run: cmdShell, humanOnly: true,
			help: `shell                  a pilldrops> prompt for every command here, for people at a
                       terminal; bare pilldrops opens it too`,
			example: `pilldrops --json version`, // a prompt needs a terminal to read from
		},
		{
			name: "test", run: cmdTest,
			help: `test <script.sh>       run a mobile E2E script in a fresh mobile-test sandbox:
                       auto screen-recording, live progress, report at the
                       sandbox URL (flags: --template --sandbox --timeout --no-wait)`,
			// A real run needs a script and the mobile-test template and takes
			// minutes, so the sweep checks the template it depends on exists.
			example: `pilldrops --json templates`,
		},
		{
			name: "start", run: cmdStart,
			help:    `start <id> [--port N] -- <cmd...>  launch a long-lived server (survives idle-pause/resume)`,
			example: `pilldrops --json start $SB --port 8000 -- sleep 30`,
		},
		{
			name: "logs", run: cmdLogs,
			help:    `logs <id> [-f]         tail (and follow) sandbox logs`,
			example: `pilldrops --json logs $SB`,
		},
		{
			name: "cp", run: cmdCp,
			help: `cp [-r] <src> <dst>    copy files; sandbox side is <id>:<path>, "-" is stdin/stdout,
                       relative and ~/ paths are the sandbox home; -r copies a
                       directory under the destination (like scp)`,
			example: `pilldrops cp $SB:.bashrc -`,
		},
		{
			name: "env", run: cmdEnv, subs: []string{"set", "list", "scope", "rm"},
			help: `env set NAME VALUE [--secret] [--auto]  store a vault entry (--secret seals it,
                       --auto injects it into every create); env list [--scope S];
                       env scope NAME all|shared|private; env rm NAME
                       (the ORG vault — not a sandbox's own environment)`,
			example: `pilldrops --json env list`,
			feature: controlPlaneFeature,
		},
		{
			name: "domain", aliases: []string{"domains"}, run: cmdDomain, subs: []string{"add", "list", "rm"},
			help: `domain add <host> --sandbox <id> [--port N] [--confirm]  put your own hostname
                       in front of a public sandbox (prints the DNS record and
                       waits for it); domain list; domain rm <host>`,
			example: `pilldrops --json domain list`,
			feature: controlPlaneFeature,
		},
		{
			name: "snapshot", aliases: []string{"snapshots"}, run: cmdSnapshot, subs: []string{"save", "list", "get", "rm"},
			help: `snapshot save <id> <name> [--no-wait]   org-owned golden image: it outlives the
                       sandbox and 'sandbox create --from-snapshot <name>' boots
                       it; snapshot list; snapshot get <name>; snapshot rm <name> --confirm`,
			example: `pilldrops --json snapshot list`,
			feature: controlPlaneFeature,
		},
		{
			name: "checkpoint", aliases: []string{"checkpoints"}, run: cmdCheckpoint, subs: []string{"save", "list", "rm", "restore"},
			help: `checkpoint save|list|rm <id> [name]     named save points on the sandbox's node;
                       rm and restore take --confirm: checkpoint restore <id>
                       <name> rolls it back in place, losing every write since`,
			example: `pilldrops --json checkpoint list $SB`,
		},
		{
			name: "backup", aliases: []string{"backups"}, run: cmdBackup, subs: []string{"create", "list", "rm", "restore", "schedule"},
			help: `backup create <id> [--no-wait]          capture to the object store (minutes);
                       backup list <id>; rm and restore take --confirm —
                       restore overwrites the disk; backup schedule <id> [--every 24h |
                       --cron '0 3 * * *' | --off] [--retain 7d]`,
			example: `pilldrops --json backup list $SB`,
		},
		{
			name: "fs", run: cmdFS, subs: []string{"ls", "stat", "mkdir", "mv", "rm"},
			help: `fs ls|stat|mkdir|rm <id> <path>         browse and edit the sandbox filesystem
                       (fs mv <id> <src> <dst>); cp moves the bytes`,
			example: `pilldrops --json fs ls $SB .`,
		},
		{
			name: "ps", run: cmdPs,
			help:    `ps <id>                list processes`,
			example: `pilldrops --json ps $SB`,
		},
		{
			name: "metrics", run: cmdMetrics,
			help:    `metrics <id>           live resource metrics`,
			example: `pilldrops --json metrics $SB`,
		},
		{
			name: "usage", run: cmdUsage,
			help:    `usage                  account usage + charges summary (--json for raw)`,
			example: `pilldrops --json usage`,
		},
		{
			name: "history", run: cmdHistory,
			help: `history [--limit N]    past sandboxes with per-pod charge + runtime
history <id>           one past sandbox: charge, runtime, logs`,
			example: `pilldrops --json history --limit 1`,
		},
		{
			name: "events", run: cmdEvents,
			help:    `events [<id>]          lifecycle events (created/paused/resumed/oom), newest first`,
			example: `pilldrops --json events`,
		},
		{
			name: "image", run: cmdImage, subs: []string{"build"},
			help:    `image build <dir> -t N build a custom image (Dockerfile at <dir> root) → --image N`,
			example: `pilldrops --json image build $DOCKERDIR -t catalog-example`,
		},
		group("team", teamVerbs(), `team login [--portal URL] [--no-wait] [--device-code C] | logout
                       team orgs | org create <name> | members [--org o:ID]
                       team invite <email> [--role R] | uninvite <email> --confirm
                       team role <user_id> <role> | remove <user_id> --confirm
                       manage people; a person approves the login in a browser`,
			// Every team verb needs a login a person approves in a browser.
			`pilldrops --json version`).needs("team"),
		group("volume", volumeVerbs(), `volume create [name] --size-mb N | list | get <id> | rm <id> --confirm
                       a disk that outlives its sandbox; attach it with
                       sandbox create --volume <id>:<path>`,
			`pilldrops --json volume list`).needs("volume"),
		// Every agent route 404s for a template that is not kind=agent, and the
		// sweep's sandbox is a plain one, so it checks the catalog those come from.
		group("agent", agentVerbs(), `agent status|models|turns|threads|tokens|approvals|policy|webhooks|isolation|connect <id>
                       read an agent workstation (connect mints a token each call)
agent budget <id> [--cap USD]   read or set the daily spend cap (0 clears it)
agent ask <id> <message> [--mode supervised|guided|autonomous] [--thread T]
                       [--priority] [--wait]   submit a turn; --wait polls until it
                       ends, prints its response, and exits 1 unless it succeeded
agent turn <id> <turn>; agent thread <id> <thread>
agent interrupt <id> <turn> [--grace N]; agent dequeue <id> <turn> --confirm
agent events <id> [--since N] [-f]   the journal; without -f it stops when the
                       journal goes quiet. --json gives one array and refuses -f
agent token create <id> --scope agent:read ... [--label L] [--ttl S]
agent token rm <id> <tokenID> --confirm; agent token rotate <id> <tokenID>
agent share <id> [--scope ...]   a one-time link (never the token itself)
agent approve <id> <approvalID> --allow|--deny
agent webhook add <id> <url> --event E ...; agent webhook rm <id> <hookID> --confirm;
                       agent webhook test <id> <hookID>. agent webhooks <id> lists
                       the events that can be delivered (turn.completed, turn.failed, …)`,
			`pilldrops --json templates`).needs("agent"),
		group("cron", cronVerbs(), `cron list | create <sandbox> <schedule> <command> [--name N] [--timezone Z]
                       [--agent-mode guided|autonomous] | rm <id> --confirm |
                       run <id> (exit 1 if the job failed) | toggle <id> --on|--off`,
			`pilldrops --json cron list`).needs("cron"),
		group("webhook", webhookVerbs(), `webhook list | add <url> --event E ... | rm <id> --confirm |
                       test <id> (exit 1 if the endpoint did not accept it) | toggle <id> --on|--off |
                       deliveries. Reports sandbox lifecycle events (sandbox.created,
                       sandbox.ended, …) for the whole organization; add prints the
                       signing secret once`,
			`pilldrops --json webhook list`).needs("webhook"),
		group("function", functionVerbs(), `function list | create <name> --start CMD [--template T] [--port N] [--vcpu N]
                       [--mem MB] [--max-replicas N] [--env K=V ...] [--file local[:remote] ...]
                       | get|code|logs|metrics <name> | rm <name> --confirm`,
			`pilldrops --json function list`).needs("function"),
		group("project", projectVerbs(), `project list | create <name> [--plan TEXT] | get <id> | rm <id> --confirm (archives)
                       | runs <id> | run <id> [--service service:sandbox ...]
                       (run records a run against the plan; it starts nothing)`,
			`pilldrops --json project list`).needs(controlPlaneFeature),
		group("thread", threadVerbs(), `thread list | create [--title T] | get <id> | rm <id> --confirm
                       stored transcripts, as the dashboard keeps them; a thread
                       runs nothing. agent ask gives work to an agent`,
			`pilldrops --json thread list`).needs("thread"),
		group("integrations", integrationVerbs(), `integrations           the MCP servers that can be switched on
                       (sandbox integrations <id> switches them)`,
			`pilldrops --json integrations`).needs("integrations"),
		group("chat", chatVerbs(), `chat <id> [message] [--autonomous] [--limit N]   with a message, send it and wait
                       for the reply; without, print the transcript`,
			`pilldrops --json chat $SB`).needs("chat"),
		group("chat-link", chatLinkVerbs(), `chat-link <id> [--read-only] [--autonomous]   a one-time code that pairs a messaging
                       app with this sandbox; chat-link rm <id> --confirm revokes every link`,
			`pilldrops --json chat-link $SB --read-only`).needs("chat"),
		group("channels", channelVerbs(), `channels <id>          which messaging channels can reach this sandbox`,
			`pilldrops --json channels $SB`).needs("chat"),
		group("audit", auditVerbs(), `audit [--limit N]      who did what in this org`,
			`pilldrops --json audit --limit 1`),
		{
			name: "api", run: cmdAPI,
			help: `api <METHOD> </v1/path> [--data JSON | --data -]   any /v1 route with the stored
                       credential, for what has no typed verb; same exit codes,
                       and no --confirm gate: this is the raw API`,
			example: `pilldrops --json api GET /v1/whoami`,
		},
		{
			name: "cluster", run: cmdCluster, subs: []string{"apply", "example"}, humanOnly: true,
			help: `cluster apply [-f cluster.toml] [--prune]   install, upgrade or update a self-hosted
                       fleet over ssh: control host, Postgres, HTTPS and nodes. A new
                       version upgrades each node in turn; --prune drains and removes
                       nodes no longer listed. cluster example prints a cluster.toml`,
			example: `pilldrops cluster example`,
		},
		{
			name: "doctor", run: cmdDoctor,
			help:    `doctor                 check version, credentials, connectivity, auth`,
			example: `pilldrops --json doctor`,
		},
		{
			name: "version", aliases: []string{"--version", "-v"}, run: cmdVersion,
			help:    `version                print CLI version`,
			example: `pilldrops --json version`,
		},
		{
			name: "upgrade", aliases: []string{"self-update"}, run: cmdUpgrade,
			help: `upgrade                self-update to the latest published binary`,
			// Replacing the running binary mid-catalog-run is not a check.
			example: `pilldrops --json version`,
		},
		{
			name: "ai", aliases: []string{"llms", "agent-guide"}, run: cmdAI,
			help: `ai                     the full agent catalog: workflows, JSON contract,
                       error→fix table (agents: run this first, use --json)`,
			example: `pilldrops --json ai`,
		},
		{
			name: "mcp", run: cmdMCP,
			help: `mcp                    serve sandbox tools over MCP on stdio (uses the stored
                       login) — e.g. claude mcp add pilldrops -- pilldrops mcp`,
			// Serving on stdio blocks forever; the catalog check is that the
			// tools it serves match the platform's (pkg/mcp TestToolManifest).
			example: `pilldrops --json version`,
		},
	}
}

// lookup resolves a verb or one of its aliases.
func lookup(name string) (command, bool) {
	for _, c := range commandTable() {
		if c.name == name {
			return c, true
		}
		for _, a := range c.aliases {
			if a == name {
				return c, true
			}
		}
	}
	return command{}, false
}

// commandPaths is the flat list an agent reads: "sandbox create", "fs ls",
// and the bare verbs. Generated, so a verb cannot be removed from the table
// and stay in the catalog.
func commandPaths(supports func(string) bool) []string {
	var out []string
	for _, c := range commandTable() {
		if c.humanOnly || (c.feature != "" && !supports(c.feature)) {
			continue
		}
		subs := c.subs
		for _, part := range c.optional {
			if part.sub != "" && supports(part.feature) {
				subs = append(subs[:len(subs):len(subs)], part.sub)
			}
		}
		if len(subs) == 0 {
			out = append(out, c.name)
			continue
		}
		for _, s := range subs {
			out = append(out, strings.TrimSpace(c.name+" "+s))
		}
	}
	return out
}

// commandHelp renders the table's help blocks, two spaces in.
func commandHelp(forAgents bool, supports func(string) bool) string {
	var b strings.Builder
	write := func(help string) {
		for _, line := range strings.Split(help, "\n") {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	for _, c := range commandTable() {
		if (forAgents && c.humanOnly) || (c.feature != "" && !supports(c.feature)) {
			continue
		}
		write(c.help)
		for _, part := range c.optional {
			if supports(part.feature) {
				write(part.help)
			}
		}
	}
	return b.String()
}

// usage prints the top-level help for this server.
func (c *cmdCtx) usage(w io.Writer) {
	fmt.Fprint(w, "usage: pilldrops [--json] [--quiet] [--api-url URL] <command> [args]\n\ncommands:\n")
	fmt.Fprint(w, commandHelp(false, c.supports))
	if missing := c.unsupportedFeatures(); len(missing) > 0 {
		fmt.Fprintf(w, "\nnot on this server: %s\n", strings.Join(missing, ", "))
	}
}
