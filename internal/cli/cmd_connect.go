package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

// The interactive shell. The server half already runs in every sandbox: envd
// serves a login-shell PTY over a WebSocket at /ws on port 7681, and the
// preview proxy is the public way in — it attaches envd's own root credential
// as an x-access-token header on the hop into the guest, so this client never
// sees or needs that token. All it needs is the preview link for port 7681,
// which the platform already hands out as terminal_url.
//
// Not portable to Windows: SIGWINCH has no equivalent there and the release
// matrix is darwin/linux only.

const connectUsage = "usage: pilldrops connect <id> [--print]"

// resizeMsg is the control frame envd's terminal already understands
// (cmd/envd/terminal.go). A TEXT frame that parses as this, with both
// dimensions positive, resizes the PTY; anything else is raw input.
type resizeMsg struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// terminalPort is envd's web terminal (rt.TerminalPort). Only a fallback:
// the port is read out of the link the platform published wherever possible,
// so a constant here cannot drift from the platform's.
const terminalPort = 7681

// hasPreviewCredential reports whether a preview link already opens on its own.
// A STANDALONE node embeds ?ptok= (internal/httpapi appURL); a FLEET node
// cannot — previewToken returns "" whenever the node verifies Ed25519, because
// the HMAC form is the only one it could mint and its own gate would refuse it.
// So on a fleet the link arrives bare and the control plane in front is what
// mints the credential a caller actually uses.
func hasPreviewCredential(u *url.URL) bool {
	q := u.Query()
	return q.Get("token") != "" || q.Get("ptok") != ""
}

// previewPortOf reads the port out of a preview link — "/preview/<id>/<port>/"
// or "<port>-<id>.<domain>" — so the token minted and the port dialled agree
// with what the platform actually published.
func previewPortOf(u *url.URL) int {
	if f := strings.Split(strings.Trim(u.Path, "/"), "/"); len(f) >= 3 && f[0] == "preview" {
		if n, err := strconv.Atoi(f[2]); err == nil && n > 0 {
			return n
		}
	}
	if head, _, ok := strings.Cut(u.Hostname(), "-"); ok {
		if n, err := strconv.Atoi(head); err == nil && n > 0 {
			return n
		}
	}
	return terminalPort
}

// mintPreviewToken asks the control plane for the browser-hop token for one
// port. Orchestrator-only route; a node does not serve it and does not need to.
func mintPreviewToken(cl *Client, id string, port int) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := cl.JSON("GET", "/v1/sandboxes/"+url.PathEscape(id)+"/preview-token?port="+strconv.Itoa(port), nil, &out)
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusServiceUnavailable:
			// A deployment fact, not a transient failure — retrying never helps,
			// so do not let the generic 5xx hint say "retry".
			return "", hintedError{apiErr.Msg,
				"an operator must configure the control plane's API key before any preview, including this one, can be opened"}
		case http.StatusNotFound:
			return "", hintedError{"cannot mint a preview token for " + id + ": " + apiErr.Msg,
				"check the id with `pilldrops sandbox ls` — a sandbox you do not own reads as missing, and so does a tier that does not serve this route"}
		}
	}
	if err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", errors.New("the control plane returned an empty preview token")
	}
	return out.Token, nil
}

// connectURL resolves the websocket to dial from the link the platform
// published, minting a credential when that link carries none.
//
// The rebuild is against the CLI's own API base, not the link's host, and that
// is the whole point: a fleet node's terminal_url names the NODE, which is
// neither reachable nor authenticated from out here — sanitizeNodeURLs rewrites
// only values containing "ptok=", so a bare one is relayed untouched. The token
// is minted by, and verified by, the tier this CLI is talking to, so the link
// has to name that tier too. The path form is served there whether or not a
// sandbox domain is configured.
func connectURL(cl *Client, id, terminalURL string) (string, error) {
	u, err := url.Parse(terminalURL)
	if err != nil {
		return "", fmt.Errorf("terminal_url is not a URL: %w", err)
	}
	if hasPreviewCredential(u) {
		return wsTerminalURL(terminalURL)
	}
	port := previewPortOf(u)
	tok, err := mintPreviewToken(cl, id, port)
	if err != nil {
		return "", err
	}
	return wsTerminalURL(fmt.Sprintf("%s/preview/%s/%d/?token=%s",
		strings.TrimSuffix(cl.Base, "/"), url.PathEscape(id), port, url.QueryEscape(tok)))
}

// wsTerminalURL turns a preview link into the WebSocket URL envd serves the
// shell on at /ws. The query is carried verbatim and deliberately not parsed:
// it is ?ptok= from a standalone node and ?token= from a control plane, and
// which one arrived is settled before this is called.
func wsTerminalURL(previewURL string) (string, error) {
	u, err := url.Parse(previewURL)
	if err != nil {
		return "", fmt.Errorf("terminal_url is not a URL: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("terminal_url has scheme %q, want http or https", u.Scheme)
	}
	// terminal_url ends at the preview root ("…/7681/" or "https://7681-id.dom/").
	u.Path = strings.TrimSuffix(u.Path, "/") + "/ws"
	return u.String(), nil
}

// cmdConnect opens an interactive shell in the sandbox.
func cmdConnect(c *cmdCtx, args []string) int {
	id, printOnly := "", false
	for _, a := range args {
		switch {
		case a == "--print":
			printOnly = true
		case strings.HasPrefix(a, "-"), id != "":
			fmt.Fprintln(c.stderr, connectUsage)
			return ExitUsage
		default:
			id = a
		}
	}
	if id == "" {
		if picked, ok := c.askSandboxID("connect"); ok {
			id = picked
		}
	}
	if id == "" {
		fmt.Fprintln(c.stderr, connectUsage)
		return ExitUsage
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	var sb struct {
		TerminalURL string `json:"terminal_url"`
	}
	if err := cl.JSON("GET", "/v1/sandboxes/"+id, nil, &sb); err != nil {
		return c.fail(err)
	}
	if sb.TerminalURL == "" {
		return c.fail(errors.New("sandbox reports no terminal_url — it is paused, or this deployment is older than the in-guest terminal"))
	}
	wsURL, err := connectURL(cl, id, sb.TerminalURL)
	if err != nil {
		return c.fail(err)
	}
	if printOnly {
		if c.jsonOut {
			printJSON(c.stdout, map[string]any{"id": id, "url": wsURL})
		} else {
			fmt.Fprintln(c.stdout, wsURL)
		}
		return ExitOK
	}
	return connectShell(c, wsURL)
}

// connectShell dials the terminal and pumps bytes until one end stops.
//
// Exit status: the protocol carries none. envd starts `bash -l` on a PTY and
// kills it when the socket closes; the shell's status is never put on the wire,
// so unlike `exec` this cannot propagate it. Instead: 0 when the remote shell
// ended and the socket closed cleanly, ExitAPI (1) plus a line on stderr when
// the connection dropped — which is how the two are told apart.
func connectShell(c *cmdCtx, wsURL string) int {
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			// The refusal bodies are the useful part: "This sandbox preview is
			// private", "App is not responding on port 7681", the boot-window
			// 503. Carry the first line rather than just "bad handshake".
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			_ = resp.Body.Close()
			return c.fail(hintedError{
				fmt.Sprintf("sandbox terminal refused the connection (HTTP %d): %s",
					resp.StatusCode, summarise(string(body))),
				dialHint(resp.StatusCode),
			})
		}
		return c.fail(fmt.Errorf("cannot reach the sandbox terminal: %w", err))
	}
	defer func() { _ = ws.Close() }()

	// One writer at a time: gorilla forbids concurrent writes, and the resize
	// goroutine writes alongside the stdin pump.
	var wmu sync.Mutex
	send := func(mt int, data []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return ws.WriteMessage(mt, data)
	}

	restore := rawMode(c)
	defer restore()

	// stdin -> ws, as BINARY frames. envd inspects TEXT frames for the resize
	// control message, so a shell line that happened to be that JSON would be
	// swallowed instead of typed; binary frames are never inspected.
	// Read the package var once: this goroutine outlives connectShell (nothing
	// can interrupt a blocking read on a terminal), so touching `stdin` from
	// inside it races with anything that reassigns it.
	in := stdin
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := in.Read(buf)
			if n > 0 {
				if send(websocket.BinaryMessage, buf[:n]) != nil {
					return
				}
			}
			if rerr != nil {
				// Local input ended (a pipe ran out, or stdin was closed).
				// EOT is what a terminal sends for end-of-input, so the remote
				// shell's line discipline sees EOF and exits instead of both
				// sides waiting on each other forever.
				_ = send(websocket.BinaryMessage, []byte{0x04})
				return
			}
		}
	}()

	if fd, ok := rawFD(); ok {
		sendResize := func() {
			cols, rows, err := term.GetSize(fd)
			if err != nil || cols <= 0 || rows <= 0 {
				return
			}
			wmu.Lock()
			defer wmu.Unlock()
			_ = ws.WriteJSON(resizeMsg{Type: "resize", Cols: cols, Rows: rows})
		}
		sendResize() // envd starts every PTY at 80x24 until told otherwise
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for range winch {
				sendResize()
			}
		}()
	}

	for {
		_, data, rerr := ws.ReadMessage()
		if rerr != nil {
			if websocket.IsCloseError(rerr, websocket.CloseNormalClosure,
				websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
				return ExitOK // the remote shell exited
			}
			// Cook the terminal before saying anything, so the message is not
			// printed into a raw screen without carriage returns.
			restore()
			fmt.Fprintf(c.stderr, "pilldrops: connection to the sandbox terminal was lost: %v\n", rerr)
			return ExitAPI
		}
		if _, werr := c.stdout.Write(data); werr != nil {
			return ExitAPI
		}
	}
}

// rawFD is the descriptor to drive as a terminal, when stdin is one. A pipe is
// not an error: `echo 'uname -a' | pilldrops connect sb` is a legitimate use,
// it just gets no raw mode, no resize and no signal handling.
func rawFD() (int, bool) {
	f, ok := stdin.(*os.File)
	if !ok {
		return 0, false
	}
	fd := int(f.Fd())
	return fd, term.IsTerminal(fd)
}

// rawMode puts the terminal in raw mode and returns the idempotent restore.
//
// A client that leaves a terminal raw is worse than no client, so restore runs
// on every path out: normal return and dropped connection (deferred in
// connectShell), a panic anywhere below (the same defer, during unwinding), and
// SIGTERM/SIGHUP/SIGQUIT (the goroutine here, which restores before exiting).
// SIGINT is listened for only for completeness — raw mode clears ISIG, so ^C
// reaches the remote shell as a byte and never becomes a local signal. Nothing
// survives SIGKILL; `reset` is the only cure for that one.
func rawMode(c *cmdCtx) func() {
	fd, ok := rawFD()
	if !ok {
		return func() {}
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		// Not fatal: the session still works, it just echoes locally.
		fmt.Fprintf(c.stderr, "pilldrops: could not put the terminal in raw mode: %v\n", err)
		return func() {}
	}
	var once sync.Once
	restore := func() { once.Do(func() { _ = term.Restore(fd, state) }) }

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, os.Interrupt)
	go func() {
		if _, open := <-sigs; !open {
			return
		}
		restore()
		os.Exit(ExitAPI) // only reachable with a real TTY, never under test
	}()
	return func() {
		signal.Stop(sigs)
		close(sigs)
		restore()
	}
}

// dialHint answers a refusal at the preview gate. Without it these fall through
// to hint()'s transport branch — "network problem, run doctor" — which for a
// 403 is both wrong and a waste of the reader's next five minutes.
func dialHint(status int) string {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "the preview gate rejected the link — check the sandbox is yours with `pilldrops sandbox ls`"
	case http.StatusNotFound:
		return "the sandbox, or its terminal port, is gone — `pilldrops sandbox ls`"
	case http.StatusServiceUnavailable:
		return "the sandbox is still waking — retry in a moment"
	}
	return ""
}

// summarise reduces a refusal body to one line worth printing.
func summarise(body string) string {
	s := strings.TrimSpace(body)
	// The one HTML refusal on this path is the preview gate's "private sandbox"
	// page; printing its doctype at someone helps nobody.
	if strings.HasPrefix(s, "<") {
		return "the preview gate refused it — the sandbox is private to another owner, or gating is not configured on this deployment"
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "(no message)"
	}
	return s
}
