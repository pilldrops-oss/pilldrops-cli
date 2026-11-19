package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const connectorUsage = `usage: pilldrops connector create <name> | run <name> NAME=HOST:PORT... (key from PILLDROPS_CONNECTOR_KEY) | list | logs <name> | key create|list|rm <name> [key-id] | rm <name>`

var connectorName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

type connectorTarget struct {
	Name string `json:"name"`
	Port int    `json:"port"`
	Host string `json:"host,omitempty"`
	addr string // dialled locally, never sent
}

func parseConnectorTargets(args []string) ([]connectorTarget, error) {
	if len(args) == 0 {
		return nil, errors.New("name at least one target, like db=10.0.0.5:5432")
	}
	seen := map[string]bool{}
	var out []connectorTarget
	for _, a := range args {
		name, hostport, ok := strings.Cut(a, "=")
		if !ok || !connectorName.MatchString(name) || seen[name] {
			return nil, fmt.Errorf("%q: want NAME=HOST:PORT with a lowercase, unique NAME", a)
		}
		host, portS, err := net.SplitHostPort(hostport)
		port, perr := strconv.Atoi(portS)
		if err != nil || perr != nil || host == "" || port < 1 || port > 65535 {
			return nil, fmt.Errorf("%q: want NAME=HOST:PORT", a)
		}
		t := connectorTarget{Name: name, Port: port, addr: net.JoinHostPort(host, portS)}
		// The server refuses a hello naming localhost or a single-label host, so
		// those stay local and the sandbox reaches them as <name>.private only.
		h := strings.TrimSuffix(strings.ToLower(host), ".")
		if net.ParseIP(h) == nil && strings.Contains(h, ".") && !strings.HasSuffix(h, ".localhost") {
			t.Host = h
		}
		seen[name] = true
		out = append(out, t)
	}
	return out, nil
}

func cmdConnector(c *cmdCtx, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, connectorUsage)
		return ExitUsage
	}
	if args[0] == "run" {
		return cmdConnectorRun(c, args[1:])
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	if code, ok := requirePrivateAccess(c, cl); !ok {
		return code
	}
	switch args[0] {
	case "create":
		if len(args) != 2 {
			break
		}
		var out map[string]any
		if err := cl.JSON("POST", "/v1/connectors", map[string]any{"name": args[1]}, &out); err != nil {
			return c.fail(adminOnly(err))
		}
		if c.jsonOut {
			printJSON(c.stdout, out)
		} else {
			fmt.Fprintf(c.stdout, "%s\t%s\n", str(out["id"]), str(out["name"]))
		}
		return ExitOK
	case "list", "ls":
		if len(args) != 1 {
			break
		}
		var out struct {
			Connectors []struct {
				ID, Name  string
				Online    bool
				Instances int
				Targets   []connectorTarget
			} `json:"connectors"`
		}
		if c.jsonOut {
			var raw map[string]any
			if err := cl.JSON("GET", "/v1/connectors", nil, &raw); err != nil {
				return c.fail(err)
			}
			printJSON(c.stdout, raw)
			return ExitOK
		}
		if err := cl.JSON("GET", "/v1/connectors", nil, &out); err != nil {
			return c.fail(err)
		}
		rows := [][]string{{"NAME", "ONLINE", "INSTANCES", "TARGETS"}}
		for _, cn := range out.Connectors {
			var ts []string
			for _, t := range cn.Targets {
				ts = append(ts, fmt.Sprintf("%s:%d", t.Name, t.Port))
			}
			rows = append(rows, []string{cn.Name, strconv.FormatBool(cn.Online), strconv.Itoa(cn.Instances), strings.Join(ts, ",")})
		}
		table(c.stdout, rows)
		return ExitOK
	case "logs":
		if len(args) != 2 {
			break
		}
		id, err := connectorID(cl, args[1])
		if err != nil {
			return c.fail(err)
		}
		var out struct {
			Connections []map[string]any `json:"connections"`
		}
		if err := cl.JSON("GET", "/v1/connectors/"+url.PathEscape(id)+"/connections", nil, &out); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, out)
			return ExitOK
		}
		rows := [][]string{{"STARTED", "SANDBOX", "TARGET", "RESULT", "BYTES_IN", "BYTES_OUT"}}
		for _, p := range out.Connections {
			rows = append(rows, []string{unixTime(p["started_at"]), str(p["sandbox_id"]), str(p["target"]), str(p["result"]), str(p["bytes_in"]), str(p["bytes_out"])})
		}
		table(c.stdout, rows)
		return ExitOK
	case "rm", "delete", "revoke":
		if len(args) != 2 {
			break
		}
		id, err := connectorID(cl, args[1])
		if err != nil {
			return c.fail(err)
		}
		if err := cl.JSON("DELETE", "/v1/connectors/"+url.PathEscape(id), nil, nil); err != nil {
			return c.fail(adminOnly(err))
		}
		if c.jsonOut {
			printJSON(c.stdout, map[string]any{"status": "revoked", "name": args[1]})
		} else if !c.quiet {
			fmt.Fprintf(c.stdout, "revoked %s\n", args[1])
		}
		return ExitOK
	case "key", "keys":
		return cmdConnectorKey(c, cl, args[1:])
	}
	fmt.Fprintln(c.stderr, connectorUsage)
	return ExitUsage
}

func unixTime(v any) string {
	if n, ok := v.(float64); ok && n > 0 {
		return time.Unix(int64(n), 0).Local().Format("Jan 2 15:04:05")
	}
	return ""
}

func requirePrivateAccess(c *cmdCtx, cl *Client) (int, bool) {
	var caps struct {
		PrivateAccess bool `json:"private_access"`
	}
	if err := cl.JSON("GET", "/v1/capabilities", nil, &caps); err != nil {
		return c.fail(err), false
	}
	if !caps.PrivateAccess {
		return c.fail(hintedError{"this server does not offer private access",
			"on a self-hosted server, start one orchestrator replica with PRIVATE_INGRESS=1; docs/deployment.md has the steps"}), false
	}
	return 0, true
}

func connectorID(cl *Client, name string) (string, error) {
	var out struct {
		Connectors []struct{ ID, Name string } `json:"connectors"`
	}
	if err := cl.JSON("GET", "/v1/connectors", nil, &out); err != nil {
		return "", err
	}
	for _, c := range out.Connectors {
		if c.Name == name {
			return c.ID, nil
		}
	}
	return "", hintedError{"no connector named " + name, "`pilldrops connector list` shows them"}
}

func cmdConnectorKey(c *cmdCtx, cl *Client, args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(c.stderr, connectorUsage)
		return ExitUsage
	}
	sub := args[0]
	if !(sub == "create" && len(args) == 2) && !(sub == "list" && len(args) == 2) && !(sub == "rm" && len(args) == 3) {
		fmt.Fprintln(c.stderr, connectorUsage)
		return ExitUsage
	}
	id, err := connectorID(cl, args[1])
	if err != nil {
		return c.fail(err)
	}
	base := "/v1/connectors/" + url.PathEscape(id) + "/keys"
	switch sub {
	case "create":
		var out map[string]any
		if err := cl.JSON("POST", base, nil, &out); err != nil {
			return c.fail(adminOnly(err))
		}
		if c.jsonOut {
			printJSON(c.stdout, out)
			return ExitOK
		}
		// The key alone on stdout, for KEY=$(pilldrops connector key create prod).
		fmt.Fprintln(c.stderr, "This key is shown once. Store it now; run the connector with PILLDROPS_CONNECTOR_KEY set to it.")
		fmt.Fprintln(c.stdout, str(out["key"]))
		return ExitOK
	case "list":
		var out struct {
			Keys []map[string]any `json:"keys"`
		}
		if err := cl.JSON("GET", base, nil, &out); err != nil {
			return c.fail(adminOnly(err))
		}
		if c.jsonOut {
			printJSON(c.stdout, out)
			return ExitOK
		}
		rows := [][]string{{"ID", "CREATED", "LAST_USED"}}
		for _, k := range out.Keys {
			rows = append(rows, []string{str(k["id"]), unixTime(k["created_at"]), unixTime(k["last_used_at"])})
		}
		table(c.stdout, rows)
		return ExitOK
	}
	if err := cl.JSON("DELETE", base+"/"+url.PathEscape(args[2]), nil, nil); err != nil {
		return c.fail(adminOnly(err))
	}
	if c.jsonOut {
		printJSON(c.stdout, map[string]any{"status": "revoked", "id": args[2]})
	} else if !c.quiet {
		fmt.Fprintf(c.stdout, "revoked %s\n", args[2])
	}
	return ExitOK
}

func cmdConnectorRun(c *cmdCtx, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, connectorUsage)
		return ExitUsage
	}
	name, rest := args[0], args[1:]
	key := os.Getenv("PILLDROPS_CONNECTOR_KEY")
	var targetArgs []string
	for i := 0; i < len(rest); i++ {
		if rest[i] == "--key" {
			if i+1 == len(rest) {
				fmt.Fprintln(c.stderr, connectorUsage)
				return ExitUsage
			}
			key = rest[i+1]
			i++
			continue
		}
		targetArgs = append(targetArgs, rest[i])
	}
	if !connectorName.MatchString(name) {
		fmt.Fprintln(c.stderr, "connector name must be a lowercase DNS label")
		return ExitUsage
	}
	targets, err := parseConnectorTargets(targetArgs)
	if err != nil {
		fmt.Fprintln(c.stderr, err)
		fmt.Fprintln(c.stderr, connectorUsage)
		return ExitUsage
	}
	var cl *Client
	if key != "" {
		// A connector key is the whole credential on a headless host: no login needed.
		base, _ := resolveCreds(c.apiURL)
		if base == "" {
			base = DefaultAPIURL
		}
		cl = &Client{Base: base, Key: key}
	} else {
		if cl, err = c.client(); err != nil {
			return c.fail(err)
		}
		if code, ok := requirePrivateAccess(c, cl); !ok {
			return code
		}
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cs := &connectorSession{cl: cl, name: name, instance: "inst-" + hex.EncodeToString(b), targets: targets, stderr: c.stderr}
	if reason := cs.run(ctx); reason != "" {
		cs.logf("the connector has stopped: %s\n", reason)
		if cs.refused == http.StatusForbidden && key == "" {
			cs.logf("  hint: in a team org only owners and admins can run a connector with their own login; " +
				"anyone can run it with a connector key from the dashboard in PILLDROPS_CONNECTOR_KEY\n")
		}
		return ExitAPI
	}
	return ExitOK
}

// connectorSession keeps one connector instance connected to the hub.
type connectorSession struct {
	cl             *Client
	name, instance string
	targets        []connectorTarget
	stderr         io.Writer
	connected      bool
	maxConns       int           // 0 means connectorMaxConns
	readWait       time.Duration // 0 means sharePongWait
	refused        int           // the HTTP status that stopped the run, if one did

	logMu sync.Mutex // connections log from their own goroutines

	connMu   sync.Mutex
	inFlight map[string]bool
}

// connectorMaxConns bounds what one process dials at once, whatever the hub asks for.
const connectorMaxConns = 256

func (s *connectorSession) logf(format string, a ...any) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	fmt.Fprintf(s.stderr, format, a...)
}

type connectorSessionInfo struct {
	TunnelURL   string `json:"tunnel_url"`
	ConnURL     string `json:"conn_url"`
	TunnelToken string `json:"tunnel_token"`
}

// run returns why the server stopped the connector, or "" when ctx was cancelled.
func (s *connectorSession) run(ctx context.Context) string {
	wait := time.Second
	for {
		start := time.Now()
		var info connectorSessionInfo
		err := s.cl.JSONContext(ctx, "POST", "/v1/connectors/session", map[string]any{"name": s.name}, &info)
		var apiErr *APIError
		if errors.As(err, &apiErr) && sessionRefused(apiErr.Status) {
			s.refused = apiErr.Status
			return apiErr.Msg
		}
		if err == nil {
			var reason string
			if reason, err = s.connect(ctx, info); reason != "" {
				return reason
			}
		}
		if ctx.Err() != nil {
			return ""
		}
		if time.Since(start) > shareBackoffMax {
			wait = time.Second
		}
		s.logf("connector disconnected (%v); retrying in %s\n", err, wait)
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(wait):
		}
		if wait *= 2; wait > shareBackoffMax {
			wait = shareBackoffMax
		}
	}
}

// sessionRefused is a session answer that retrying cannot change: a bad key,
// not an admin, no private access. Timeouts, rate limits and 5xx are retried.
func sessionRefused(status int) bool {
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
		return false
	}
	return (status >= 400 && status < 500) || status == http.StatusNotImplemented
}

// connect runs one control channel until it drops or the server closes it.
func (s *connectorSession) connect(ctx context.Context, info connectorSessionInfo) (string, error) {
	ctrl, resp, err := websocket.DefaultDialer.DialContext(ctx, info.TunnelURL, http.Header{"Authorization": {"Bearer " + info.TunnelToken}})
	if err != nil {
		// 403 is a bad token and 410 a revoked connector or key; anything else, 503 included, is retried.
		if resp != nil && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone) {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			return apiError(resp.StatusCode, body).Msg, nil
		}
		return "", err
	}
	defer func() { _ = ctrl.Close() }()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = ctrl.Close()
	}()
	var writeMu sync.Mutex
	send := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return ctrl.WriteJSON(v)
	}
	if err := send(map[string]any{"type": "hello", "instance_id": s.instance, "version": Version,
		"capabilities": map[string]any{"targets": s.targets}}); err != nil {
		return "", err
	}
	if s.connected {
		s.logf("connector reconnected\n")
	} else {
		s.logf("connector %s is up; grant a sandbox one of its targets with: pilldrops sandbox grant <sandbox> %s/<target>\n", s.name, s.name)
	}
	s.connected = true
	readWait := s.readWait
	if readWait == 0 {
		readWait = sharePongWait
	}
	_ = ctrl.SetReadDeadline(time.Now().Add(readWait))
	ctrl.SetPingHandler(func(data string) error {
		_ = ctrl.SetReadDeadline(time.Now().Add(readWait))
		writeMu.Lock()
		defer writeMu.Unlock()
		return ctrl.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})
	for {
		var msg struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			Proto  string `json:"proto"`
			Target string `json:"target"`
			Port   int    `json:"port"`
			Reason string `json:"reason"`
		}
		if err := ctrl.ReadJSON(&msg); err != nil {
			return "", err
		}
		// Any message proves the hub is there: a proxy in between may answer
		// its pings itself, so the hub's keepalive may be all that arrives.
		_ = ctrl.SetReadDeadline(time.Now().Add(readWait))
		switch msg.Type {
		case "closed":
			return msg.Reason, nil
		case "open":
			// The id goes into a URL path, so nothing but the expected form is used.
			if !shareOpenID.MatchString(msg.ID) {
				continue
			}
			t, ok := s.target(msg.Target, msg.Port)
			if !ok || (msg.Proto != "" && msg.Proto != "tcp") {
				_ = send(map[string]any{"type": "fail", "id": msg.ID, "reason": "not announced"})
				continue
			}
			switch s.claim(msg.ID) {
			case claimDuplicate:
				continue
			case claimBusy:
				_ = send(map[string]any{"type": "fail", "id": msg.ID, "reason": "busy"})
				continue
			}
			go func(id string) {
				defer s.release(id)
				s.serveConn(ctx, info.ConnURL, id, t, send)
			}(msg.ID)
		}
	}
}

// target matches an open against this process's own list, so a server can
// never make the connector dial anything else.
func (s *connectorSession) target(name string, port int) (connectorTarget, bool) {
	for _, t := range s.targets {
		if t.Name == name && t.Port == port {
			return t, true
		}
	}
	return connectorTarget{}, false
}

const (
	claimOK = iota
	claimDuplicate
	claimBusy
)

func (s *connectorSession) claim(id string) int {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	limit := s.maxConns
	if limit == 0 {
		limit = connectorMaxConns
	}
	switch {
	case s.inFlight[id]:
		return claimDuplicate
	case len(s.inFlight) >= limit:
		return claimBusy
	}
	if s.inFlight == nil {
		s.inFlight = map[string]bool{}
	}
	s.inFlight[id] = true
	return claimOK
}

func (s *connectorSession) release(id string) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	delete(s.inFlight, id)
}

func (s *connectorSession) serveConn(ctx context.Context, connURL, id string, t connectorTarget, send func(any) error) {
	local, err := (&net.Dialer{Timeout: shareDialLocal}).DialContext(ctx, "tcp", t.addr)
	if err != nil {
		_ = send(map[string]any{"type": "fail", "id": id, "reason": "dial"})
		s.logf("%s: cannot reach %s: %v\n", t.Name, t.addr, err)
		return
	}
	defer func() { _ = local.Close() }()
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, connURL+id, nil)
	if err != nil {
		_ = send(map[string]any{"type": "fail", "id": id, "reason": "conn"})
		s.logf("%s: cannot open the data connection: %v\n", t.Name, err)
		return
	}
	remote := &wsConn{ws: ws}
	defer func() { _ = remote.Close() }()
	start := time.Now()
	var toTarget, fromTarget int64
	done := make(chan struct{}, 2)
	go func() { toTarget, _ = io.Copy(local, remote); done <- struct{}{} }()
	go func() { fromTarget, _ = io.Copy(remote, local); done <- struct{}{} }()
	<-done
	_ = local.Close()
	_ = remote.Close()
	<-done
	s.logf("%s  %d B in, %d B out, %s\n", t.Name, toTarget, fromTarget, time.Since(start).Round(time.Millisecond))
}

// sandboxPrivateGrant is sandbox grant|revoke <id> <connector>/<target>.
func sandboxPrivateGrant(c *cmdCtx, cl *Client, sub string, args []string) int {
	var conn, target string
	if len(args) == 2 {
		conn, target, _ = strings.Cut(args[1], "/")
	}
	if conn == "" || target == "" {
		fmt.Fprintf(c.stderr, "usage: pilldrops sandbox %s <id> <connector>/<target>\n", sub)
		return ExitUsage
	}
	path := "/v1/sandboxes/" + url.PathEscape(args[0]) + "/private"
	if sub == "revoke" {
		if err := cl.JSON("DELETE", path+"/"+url.PathEscape(conn)+"/"+url.PathEscape(target), nil, nil); err != nil {
			return c.fail(adminOnly(err))
		}
		if c.jsonOut {
			printJSON(c.stdout, map[string]any{"status": "revoked", "connector": conn, "target": target})
		} else if !c.quiet {
			fmt.Fprintf(c.stdout, "revoked %s/%s\n", conn, target)
		}
		return ExitOK
	}
	var out map[string]any
	if err := cl.JSON("POST", path, map[string]any{"connector": conn, "target": target}, &out); err != nil {
		return c.fail(adminOnly(err))
	}
	if c.jsonOut {
		printJSON(c.stdout, out)
	} else if names, ok := out["names"].([]any); ok {
		for _, n := range names {
			fmt.Fprintln(c.stdout, str(n))
		}
	}
	return ExitOK
}

// copyPrivateGrants re-grants the parent's targets to a fork through the grant
// API, so the caller's own permission decides each one.
func copyPrivateGrants(c *cmdCtx, cl *Client, parent, child string) int {
	var out struct {
		Grants []struct {
			Connector string `json:"connector"`
			Target    string `json:"target"`
		} `json:"grants"`
	}
	if err := cl.JSON("GET", "/v1/sandboxes/"+url.PathEscape(parent)+"/private", nil, &out); err != nil {
		fmt.Fprintf(c.stderr, "%s was forked, but its private-network grants were not copied\n", child)
		return c.fail(err)
	}
	failed := 0
	for _, g := range out.Grants {
		err := cl.JSON("POST", "/v1/sandboxes/"+url.PathEscape(child)+"/private", map[string]any{"connector": g.Connector, "target": g.Target}, nil)
		if err != nil {
			failed++
			fmt.Fprintf(c.stderr, "not copied: %s/%s: %v\n", g.Connector, g.Target, err)
		}
	}
	if failed > 0 {
		fmt.Fprintf(c.stderr, "%s was forked; %d of %d grants were not copied\n", child, failed, len(out.Grants))
		return ExitAPI
	}
	return ExitOK
}

// adminOnly replaces the generic 403 remedy, which points at plan limits, for
// the routes a team org keeps to its owners and admins.
func adminOnly(err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden {
		return pathError{err: err, hint: "in a team org only owners and admins can manage connectors and grants, from the dashboard"}
	}
	return err
}
