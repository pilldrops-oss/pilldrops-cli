package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/pilldrops-oss/pilldrops-cli/pkg/mcp"
)

// cmdAI prints the agent catalog: everything an AI (or a script author) needs
// to drive the platform from this CLI without reading any other docs.
//
// The command list and the MCP tool count are rendered from the code that
// implements them (commands.go, pkg/mcp), not typed here: a verb deleted from
// the table leaves the catalog with it, and the count cannot go stale. Only the
// prose — the contract, the workflows, the error table — is written by hand.
func cmdAI(c *cmdCtx, _ []string) int {
	if c.jsonOut {
		printJSON(c.stdout, map[string]any{
			"version":  Version,
			"catalog":  aiCatalog(),
			"commands": commandPaths(),
			"mcp_tools": map[string]any{
				"count":  mcp.ToolCount(),
				"stdio":  "pilldrops mcp",
				"remote": "<api-url>/mcp",
			},
			"exit_codes": map[string]int{
				"ok": ExitOK, "api_error": ExitAPI, "usage_error": ExitUsage, "auth_error": ExitAuth,
			},
		})
		return ExitOK
	}
	_, _ = io.WriteString(c.stdout, aiCatalog())
	return ExitOK
}

func aiCatalog() string {
	var b strings.Builder
	b.WriteString(strings.ReplaceAll(aiHeader, "{{version}}", Version))
	b.WriteString("\n## Commands\n\n")
	b.WriteString(commandHelp())
	b.WriteString(strings.ReplaceAll(aiBody, "{{tools}}", fmt.Sprint(mcp.ToolCount())))
	return b.String()
}

const aiHeader = `# pilldrops — AI agent catalog (v{{version}})

> PillDrops gives AI agents disposable cloud computers: isolated sandboxes that run
> untrusted/generated code, host app servers with public preview URLs, and bill per
> second. This catalog is the complete contract for driving it from the CLI.

## Contract

- Add ` + "`--json`" + ` (global flag, before the command) to get machine-readable output.
  Results go to stdout; errors go to stderr as
  {"error", "code", "exit_code", "hint"}.
- ` + "`code`" + ` is the PLATFORM's own error code, verbatim: slug_taken,
  egress_not_enforced, confirm_required, backups_need_object_store, … Branch on it.
  It is absent when the request never reached the platform, which is how a
  transport failure is told apart from a refusal.
- ` + "`exit_code`" + ` is this process's exit status: 0 ok · 1 API/network error ·
  2 bad usage · 3 not authenticated. Exception: ` + "`exec`" + ` exits with the
  REMOTE command's exit code.
- Credentials resolve in order: ` + "`--api-url`" + ` flag >
  PILLDROPS_API_KEY/PILLDROPS_API_URL env > the SANDFORGE_ equivalents >
  /run/pilldrops/{token,env} (present only inside a sandbox, which is then its
  own principal) > ~/.sandforge/config.json (written by login).
  API URL defaults to http://localhost:8080 when unset.
- A missing required argument is always exit 2 with a usage line, never a
  question: the CLI asks for one only when stdin is a terminal AND ` + "`--json`" + `
  was not given, so nothing you run can block waiting for an answer.
- Non-interactive auth: ` + "`pilldrops login --key <key>`" + `, or set
  PILLDROPS_API_KEY. Mint a key with ` + "`pilldrops keys create <name>`" + ` —
  the secret is printed once and never again.
- Ask before you assume: ` + "`pilldrops capabilities`" + ` says what this
  deployment supports and ` + "`pilldrops limits`" + ` what this plan allows, so a
  501 or a 402 can be predicted rather than discovered.
- ` + "`pilldrops api <METHOD> /v1/...`" + ` reaches any route without a typed verb.
  /v1/members and /v1/orgs refuse API keys on purpose: they need a dashboard
  sign-in, so nothing here can reach them.
- Self-checks: ` + "`pilldrops doctor --json`" + ` diagnoses auth/connectivity;
  ` + "`pilldrops upgrade`" + ` replaces this binary with the latest release.
`

const aiBody = `
## Workflows

### 1. Run generated code, capture output, clean up
    pilldrops --json sandbox create --template python-3.11    # → {"id":"sb_x",...}
    printf '%s' "$GENERATED_CODE" > /tmp/task.py
    pilldrops cp /tmp/task.py sb_x:/work/task.py
    pilldrops exec sb_x -- python3 /work/task.py              # stdout/stderr stream back
    echo $?                                                   # remote exit code
    pilldrops sandbox rm sb_x --confirm            # every destructive verb takes --confirm

### 2. Deploy a generated app with a public preview URL
    pilldrops cp ./app sb_x:/app                              # or bake files first, then:
    pilldrops --json sandbox create --template python-3.11 \
        --start "python /app/main.py" --port 8000             # → {"url":"https://..."}
    # CRITICAL: the app must listen on the injected $PORT env var. A hardcoded
    # port is the #1 cause of preview 502s.
    curl -sS <url>                                            # verify it serves

### 3. Debug a misbehaving sandbox
    pilldrops logs sb_x            # what did it print? (add -f to follow live)
    pilldrops ps sb_x              # is the process even running?
    pilldrops metrics sb_x         # cpu/mem — OOM or a busy loop?
    pilldrops exec sb_x -- bash -c 'cat /work/*.log; ls -la /work'
    pilldrops connect sb_x         # a real PTY shell when one command is not enough
    # sandbox already gone? its record, cost, and logs survive:
    pilldrops history sb_x

### 4. Long-lived server that survives idle-pause
    pilldrops start sb_x --port 3000 -- npm run dev
    # unlike a bare exec, start registers the command so resume relaunches it;
    # or create with --idle -1 to disable pausing entirely.

### 5. CI / verification gate
    pilldrops exec sb_x -- bash -c 'cd /work && pytest -q'    # CLI exit = pytest exit
    pilldrops --json history sb_x                             # runtime + exact cost

### 6. Custom environment
    pilldrops image build ./mydir -t myimg     # Dockerfile at ./mydir/Dockerfile
    pilldrops sandbox create --image myimg

### 7. Working from inside a sandbox
    # every base template ships this CLI and the sandbox holds its own identity
    # at /run/pilldrops/token, so no key has to be pasted in:
    pilldrops --json whoami                    # → {"principal":"sandbox","sandbox_id":"sb_x"}
    pilldrops --json sandbox ls

## Error → remediation

    exit 3 / "not logged in"        pilldrops login --key <key>  (or set PILLDROPS_API_KEY)
    "not found (HTTP 404)"          sandbox is gone or the id is wrong: pilldrops sandbox ls;
                                    destroyed sandboxes live in pilldrops history
    code slug_taken                 another live sandbox holds that slug — pick another
    code egress_not_enforced        the rule needs a capability this node lacks; check
                                    pilldrops capabilities (egress_domains)
    code confirm_required           the call destroys something — re-run with --confirm
    code backups_need_object_store  this deployment has no object store; checkpoints still work
    no "code" at all                the request never reached the platform: network/DNS/TLS.
                                    pilldrops doctor --json diagnoses it
    preview URL returns 502         app isn't bound to $PORT — restart it with the injected
                                    port ($PORT env inside the sandbox), or use --port to match
    exec output truncated           large outputs are capped; write to a file and cp it out
    sandbox paused unexpectedly     idle auto-pause (default ~5m) — pilldrops sandbox resume <id>,
                                    then use start (not exec) for servers, or create with --idle -1
    "limit/quota" errors            plan caps hit — pilldrops usage and pilldrops limits show
                                    consumption against the ceilings
    network/5xx errors              pilldrops doctor --json diagnoses; GETs retry once automatically

## MCP (use sandboxes as native agent tools)

    # stdio, using the stored login (run pilldrops login first; logged out, tools fail):
    claude mcp add pilldrops -- pilldrops mcp
    # remote, served by the control plane at /mcp, with an API key:
    claude mcp add --transport http pilldrops <server>/mcp --header "Authorization: Bearer <api-key>"
    # {{tools}} tools — tools/list is the authoritative catalog and is public;
    # only tools/call requires auth. The same set is served by both transports,
    # and a test in each repo fails if they ever diverge.

## Notes for agents

- Quote through the shell boundary: pilldrops exec sb_x -- bash -c 'complex | pipeline'.
- exec runs one command and buffers its output, so nothing that reads a terminal
  works in it: editors, pagers, REPLs, interactive installers. connect (alias ssh)
  attaches a real PTY for those. It carries no remote exit code — the protocol has
  none — and exits 0 when the remote shell ended, 1 when the connection dropped.
  Piped stdin works (echo 'uname -a' | pilldrops connect sb_x); for scripting,
  prefer exec, whose exit status IS the command's.
- Everything here is also available as SDKs: pip install pilldrops · npm install @pilldrops/sdk,
  and raw HTTP at /v1 (see /docs on your server, machine copy at /llms.txt).
- Usage is metered per second; an idle-paused sandbox stops accruing it. Check pilldrops usage.
`
