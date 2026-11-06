package cli

import (
	"errors"
	"fmt"
	"io"
)

// Exit codes — the CLI's contract with scripts and AI agents. exec is the one
// exception: it propagates the remote command's exit code.
const (
	ExitOK    = 0
	ExitAPI   = 1 // API/server/network error
	ExitUsage = 2 // bad flags or arguments
	ExitAuth  = 3 // not authenticated — run `pilldrops login`
)

// cmdCtx carries per-invocation state to command implementations.
type cmdCtx struct {
	jsonOut bool
	quiet   bool
	apiURL  string
	stdout  io.Writer
	stderr  io.Writer
}

// DefaultAPIURL is a PillDrops server on this machine, where the engine's quick
// start runs one. Anything else is --api-url, PILLDROPS_API_URL, or
// `pilldrops login --api-url`. Having a default removes a whole class of
// first-run failures (`login` without --api-url used to error).
const DefaultAPIURL = "http://localhost:8080"

// client builds the API client, or returns an ExitAuth error when no
// credential is available.
func (c *cmdCtx) client() (*Client, error) {
	url, key := resolveCreds(c.apiURL)
	if key == "" {
		return nil, errNotLoggedIn
	}
	if url == "" {
		url = DefaultAPIURL
	}
	return &Client{Base: url, Key: key}, nil
}

var errNotLoggedIn = errors.New("not logged in — run `pilldrops login`")

// hint maps an error to the exact next command that unblocks it. Agents (and
// humans) act on these verbatim, so keep them copy-pasteable.
func hint(err error) string {
	// An error that knows its own remedy beats one guessed from a status code.
	// The status alone cannot tell a plan refusal from a preview-gate refusal,
	// and sending someone to `doctor` for a 403 wastes their time.
	var own interface{ Hint() string }
	if errors.As(err, &own) {
		return own.Hint()
	}
	var apiErr *APIError
	if errors.Is(err, errNotLoggedIn) {
		return "run `pilldrops login --key <key>` or set PILLDROPS_API_KEY (keys: dashboard → API keys)"
	}
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Status == 401:
			return "credential rejected — re-run `pilldrops login`, or check PILLDROPS_API_KEY"
		case apiErr.Status == 404:
			return "check the id with `pilldrops sandbox ls`; destroyed sandboxes are in `pilldrops history`"
		case apiErr.Status == 402 || apiErr.Status == 403:
			return "plan/limit issue — `pilldrops usage` shows consumption vs your plan caps"
		case apiErr.Status == 429:
			return "rate limited — wait a few seconds and retry"
		case apiErr.Status == 501:
			// Not a transient failure: the node or the runtime does not have
			// the thing at all, and retrying never changes that.
			return "this deployment or runtime does not support that — the server's message says what is missing"
		case apiErr.Status >= 500:
			return "server-side error — retry; if it persists run `pilldrops doctor`"
		}
		return ""
	}
	// No structured API error ⇒ transport-level failure.
	return "network problem — `pilldrops doctor` checks connectivity and auth end to end"
}

// fail prints err per the output contract and returns its exit code.
//
// "code" is the PLATFORM's code, verbatim; this process's exit status is
// "exit_code". They used to be the same key, which meant every refusal the
// platform explained — slug_taken, egress_not_enforced — was parsed and thrown
// away, and replaced with a 1.
func (c *cmdCtx) fail(err error) int {
	exit := ExitAPI
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == 401 {
		exit = ExitAuth
	}
	if errors.Is(err, errNotLoggedIn) {
		exit = ExitAuth
	}
	h := hint(err)
	if c.jsonOut {
		out := map[string]any{"error": err.Error(), "exit_code": exit}
		if apiErr != nil {
			out["status"] = apiErr.Status
			// Absent when the platform sent none, and absent entirely on a
			// transport failure — which is how the two are told apart.
			if apiErr.Code != "" {
				out["code"] = apiErr.Code
			}
		}
		if h != "" {
			out["hint"] = h
		}
		printJSON(c.stderr, out)
	} else {
		if apiErr != nil && apiErr.Code != "" {
			fmt.Fprintf(c.stderr, "pilldrops: %s [%s]\n", err.Error(), apiErr.Code)
		} else {
			fmt.Fprintln(c.stderr, "pilldrops:", err.Error())
		}
		if h != "" {
			fmt.Fprintln(c.stderr, "  hint:", h)
		}
	}
	return exit
}

// takeGlobals pulls --json/--quiet/--api-url out of the argument list wherever
// they appear, and returns what is left for the command.
//
// They used to be read only before the subcommand, so `sandbox ls --json` got a
// table (no flag set means no parse error, so the request for JSON was simply
// dropped) and `backup create <id> --json` got "flag provided but not defined".
// Both are the position a flag falls into naturally when you are already typing
// the command.
//
// Scanning stops at the first "--": everything after it is a command for the
// other side — exec relies on that — and a global flag there is the remote
// command's flag, not ours. Nothing else is touched, and no subcommand defines
// a flag by these names (login's own --api-url ends up in the same place
// through resolveCreds), so nothing is swallowed from under one.
func (c *cmdCtx) takeGlobals(args []string) []string {
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--":
			return append(rest, args[i:]...)
		case args[i] == "--json":
			c.jsonOut = true
		case args[i] == "--quiet":
			c.quiet = true
		case args[i] == "--api-url" && i+1 < len(args):
			c.apiURL = args[i+1]
			i++
		default:
			rest = append(rest, args[i])
		}
	}
	return rest
}

// Run is the CLI entrypoint; it returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	ctx := &cmdCtx{stdout: stdout, stderr: stderr}

	rest := ctx.takeGlobals(args)
	if len(rest) == 0 {
		if ctx.interactive() {
			return cmdShell(ctx, nil)
		}
		usage(stderr)
		return ExitUsage
	}
	cmd, cmdArgs := rest[0], rest[1:]
	switch cmd {
	case "help", "--help", "-h":
		usage(stdout)
		return ExitOK
	}
	if c, ok := lookup(cmd); ok {
		return c.run(ctx, cmdArgs)
	}
	fmt.Fprintf(stderr, "pilldrops: unknown command %q\n\n", cmd)
	usage(stderr)
	return ExitUsage
}

// hintedError carries its own remediation, for refusals whose HTTP status is
// ambiguous on its own. Read by hint() before any status-based guess.
type hintedError struct{ msg, hint string }

func (e hintedError) Error() string { return e.msg }
func (e hintedError) Hint() string  { return e.hint }

// pathError keeps a server error exactly as it came — status, platform code and
// message — and replaces only the remedy. hintedError cannot: it drops the
// *APIError, and with it the exit status and the code the caller parses.
type pathError struct {
	err  error
	hint string
}

func (e pathError) Error() string { return e.err.Error() }
func (e pathError) Unwrap() error { return e.err }
func (e pathError) Hint() string  { return e.hint }
