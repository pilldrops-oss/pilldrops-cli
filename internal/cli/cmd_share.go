package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/signal"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const shareUsage = `usage: pilldrops share <port> [--ttl <duration>]`

const (
	shareBackoffMax = 30 * time.Second
	shareDialLocal  = 5 * time.Second
	sharePongWait   = 90 * time.Second
	shareWarnEvery  = 10 * time.Second
)

var shareOpenID = regexp.MustCompile(`^[0-9a-f]{32}$`)

type shareCreated struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	TunnelURL   string `json:"tunnel_url"`
	TunnelToken string `json:"tunnel_token"`
	ExpiresAt   int64  `json:"expires_at"`
}

// cmdShare gives an app on this machine a public URL until stopped. The server
// never names a host or port: the only thing this ever dials is <port> on loopback.
func cmdShare(c *cmdCtx, args []string) int {
	port, ttl := 0, time.Duration(0)
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--ttl" && i+1 < len(args):
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil || d <= 0 {
				fmt.Fprintln(c.stderr, shareUsage)
				return ExitUsage
			}
			ttl = d
		case port == 0:
			n, err := strconv.Atoi(a)
			if err != nil || n < 1 || n > 65535 {
				fmt.Fprintln(c.stderr, shareUsage)
				return ExitUsage
			}
			port = n
		default:
			fmt.Fprintln(c.stderr, shareUsage)
			return ExitUsage
		}
	}
	if port == 0 {
		fmt.Fprintln(c.stderr, shareUsage)
		return ExitUsage
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	var caps struct {
		ShareLinks bool `json:"share_links"`
	}
	if err := cl.JSON("GET", "/v1/capabilities", nil, &caps); err != nil {
		return c.fail(err)
	}
	if !caps.ShareLinks {
		return c.fail(hintedError{"this server does not offer share links",
			"on a self-hosted server, set SHARE_DOMAIN and SHARE_INGRESS; docs/deployment.md has the steps"})
	}
	body := map[string]any{}
	if ttl > 0 {
		body["ttl_seconds"] = int64(ttl / time.Second)
	}
	var sh shareCreated
	if err := cl.JSON("POST", "/v1/shares", body, &sh); err != nil {
		return c.fail(err)
	}
	if conn, err := dialLoopback(port); err != nil {
		fmt.Fprintf(c.stderr, "nothing is listening on port %d yet; the link will work once it is\n", port)
	} else {
		_ = conn.Close()
	}
	if c.jsonOut {
		printJSON(c.stdout, map[string]any{"url": sh.URL, "name": sh.Name, "expires_at": sh.ExpiresAt})
	} else if !c.quiet {
		fmt.Fprintf(c.stdout, "%s -> localhost:%d\n", sh.URL, port)
		fmt.Fprintf(c.stdout, "Expires %s. Press Ctrl-C to stop.\n", time.Unix(sh.ExpiresAt, 0).Local().Format("Jan 2 15:04"))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ss := &shareSession{cl: cl, id: sh.ID, port: port, tunnelURL: sh.TunnelURL, token: sh.TunnelToken, stderr: c.stderr}
	reason := ss.run(ctx)
	if reason == "" {
		if err := cl.JSON("DELETE", "/v1/shares/"+url.PathEscape(sh.ID), nil, nil); err != nil {
			fmt.Fprintf(c.stderr, "could not close the link; it ends at its expiry: %v\n", err)
		}
		return ExitOK
	}
	fmt.Fprintf(c.stderr, "the link has ended: %s\n", reason)
	return ExitAPI
}

func dialLoopback(port int) (net.Conn, error) {
	portS := strconv.Itoa(port)
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", portS), shareDialLocal)
	if err == nil {
		return conn, nil
	}
	// Some dev servers bind only ::1.
	if conn, err6 := net.DialTimeout("tcp", net.JoinHostPort("::1", portS), shareDialLocal); err6 == nil {
		return conn, nil
	}
	return nil, err
}

// shareSession keeps one share's tunnel connected.
type shareSession struct {
	cl         *Client
	id         string
	port       int
	tunnelURL  string
	token      string
	stderr     io.Writer
	backoffMin time.Duration

	connected bool // a control channel has been up before, so the next one is a reconnect

	warnMu   sync.Mutex
	lastWarn time.Time
}

// run returns the reason the server ended the link, or "" when ctx was cancelled.
func (s *shareSession) run(ctx context.Context) string {
	minWait := s.backoffMin
	if minWait == 0 {
		minWait = time.Second
	}
	wait := minWait
	fresh := true
	for {
		if !fresh {
			var sh shareCreated
			err := s.cl.JSON("POST", "/v1/shares/"+url.PathEscape(s.id)+"/token", nil, &sh)
			var apiErr *APIError
			if errors.As(err, &apiErr) && (apiErr.Status == http.StatusGone || apiErr.Status == http.StatusNotFound) {
				return endedReason(apiErr)
			}
			if err == nil {
				s.token, s.tunnelURL = sh.TunnelToken, sh.TunnelURL
			}
		}
		fresh = false
		start := time.Now()
		reason, err := s.connect(ctx)
		if reason != "" || ctx.Err() != nil {
			return reason
		}
		if err != nil {
			fmt.Fprintf(s.stderr, "tunnel lost, reconnecting: %v\n", err)
		}
		if time.Since(start) > shareBackoffMax {
			wait = minWait
		}
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

func endedReason(apiErr *APIError) string {
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(apiErr.Body, &body); err == nil && body.Reason != "" {
		return body.Reason
	}
	return apiErr.Msg
}

// connect runs one control channel until it drops or the server ends the link.
func (s *shareSession) connect(ctx context.Context) (string, error) {
	u := s.tunnelURL + "?share=" + url.QueryEscape(s.id)
	h := http.Header{"Authorization": {"Bearer " + s.token}}
	ctrl, _, err := websocket.DefaultDialer.DialContext(ctx, u, h)
	if err != nil {
		return "", err
	}
	defer func() { _ = ctrl.Close() }()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = ctrl.Close()
	}()
	if err := ctrl.WriteJSON(map[string]any{"type": "hello", "port": s.port}); err != nil {
		return "", err
	}
	if s.connected {
		fmt.Fprintln(s.stderr, "tunnel reconnected")
	}
	s.connected = true
	_ = ctrl.SetReadDeadline(time.Now().Add(sharePongWait))
	ctrl.SetPingHandler(func(data string) error {
		_ = ctrl.SetReadDeadline(time.Now().Add(sharePongWait))
		return ctrl.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})
	for {
		var msg struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			Reason string `json:"reason"`
		}
		if err := ctrl.ReadJSON(&msg); err != nil {
			return "", err
		}
		switch msg.Type {
		case "closed":
			return msg.Reason, nil
		case "open":
			// The id goes into a URL path, so nothing but the expected form is used.
			if shareOpenID.MatchString(msg.ID) {
				go s.serveConn(ctx, msg.ID)
			}
		}
	}
}

// serveConn answers one open. With the app down it still connects and closes,
// so the visitor gets the error page at once rather than after the server's timeout.
func (s *shareSession) serveConn(ctx context.Context, id string) {
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, s.tunnelURL+"/conn/"+id, nil)
	if err != nil {
		return
	}
	remote := &wsConn{ws: ws}
	defer func() { _ = remote.Close() }()
	app, err := dialLoopback(s.port)
	if err != nil {
		s.warnAppDown()
		return
	}
	defer func() { _ = app.Close() }()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(app, remote); done <- struct{}{} }()
	go func() { _, _ = io.Copy(remote, app); done <- struct{}{} }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (s *shareSession) warnAppDown() {
	s.warnMu.Lock()
	defer s.warnMu.Unlock()
	if time.Since(s.lastWarn) < shareWarnEvery {
		return
	}
	s.lastWarn = time.Now()
	fmt.Fprintf(s.stderr, "a visitor got an error: nothing is listening on port %d\n", s.port)
}
