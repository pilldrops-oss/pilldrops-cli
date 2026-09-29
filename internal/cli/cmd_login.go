package cli

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"
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
	key := fs.String("key", "", "store an existing API key instead of browser login")
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
	if gotKey == "" {
		var err error
		if gotKey, err = browserLogin(c, base); err != nil {
			return c.fail(err)
		}
	}

	// Verify before storing: a bad key must not be persisted.
	cl := &Client{Base: base, Key: gotKey}
	id, err := whoami(cl)
	if err != nil {
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

// browserLogin runs the localhost-callback dance against <base>/cli/auth.
func browserLogin(c *cmdCtx, base string) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	stateB := make([]byte, 16)
	if _, err := rand.Read(stateB); err != nil {
		return "", fmt.Errorf("generating login state: %w", err)
	}
	state := hex.EncodeToString(stateB)

	type result struct {
		key string
		err error
	}
	done := make(chan result, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			http.Error(w, "state mismatch — close this tab and retry", http.StatusForbidden)
			done <- result{err: fmt.Errorf("login callback state mismatch")}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, loginDoneHTML)
		done <- result{key: q.Get("key")}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	authURL := fmt.Sprintf("%s/cli/auth?port=%d&state=%s", base, port, state)
	if !c.quiet {
		fmt.Fprintf(c.stderr, "Opening browser for sign-in… if it does not open, visit:\n  %s\n", authURL)
	}
	if err := openBrowser(authURL); err != nil {
		fmt.Fprintf(c.stderr, "could not open browser (%v) — open the URL above manually\n", err)
	}

	timeout := 3 * time.Minute
	if v := os.Getenv("SANDFORGE_LOGIN_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			timeout = d
		}
	}
	select {
	case res := <-done:
		if res.err != nil {
			return "", res.err
		}
		if res.key == "" {
			return "", fmt.Errorf("login callback carried no key")
		}
		return res.key, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("login timed out after %s", timeout)
	}
}

// loginDoneHTML is the themed success page shown after `pilldrops login`. It
// matches the console's light theme, immediately strips the key + state from
// the visible URL/history (they only ever traveled over localhost, but shouldn't
// linger in browser history), and auto-closes the tab — falling back to a clear
// "you can close this tab" message when the browser blocks window.close() for a
// user-opened tab.
const loginDoneHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>SandForge — signed in</title>
<style>
  :root{--bg:#fafafa;--card:#fff;--bd:#ececec;--fg:#0a0a0a;--muted:#6b7280;--ok:#16a34a}
  *{box-sizing:border-box}
  html,body{height:100%;margin:0}
  body{background:var(--bg);color:var(--fg);display:flex;align-items:center;justify-content:center;
    font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Inter,Roboto,Helvetica,Arial,sans-serif;
    -webkit-font-smoothing:antialiased}
  .card{background:var(--card);border:1px solid var(--bd);border-radius:16px;padding:40px 44px;
    max-width:420px;width:calc(100% - 32px);text-align:center;
    box-shadow:0 1px 2px rgba(0,0,0,.04),0 8px 30px rgba(0,0,0,.06)}
  .mark{width:44px;height:44px;border:2px solid var(--fg);border-radius:11px;margin:0 auto 22px;
    display:flex;align-items:center;justify-content:center}
  .mark span{width:16px;height:16px;border:2px solid var(--fg);border-radius:4px}
  .check{width:26px;height:26px;border-radius:50%;background:var(--ok);display:inline-flex;
    align-items:center;justify-content:center;vertical-align:middle;margin-right:9px}
  .check svg{width:15px;height:15px}
  h1{font-size:19px;font-weight:600;margin:0 0 8px;letter-spacing:-.01em;display:flex;
    align-items:center;justify-content:center}
  p{margin:0;color:var(--muted);font-size:14px;line-height:1.55}
  .hint{margin-top:20px;font:12px ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;color:#9ca3af}
  .dot{display:inline-block;width:7px;height:7px;border-radius:50%;background:var(--ok);
    margin-right:7px;vertical-align:middle}
</style></head>
<body>
  <div class="card">
    <div class="mark"><span></span></div>
    <h1><span class="check"><svg viewBox="0 0 24 24" fill="none" stroke="#fff" stroke-width="3.5"
      stroke-linecap="round" stroke-linejoin="round"><path d="M20 6 9 17l-5-5"/></svg></span>Signed in</h1>
    <p>You're authenticated with SandForge.<br>Return to your terminal to continue.</p>
    <div class="hint"><span class="dot"></span><span id="hint">this tab will close automatically…</span></div>
  </div>
  <script>
    // Strip key + state from the URL bar and history right away.
    try { history.replaceState({}, document.title, location.pathname); } catch (e) {}
    // Try to auto-close (works when the tab was opened by a script); otherwise
    // leave a clear instruction.
    setTimeout(function(){
      window.close();
      setTimeout(function(){
        var h = document.getElementById('hint');
        if (h) h.textContent = 'you can close this tab';
      }, 400);
    }, 1500);
  </script>
</body></html>`

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
