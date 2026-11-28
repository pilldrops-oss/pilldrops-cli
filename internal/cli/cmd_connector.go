package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/dns/dnsmessage"
)

const connectorUsage = `usage: pilldrops connector create <name> | run <name> [NAME=HOST:PORT...] [--subnet CIDR]... [--dns SUFFIX]... (key from PILLDROPS_CONNECTOR_KEY) | list | logs <name> | key create|list|rm <name> [key-id] | rm <name>`

var connectorName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

const (
	connectorMaxSubnets  = 20
	connectorMaxSuffixes = 10
	// connectorMaxDNS bounds lookups answered at once, whatever the hub sends.
	connectorMaxDNS = 32
	// connectorMaxAnswered bounds the DNS-learned addresses kept, as the hub and node do.
	connectorMaxAnswered = 1024
	dnsMinTTL            = 30
	dnsMaxTTL            = 300
	dnsQueryBudget       = 2500 * time.Millisecond
	// connectorMaxReplyBytes is the reply size the hub accepts; dnsReplyOverhead
	// leaves room for the message around the answers.
	connectorMaxReplyBytes = 4 << 10
	dnsReplyOverhead       = 256
)

// platformRanges are the ranges the server carves out of a sandbox's routes.
var platformRanges = []netip.Prefix{
	netip.MustParsePrefix("10.128.0.0/16"), netip.MustParsePrefix("192.168.30.0/23"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
}

// parseConnectorSubnets checks --subnet values as the server does: IPv4, /8 to
// /32, no host bits.
func parseConnectorSubnets(args []string) ([]netip.Prefix, error) {
	if len(args) > connectorMaxSubnets {
		return nil, fmt.Errorf("at most %d --subnet values", connectorMaxSubnets)
	}
	out := make([]netip.Prefix, 0, len(args))
	for _, a := range args {
		p, err := netip.ParsePrefix(a)
		if err != nil || !p.Addr().Is4() || p.Bits() < 8 || p.Masked() != p {
			return nil, fmt.Errorf("--subnet %q: want an IPv4 CIDR from /8 to /32 with no host bits, like 10.0.0.0/16", a)
		}
		if slices.Contains(out, p) {
			return nil, fmt.Errorf("--subnet %q is given twice", a)
		}
		for _, r := range platformRanges {
			if r.Bits() <= p.Bits() && r.Contains(p.Addr()) {
				return nil, fmt.Errorf("--subnet %q: every address in it is reserved by the platform (%s)", a, r)
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// parseConnectorSuffixes checks --dns values: two or more lowercase labels,
// not an IP address, outside the sandbox-internal zones.
func parseConnectorSuffixes(args []string) ([]string, error) {
	if len(args) > connectorMaxSuffixes {
		return nil, fmt.Errorf("at most %d --dns values", connectorMaxSuffixes)
	}
	out := make([]string, 0, len(args))
	for _, a := range args {
		d := strings.TrimSuffix(strings.ToLower(a), ".")
		labels := strings.Split(d, ".")
		_, ipErr := netip.ParseAddr(d)
		ok := len(labels) >= 2 && len(d) <= 253 && ipErr != nil
		for _, l := range labels {
			ok = ok && connectorName.MatchString(l)
		}
		switch labels[len(labels)-1] {
		case "pd", "private", "localhost":
			ok = false
		}
		if !ok {
			return nil, fmt.Errorf("--dns %q: want a DNS suffix like corp.internal", a)
		}
		if slices.Contains(out, d) {
			return nil, fmt.Errorf("--dns %q is given twice", a)
		}
		out = append(out, d)
	}
	return out, nil
}

type connectorTarget struct {
	Name string `json:"name"`
	Port int    `json:"port"`
	Host string `json:"host,omitempty"`
	addr string // dialled locally, never sent
}

func parseConnectorTargets(args []string) ([]connectorTarget, error) {
	if len(args) == 0 {
		return nil, errors.New("name at least one target like db=10.0.0.5:5432, or a --subnet or --dns")
	}
	seen := map[string]bool{}
	var out []connectorTarget
	for _, a := range args {
		name, hostport, ok := strings.Cut(a, "=")
		if !ok || !connectorName.MatchString(name) || seen[name] {
			return nil, fmt.Errorf("%q: want NAME=HOST:PORT with a lowercase, unique NAME", a)
		}
		if hostport == "HOST:PORT" {
			return nil, fmt.Errorf("%q: replace HOST:PORT with the target's address, like %s=10.0.0.5:5432", a, name)
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
				Subnets   []string
				DNS       struct{ Suffixes []string }
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
		rows := [][]string{{"NAME", "ONLINE", "INSTANCES", "TARGETS", "ROUTES"}}
		for _, cn := range out.Connectors {
			var ts []string
			for _, t := range cn.Targets {
				ts = append(ts, fmt.Sprintf("%s:%d", t.Name, t.Port))
			}
			rows = append(rows, []string{cn.Name, strconv.FormatBool(cn.Online), strconv.Itoa(cn.Instances), strings.Join(ts, ","),
				strings.Join(append(append([]string{}, cn.Subnets...), cn.DNS.Suffixes...), ",")})
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
	key := os.Getenv("PILLDROPS_CONNECTOR_KEY")
	// Flags may sit anywhere: the dashboard's run hint puts them after a target.
	var positional, subnetArgs, dnsArgs []string
	for i := 0; i < len(args); i++ {
		opt, val, inline := args[i], "", false
		if f, v, ok := strings.Cut(opt, "="); ok && (f == "--key" || f == "--subnet" || f == "--dns") {
			opt, val, inline = f, v, true
		}
		switch opt {
		case "--key", "--subnet", "--dns":
			if !inline {
				if i+1 == len(args) {
					fmt.Fprintln(c.stderr, connectorUsage)
					return ExitUsage
				}
				i++
				val = args[i]
			}
			switch opt {
			case "--key":
				key = val
			case "--subnet":
				subnetArgs = append(subnetArgs, val)
			default:
				dnsArgs = append(dnsArgs, val)
			}
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) == 0 {
		fmt.Fprintln(c.stderr, connectorUsage)
		return ExitUsage
	}
	name, targetArgs := positional[0], positional[1:]
	if !connectorName.MatchString(name) {
		fmt.Fprintln(c.stderr, "connector name must be a lowercase DNS label")
		return ExitUsage
	}
	var targets []connectorTarget
	var err error
	if len(targetArgs) > 0 || (len(subnetArgs) == 0 && len(dnsArgs) == 0) {
		targets, err = parseConnectorTargets(targetArgs)
	}
	subnets, serr := parseConnectorSubnets(subnetArgs)
	suffixes, derr := parseConnectorSuffixes(dnsArgs)
	if err = errors.Join(err, serr, derr); err != nil {
		fmt.Fprintln(c.stderr, err)
		fmt.Fprintln(c.stderr, connectorUsage)
		return ExitUsage
	}
	if len(suffixes) > 0 && len(systemNameservers()) == 0 {
		fmt.Fprintln(c.stderr, "warning: no nameserver found in "+resolvConfPath+", so --dns lookups will fail; --subnet routes still work")
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
	cs := &connectorSession{cl: cl, name: name, instance: "inst-" + hex.EncodeToString(b),
		targets: targets, subnets: subnets, suffixes: suffixes, stderr: c.stderr}
	if reason := cs.run(ctx); reason != "" {
		cs.logf("the connector has stopped: %s\n", reason)
		if cs.teamRefused && key == "" {
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
	subnets        []netip.Prefix
	suffixes       []string
	stderr         io.Writer
	connected      bool
	maxConns       int           // 0 means connectorMaxConns
	readWait       time.Duration // 0 means sharePongWait
	teamRefused    bool          // the session was refused because the login is not a team owner or admin

	logMu sync.Mutex // connections log from their own goroutines

	connMu   sync.Mutex
	inFlight map[string]bool

	// dnsServers is where lookups go; empty reads resolv.conf. Tests set it.
	dnsServers []string
	dnsBusy    atomic.Int32
	// dialLocalAnswers keeps loopback and link-local A records; tests only, their listeners are on loopback.
	dialLocalAnswers bool

	answerMu    sync.Mutex
	answered    map[netip.Addr]time.Time // addresses this process answered, until their TTL
	answerOrder []netip.Addr             // answered's keys, oldest first
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
			s.teamRefused = teamAdminRefusal(apiErr)
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
	targets := s.targets
	if targets == nil {
		targets = []connectorTarget{}
	}
	caps := map[string]any{"targets": targets}
	if len(s.subnets) > 0 {
		nets := make([]string, len(s.subnets))
		for i, p := range s.subnets {
			nets[i] = p.String()
		}
		caps["subnets"] = nets
	}
	if len(s.suffixes) > 0 {
		caps["dns"] = map[string]any{"suffixes": s.suffixes}
	}
	if err := send(map[string]any{"type": "hello", "instance_id": s.instance, "version": Version, "capabilities": caps}); err != nil {
		return "", err
	}
	if s.connected {
		s.logf("connector reconnected\n")
	} else {
		s.logf("connector %s is up; grant a sandbox one of its targets with: pilldrops sandbox grant <sandbox> %s/<target>\n", s.name, s.name)
		if len(s.subnets)+len(s.suffixes) > 0 {
			s.logf("  and a subnet or names under a suffix with %s/subnet:<cidr> or %s/dns:<suffix>\n", s.name, s.name)
		}
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
			Dest   string `json:"dest"`
			Name   string `json:"name"`
			QType  string `json:"qtype"`
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
			var label, addr, resolveName string
			var dest netip.AddrPort
			ok := msg.Proto == "" || msg.Proto == "tcp"
			if ok && msg.Dest != "" {
				dest, ok, resolveName = s.destAllowed(msg.Dest, msg.Name)
				label, addr = dest.String(), dest.String()
			} else if ok {
				var t connectorTarget
				t, ok = s.target(msg.Target, msg.Port)
				label, addr = t.Name, t.addr
			}
			if !ok {
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
				// Resolving here, not in the read loop, keeps a slow resolver from stalling every other message.
				if resolveName != "" && !s.resolvesTo(ctx, resolveName, dest.Addr()) {
					_ = send(map[string]any{"type": "fail", "id": id, "reason": "not announced"})
					return
				}
				s.serveConn(ctx, info.ConnURL, id, label, addr, send)
			}(msg.ID)
		case "dns":
			if !shareOpenID.MatchString(msg.ID) {
				continue
			}
			if s.dnsBusy.Add(1) > connectorMaxDNS {
				s.dnsBusy.Add(-1)
				_ = send(map[string]any{"type": "dns_answer", "id": msg.ID, "rcode": int(dnsmessage.RCodeServerFailure), "answers": []dnsAnswer{}})
				continue
			}
			go func(id, name, qtype string) {
				defer s.dnsBusy.Add(-1)
				rcode, answers := s.answerDNS(ctx, name, qtype)
				_ = send(map[string]any{"type": "dns_answer", "id": id, "rcode": rcode, "answers": answers})
			}(msg.ID, msg.Name, msg.QType)
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

func (s *connectorSession) serveConn(ctx context.Context, connURL, id, label, addr string, send func(any) error) {
	local, err := (&net.Dialer{Timeout: shareDialLocal}).DialContext(ctx, "tcp", addr)
	if err != nil {
		_ = send(map[string]any{"type": "fail", "id": id, "reason": "dial"})
		s.logf("%s: cannot reach %s: %v\n", label, addr, err)
		return
	}
	defer func() { _ = local.Close() }()
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, connURL+id, nil)
	if err != nil {
		_ = send(map[string]any{"type": "fail", "id": id, "reason": "conn"})
		s.logf("%s: cannot open the data connection: %v\n", label, err)
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
	s.logf("%s  %d B in, %d B out, %s\n", label, toTarget, fromTarget, time.Since(start).Round(time.Millisecond))
}

type dnsAnswer struct {
	Type  string `json:"type"`
	Value string `json:"value"`
	TTL   uint32 `json:"ttl"`
}

func (s *connectorSession) underSuffix(name string) bool {
	for _, d := range s.suffixes {
		if name == d || strings.HasSuffix(name, "."+d) {
			return true
		}
	}
	return false
}

// destAllowed reports whether an open naming dest may be dialled: inside one of
// this process's own --subnet ranges, or an address it answered for its own
// --dns suffixes within the TTL. When neither holds but name is under its
// suffixes, it returns name for the caller to resolve off the read loop, for an
// instance that did not answer the lookup. Whatever the server sends, nothing
// else is dialled.
func (s *connectorSession) destAllowed(raw, name string) (dest netip.AddrPort, ok bool, resolve string) {
	dest, err := netip.ParseAddrPort(raw)
	if err != nil || !dest.Addr().Is4() || dest.Port() == 0 {
		return netip.AddrPort{}, false, ""
	}
	for _, p := range s.subnets {
		if p.Contains(dest.Addr()) {
			return dest, true, ""
		}
	}
	if s.answeredRecently(dest.Addr()) {
		return dest, true, ""
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	if name == "" || !s.underSuffix(name) {
		return netip.AddrPort{}, false, ""
	}
	return dest, true, name
}

// resolvesTo reports whether name, resolved by this process, answers addr.
func (s *connectorSession) resolvesTo(ctx context.Context, name string, addr netip.Addr) bool {
	answers, rcode := s.resolve(ctx, name, "A")
	if rcode != dnsmessage.RCodeSuccess {
		return false
	}
	for _, a := range answers {
		if a.Type == "A" && a.Value == addr.String() {
			return true
		}
	}
	return false
}

// answerDNS resolves a relayed lookup with this machine's resolver: A and
// CNAME chains with their TTLs, AAAA empty, other types NOTIMP, names outside
// this connector's --dns suffixes REFUSED.
func (s *connectorSession) answerDNS(ctx context.Context, name, qtype string) (int, []dnsAnswer) {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	if !s.underSuffix(name) {
		return int(dnsmessage.RCodeRefused), []dnsAnswer{}
	}
	switch qtype {
	case "AAAA":
		return int(dnsmessage.RCodeSuccess), []dnsAnswer{}
	case "A", "CNAME":
	default:
		return int(dnsmessage.RCodeNotImplemented), []dnsAnswer{}
	}
	answers, rcode := s.resolve(ctx, name, qtype)
	if answers == nil {
		answers = []dnsAnswer{}
	}
	return int(rcode), answers
}

// resolve asks the machine's resolvers (dnsServers in tests) in order for name
// and returns the answer chain in order, at most 32 records and a reply the hub
// accepts, remembering each A address for its TTL clamped to 30-300 s, as the
// hub does. A resolver that times out, errors or answers SERVFAIL or REFUSED
// passes the lookup to the next; NXDOMAIN and good answers end it.
func (s *connectorSession) resolve(ctx context.Context, name, qtype string) ([]dnsAnswer, dnsmessage.RCode) {
	servers := s.dnsServers
	if len(servers) == 0 {
		servers = systemNameservers()
	}
	qname, err := dnsmessage.NewName(name + ".")
	if len(servers) == 0 || err != nil {
		return nil, dnsmessage.RCodeServerFailure
	}
	typ := dnsmessage.TypeA
	if qtype == "CNAME" {
		typ = dnsmessage.TypeCNAME
	}
	var idb [2]byte
	_, _ = rand.Read(idb[:])
	id := uint16(idb[0])<<8 | uint16(idb[1])
	req, err := (&dnsmessage.Message{Header: dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: qname, Type: typ, Class: dnsmessage.ClassINET}}}).Pack()
	if err != nil {
		return nil, dnsmessage.RCodeServerFailure
	}
	// The hub waits 3 s for a reply, so the servers share one budget.
	budget := dnsQueryBudget / time.Duration(len(servers))
	failure := dnsmessage.RCodeServerFailure
	var resp dnsmessage.Message
	answered := false
	for _, server := range servers {
		deadline := time.Now().Add(budget)
		resp, err = dnsExchange(ctx, "udp", server, req, deadline, id, qname, typ)
		if err == nil && resp.Truncated {
			resp, err = dnsExchange(ctx, "tcp", server, req, deadline, id, qname, typ)
		}
		if err != nil {
			continue
		}
		if resp.RCode == dnsmessage.RCodeServerFailure || resp.RCode == dnsmessage.RCodeRefused {
			failure = resp.RCode
			continue
		}
		answered = true
		break
	}
	if !answered {
		return nil, failure
	}
	if resp.RCode != dnsmessage.RCodeSuccess {
		return nil, resp.RCode
	}
	var out []dnsAnswer
	size := dnsReplyOverhead
	for _, a := range resp.Answers {
		var ans dnsAnswer
		switch b := a.Body.(type) {
		case *dnsmessage.CNAMEResource:
			ans = dnsAnswer{"CNAME", strings.TrimSuffix(b.CNAME.String(), "."), a.Header.TTL}
		case *dnsmessage.AResource:
			ip := netip.AddrFrom4(b.A)
			// A record under a suffix must not open this host's own or metadata addresses.
			if !s.dialLocalAnswers && (ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast()) {
				continue
			}
			ans = dnsAnswer{"A", ip.String(), a.Header.TTL}
		default:
			continue
		}
		enc, _ := json.Marshal(ans)
		if size += len(enc) + 1; size > connectorMaxReplyBytes {
			break
		}
		out = append(out, ans)
		if ans.Type == "A" {
			s.remember(netip.MustParseAddr(ans.Value), ans.TTL)
		}
		if len(out) == 32 {
			break
		}
	}
	return out, dnsmessage.RCodeSuccess
}

// dnsExchange sends req to server over network and returns the matching reply,
// before deadline.
func dnsExchange(ctx context.Context, network, server string, req []byte, deadline time.Time, id uint16, qname dnsmessage.Name, typ dnsmessage.Type) (dnsmessage.Message, error) {
	var resp dnsmessage.Message
	conn, err := (&net.Dialer{Deadline: deadline}).DialContext(ctx, network, server)
	if err != nil {
		return resp, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(deadline)
	if network == "tcp" {
		if _, err := conn.Write(append([]byte{byte(len(req) >> 8), byte(len(req))}, req...)); err != nil {
			return resp, err
		}
		var n [2]byte
		if _, err := io.ReadFull(conn, n[:]); err != nil {
			return resp, err
		}
		buf := make([]byte, int(n[0])<<8|int(n[1]))
		if _, err := io.ReadFull(conn, buf); err != nil {
			return resp, err
		}
		if err := resp.Unpack(buf); err != nil || !dnsReplyMatches(&resp, id, qname, typ) {
			return resp, errors.New("bad DNS reply")
		}
		return resp, nil
	}
	if _, err := conn.Write(req); err != nil {
		return resp, err
	}
	buf := make([]byte, 1500)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return resp, err
		}
		// A stray or spoofed datagram is skipped; the real reply may still come before the deadline.
		if resp.Unpack(buf[:n]) == nil && dnsReplyMatches(&resp, id, qname, typ) {
			return resp, nil
		}
	}
}

func dnsReplyMatches(resp *dnsmessage.Message, id uint16, qname dnsmessage.Name, typ dnsmessage.Type) bool {
	return resp.ID == id && resp.Response && len(resp.Questions) == 1 &&
		strings.EqualFold(resp.Questions[0].Name.String(), qname.String()) &&
		resp.Questions[0].Type == typ && resp.Questions[0].Class == dnsmessage.ClassINET
}

func (s *connectorSession) remember(a netip.Addr, ttl uint32) {
	ttl = max(dnsMinTTL, min(ttl, dnsMaxTTL))
	s.answerMu.Lock()
	defer s.answerMu.Unlock()
	if s.answered == nil {
		s.answered = map[netip.Addr]time.Time{}
	}
	if _, ok := s.answered[a]; !ok {
		// ponytail: oldest by first answer, not by expiry; a refreshed address keeps its place.
		if len(s.answerOrder) >= connectorMaxAnswered {
			delete(s.answered, s.answerOrder[0])
			s.answerOrder = s.answerOrder[1:]
		}
		s.answerOrder = append(s.answerOrder, a)
	}
	s.answered[a] = time.Now().Add(time.Duration(ttl) * time.Second)
}

func (s *connectorSession) answeredRecently(a netip.Addr) bool {
	s.answerMu.Lock()
	defer s.answerMu.Unlock()
	exp, ok := s.answered[a]
	return ok && exp.After(time.Now())
}

// resolvConfPath is where systemNameservers reads; tests point it elsewhere.
var resolvConfPath = "/etc/resolv.conf"

// systemNameservers lists the first three nameservers in resolv.conf as
// host:53, in order, as glibc does.
// ponytail: a machine with no resolv.conf (Windows) answers no split DNS;
// its subnets still work.
func systemNameservers() []string {
	b, err := os.ReadFile(resolvConfPath)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			if ip, err := netip.ParseAddr(f[1]); err == nil {
				out = append(out, netip.AddrPortFrom(ip.WithZone(""), 53).String())
			}
		}
	}
	return out[:min(len(out), 3)]
}

const grantArgUsage = "<connector>/<target>|<connector>/subnet:<cidr>|<connector>/dns:<suffix>"

// privateGrantArg parses <connector>/<target>, <connector>/subnet:<cidr> or
// <connector>/dns:<suffix> into a grant request body.
func privateGrantArg(arg string) (map[string]any, bool) {
	conn, rest, _ := strings.Cut(arg, "/")
	if conn == "" || rest == "" {
		return nil, false
	}
	body := map[string]any{"connector": conn}
	switch {
	case strings.HasPrefix(rest, "subnet:"):
		body["subnet"] = strings.TrimPrefix(rest, "subnet:")
	case strings.HasPrefix(rest, "dns:"):
		body["dns"] = strings.TrimPrefix(rest, "dns:")
	default:
		body["target"] = rest
	}
	for _, k := range []string{"subnet", "dns", "target"} {
		if v, ok := body[k]; ok && v == "" {
			return nil, false
		}
	}
	return body, true
}

// sandboxPrivateGrant is sandbox grant|revoke <id> <connector>/<what>.
func sandboxPrivateGrant(c *cmdCtx, cl *Client, sub string, args []string) int {
	var body map[string]any
	ok := len(args) == 2
	if ok {
		body, ok = privateGrantArg(args[1])
	}
	if !ok {
		fmt.Fprintf(c.stderr, "usage: pilldrops sandbox %s <id> %s\n", sub, grantArgUsage)
		return ExitUsage
	}
	path := "/v1/sandboxes/" + url.PathEscape(args[0]) + "/private"
	if sub == "revoke" {
		del := path + "/" + url.PathEscape(str(body["connector"])) + "/" + url.PathEscape(str(body["target"]))
		if _, isTarget := body["target"]; !isTarget {
			id, err := ownGrantID(cl, path, body)
			if err != nil {
				return c.fail(err)
			}
			del = path + "/" + url.PathEscape(id)
		}
		if err := cl.JSON("DELETE", del, nil, nil); err != nil {
			return c.fail(adminOnly(err))
		}
		if c.jsonOut {
			out := map[string]any{"status": "revoked"}
			maps.Copy(out, body)
			printJSON(c.stdout, out)
		} else if !c.quiet {
			fmt.Fprintf(c.stdout, "revoked %s\n", args[1])
		}
		return ExitOK
	}
	var out map[string]any
	if err := cl.JSON("POST", path, body, &out); err != nil {
		return c.fail(adminOnly(err))
	}
	if c.jsonOut {
		printJSON(c.stdout, out)
		return ExitOK
	}
	printGrant(c, out)
	return ExitOK
}

// printGrant prints the names a target answers to, or a subnet or suffix
// grant's id and value, and on stderr any part the platform keeps for itself.
func printGrant(c *cmdCtx, out map[string]any) {
	if names, ok := out["names"].([]any); ok {
		for _, n := range names {
			fmt.Fprintln(c.stdout, str(n))
		}
		return
	}
	fmt.Fprintf(c.stdout, "%s\t%s\n", str(out["id"]), str(out["value"]))
	printShadowed(c, out)
}

// printShadowed warns, on stderr, of the ranges of a grant the platform keeps for itself.
func printShadowed(c *cmdCtx, out map[string]any) {
	if sh, ok := out["shadowed"].([]any); ok && len(sh) > 0 {
		parts := make([]string, len(sh))
		for i, s := range sh {
			parts[i] = str(s)
		}
		fmt.Fprintf(c.stderr, "not routed, reserved by the platform: %s\n", strings.Join(parts, ", "))
	}
}

// ownGrantID finds the id of the sandbox's own subnet or DNS grant that body names.
func ownGrantID(cl *Client, path string, body map[string]any) (string, error) {
	var out struct {
		Grants []struct{ ID, Connector, Kind, Value, Source string } `json:"grants"`
	}
	if err := cl.JSON("GET", path, nil, &out); err != nil {
		return "", err
	}
	kind, value := "subnet", str(body["subnet"])
	if p, err := netip.ParsePrefix(value); err == nil {
		value = p.Masked().String()
	}
	if v, ok := body["dns"]; ok {
		kind, value = "dns", strings.TrimSuffix(strings.ToLower(str(v)), ".")
	}
	for _, g := range out.Grants {
		if g.Connector != str(body["connector"]) || g.Kind != kind || g.Value != value {
			continue
		}
		if g.Source == "sandbox" {
			return g.ID, nil
		}
		return "", hintedError{"that grant comes from " + g.Source + ", not from the sandbox itself",
			"revoke it on the tag with `pilldrops private revoke " + g.ID + "`"}
	}
	return "", hintedError{"this sandbox has no such grant", "the sandbox's Private access panel in the dashboard lists its grants"}
}

// copyPrivateGrants re-grants the parent's own grants to a fork through the
// grant API, so the caller's own permission decides each one. Grants the
// parent has through a tag follow the tag, not the fork.
func copyPrivateGrants(c *cmdCtx, cl *Client, parent, child string) int {
	var out struct {
		Grants []struct{ Connector, Target, Kind, Value, Source string } `json:"grants"`
	}
	if err := cl.JSON("GET", "/v1/sandboxes/"+url.PathEscape(parent)+"/private", nil, &out); err != nil {
		fmt.Fprintf(c.stderr, "%s was forked, but its private-network grants were not copied\n", child)
		return c.fail(err)
	}
	tried, failed := 0, 0
	for _, g := range out.Grants {
		if g.Source != "" && g.Source != "sandbox" {
			continue
		}
		body := map[string]any{"connector": g.Connector}
		label := g.Connector + "/" + g.Target
		switch g.Kind {
		case "subnet", "dns":
			body[g.Kind] = g.Value
			label = g.Connector + "/" + g.Kind + ":" + g.Value
		default:
			body["target"] = g.Target
		}
		tried++
		if err := cl.JSON("POST", "/v1/sandboxes/"+url.PathEscape(child)+"/private", body, nil); err != nil {
			failed++
			fmt.Fprintf(c.stderr, "not copied: %s: %v\n", label, err)
		}
	}
	if failed > 0 {
		fmt.Fprintf(c.stderr, "%s was forked; %d of %d grants were not copied\n", child, failed, tried)
		return ExitAPI
	}
	return ExitOK
}

// teamAdminRefusal reports a 403 that is the server's team-admin refusal, not
// a plan limit or another forbidden.
func teamAdminRefusal(apiErr *APIError) bool {
	return apiErr.Status == http.StatusForbidden &&
		strings.Contains(strings.ToLower(apiErr.Msg+" "+apiErr.Code), "owner or admin of this team")
}

// adminOnly replaces the generic 403 remedy, which points at plan limits, for
// the routes a team org keeps to its owners and admins.
func adminOnly(err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) && teamAdminRefusal(apiErr) {
		return pathError{err: err, hint: "in a team org only owners and admins can manage connectors and grants, from the dashboard"}
	}
	return err
}
