package cli

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/term"
)

// openBrowser launches the system browser; swapped out in tests.
var openBrowser = func(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

// identity is what a deployment says this credential is. The two shapes are
// merged deliberately: the platform answers GET /v1/whoami with
// {org, principal, sandbox_id?} and the hosted service answers the older
// /v1/auth/whoami with {owner, email}. Every field is optional.
type identity struct {
	Org       string `json:"org"`
	Principal string `json:"principal"` // key | operator | sandbox
	SandboxID string `json:"sandbox_id"`
	Owner     string `json:"owner"`
	Email     string `json:"email"`
}

// label is the one-line rendering: what this credential is, then who or what
// it acts for, from whichever fields the deployment answered with.
func (id identity) label() string {
	who := id.Email
	if who == "" {
		who = id.Principal
		if id.Principal == "sandbox" && id.SandboxID != "" {
			who = "sandbox " + id.SandboxID
		}
	}
	scope := id.Owner
	if scope == "" {
		scope = id.Org
	}
	switch {
	case who != "" && scope != "":
		return who + " (" + scope + ")"
	case who != "":
		return who
	case scope != "":
		return scope
	}
	return "unknown"
}

// whoami asks the deployment what this credential is.
//
// /v1/whoami is the platform's route (internal/orchestrator/server.go). The CLI
// used to call /v1/auth/whoami only, which exists nowhere in the platform, so
// `login --key` — the scripted path — 404'd and silently stored nothing. The
// older path stays as a fallback rather than being aliased on the platform: one
// binary talks to both deployments, so it has to tolerate both anyway, and the
// open-source platform should not grow a route that exists to paper over a
// client's hardcoded path.
func whoami(cl *Client) (identity, error) {
	var id identity
	err := cl.JSON("GET", "/v1/whoami", nil, &id)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == 404 {
		// Only a 404 falls back: a 401 means the route is there and the
		// credential is not, and retrying elsewhere would report the wrong
		// failure — which is how a bad key came to exit 1 instead of 3.
		var legacy identity
		err = cl.JSON("GET", "/v1/auth/whoami", nil, &legacy)
		return legacy, err
	}
	return id, err
}

func cmdLogin(c *cmdCtx, args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	key := fs.String("key", "", "API key to store; omitted on a terminal, it is asked for with echo off")
	apiURL := fs.String("api-url", "", "portal URL (defaults to the global --api-url / PILLDROPS_API_URL, then "+DefaultAPIURL+")")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	base := *apiURL
	if base == "" {
		base, _ = resolveCreds(c.apiURL)
	}
	if base == "" {
		base = DefaultAPIURL
	}

	gotKey := *key
	asked := gotKey == ""
	if asked {
		var ok bool
		if base, gotKey, ok = c.askCredential(base); !ok {
			return ExitUsage
		}
	}

	// Verify before storing: a bad key must not be persisted.
	cl := &Client{Base: base, Key: gotKey}
	id, err := whoami(cl)
	if err != nil {
		if asked {
			fmt.Fprintf(c.stderr, "%s did not accept that key; nothing was stored\n", base)
		}
		return c.fail(err)
	}
	if err := SaveConfig(Config{APIURL: base, Key: gotKey, Email: id.Email}); err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, identityJSON(id, map[string]any{"api_url": base}))
	} else if !c.quiet {
		fmt.Fprintf(c.stdout, "logged in as %s\n", id.label())
	}
	return ExitOK
}

// identityJSON renders only the fields the deployment actually answered with,
// so a caller can tell "no email" from "email: empty string".
func identityJSON(id identity, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range extra {
		out[k] = v
	}
	for k, v := range map[string]string{
		"org": id.Org, "principal": id.Principal, "sandbox_id": id.SandboxID,
		"owner": id.Owner, "email": id.Email,
	} {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

const loginUsage = "usage: pilldrops login --key <key> [--api-url URL]"

// askCredential is the terminal half of login. Everything automated gets the
// usage line, because nothing can answer a question there.
func (c *cmdCtx) askCredential(base string) (server, key string, ok bool) {
	if !c.promptable() {
		fmt.Fprintln(c.stderr, loginUsage)
		return "", "", false
	}
	server, ok = c.ask("Server URL", base)
	if !ok {
		fmt.Fprintln(c.stderr, loginUsage)
		return "", "", false
	}
	server = strings.TrimRight(server, "/")
	if u, err := url.Parse(server); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		fmt.Fprintf(c.stderr, "pilldrops login: %q is not an http(s) URL\n", server)
		return "", "", false
	}
	fmt.Fprintf(c.stderr, "Mint a key at %s/dashboard (API keys), then paste it here.\n", server)
	key, ok = c.askSecret("API key")
	if !ok {
		fmt.Fprintln(c.stderr, loginUsage)
	}
	return server, key, ok
}

// askSecret reads one line with echo off. ReadPassword keeps ISIG, so unlike
// ask, a ^C here would kill the process with the terminal still not echoing.
func (c *cmdCtx) askSecret(question string) (string, bool) {
	fd, _ := rawFD()
	state, err := term.GetState(fd)
	if err != nil {
		fmt.Fprintf(c.stderr, "pilldrops: cannot read the terminal: %v\n", err)
		return "", false
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	read := make(chan struct{})
	defer close(read)
	go func() {
		select {
		case <-sigs:
			_ = term.Restore(fd, state)
			fmt.Fprintln(c.stderr)
			os.Exit(ExitUsage) // only reachable with a real TTY, never under test
		case <-read:
		}
	}()

	fmt.Fprintf(c.stderr, "%s: ", question)
	line, err := term.ReadPassword(fd)
	fmt.Fprintln(c.stderr) // the newline that ended it was not echoed either
	secret := strings.TrimSpace(string(line))
	return secret, err == nil && secret != ""
}

func cmdLogout(c *cmdCtx, _ []string) int {
	// Drop the credential but keep the portal URL, so a follow-up
	// `pilldrops login --key K` doesn't have to re-supply --api-url. If there was
	// no stored URL, remove the file entirely (clean "never configured" state).
	cfg, _ := LoadConfig()
	if cfg.APIURL != "" {
		if err := SaveConfig(Config{APIURL: cfg.APIURL}); err != nil {
			return c.fail(err)
		}
	} else if err := DeleteConfig(); err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, map[string]string{"status": "logged_out"})
	} else if !c.quiet {
		fmt.Fprintln(c.stdout, "logged out")
	}
	return ExitOK
}

func cmdWhoami(c *cmdCtx, _ []string) int {
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	id, err := whoami(cl)
	if err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, identityJSON(id, nil))
	} else {
		fmt.Fprintln(c.stdout, id.label())
	}
	return ExitOK
}
