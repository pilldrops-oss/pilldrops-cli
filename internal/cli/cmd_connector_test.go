package cli

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const teamAdminMsg = "only an owner or admin of this team can change connectors and grants; sign in to the dashboard to do it"

// fakeConnectorServer is the API and the connector hub in one, driven by the test.
type fakeConnectorServer struct {
	srv          *httptest.Server
	mu           sync.Mutex
	ctrl         *websocket.Conn
	hello        string
	fails        chan string
	dataConns    chan *websocket.Conn
	sessionAuth  atomic.Value
	parentGrants string
	grantCalls   atomic.Int32
	grantBody    atomic.Value
	createBody   atomic.Value
	sessionCode  atomic.Int32 // answered once in place of a session
	sessionHang  chan struct{}
	tunnelCode   atomic.Int32 // answered once in place of the control socket
	sessionMsg   string       // the error of a refused session; set before the run starts
	connRefused  atomic.Bool  // refuse the data sockets
	createDenied string       // when set, POST /v1/sandboxes answers 403 with this error
}

func newFakeConnectorServer(t *testing.T, private bool) *fakeConnectorServer {
	t.Helper()
	f := &fakeConnectorServer{fails: make(chan string, 8), dataConns: make(chan *websocket.Conn, 8), parentGrants: `[]`}
	ws := func() string { return "ws" + strings.TrimPrefix(f.srv.URL, "http") }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/capabilities", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"private_access": private, "features": []string{}})
	})
	mux.HandleFunc("GET /v1/connectors", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"connectors": []map[string]any{
			{"id": "cn_1", "name": "prod", "online": true, "instances": 1, "targets": []map[string]any{{"name": "db", "port": 5432}}}}})
	})
	mux.HandleFunc("POST /v1/connectors/session", func(w http.ResponseWriter, r *http.Request) {
		f.sessionAuth.Store(r.Header.Get("Authorization"))
		if f.sessionHang != nil {
			<-f.sessionHang
		}
		if code := f.sessionCode.Swap(0); code != 0 {
			w.WriteHeader(int(code))
			msg := f.sessionMsg
			if msg == "" {
				msg = "session refused"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"error": msg})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "cn_1", "name": "prod",
			"tunnel_url": ws() + "/v1/connectors/cn_1/tunnel", "conn_url": ws() + "/v1/connector-conns/", "tunnel_token": "tok"})
	})
	mux.HandleFunc("GET /v1/connectors/{id}/tunnel", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if code := f.tunnelCode.Swap(0); code != 0 {
			w.WriteHeader(int(code))
			_, _ = io.WriteString(w, `{"error":"connector revoked"}`)
			return
		}
		c, err := shareTestUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_, hello, err := c.ReadMessage()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.ctrl, f.hello = c, string(hello)
		f.mu.Unlock()
		for {
			var msg map[string]any
			if err := c.ReadJSON(&msg); err != nil {
				return
			}
			if msg["type"] == "fail" {
				f.fails <- str(msg["id"]) + " " + str(msg["reason"])
			}
		}
	})
	mux.HandleFunc("GET /v1/connector-conns/{id}", func(w http.ResponseWriter, r *http.Request) {
		if f.connRefused.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		c, err := shareTestUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.dataConns <- c
	})
	mux.HandleFunc("POST /v1/connectors", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"`+teamAdminMsg+`"}`)
	})
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		if f.createDenied != "" {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": f.createDenied})
			return
		}
		raw, _ := io.ReadAll(r.Body)
		f.createBody.Store(string(raw))
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "sb1", "status": "running"})
	})
	mux.HandleFunc("POST /v1/sandboxes/sb1/fork", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "sb2", "status": "running"})
	})
	mux.HandleFunc("GET /v1/sandboxes/sb1/private", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"grants":`+f.parentGrants+`,"supported":true}`)
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/private", func(w http.ResponseWriter, r *http.Request) {
		f.grantCalls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		f.grantBody.Store(r.PathValue("id") + " " + string(raw))
		if strings.Contains(string(raw), `"target":"api"`) {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"name taken","code":"private_name_taken"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"connector":"prod","target":"db","names":["db.private"]}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	// Ends a connector run left going by the test, before the server goes.
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.ctrl != nil {
			_ = f.ctrl.WriteJSON(map[string]any{"type": "closed", "reason": "test over"})
		}
	})
	return f
}

func (f *fakeConnectorServer) waitHello(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		h := f.hello
		f.mu.Unlock()
		if h != "" {
			return h
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the connector never said hello")
	return ""
}

var openSeq atomic.Int32

// open asks the connector for a connection and reports "ok" when a data socket
// arrives and echoes, "fail" when the connector refuses it first.
func (f *fakeConnectorServer) open(t *testing.T, target string, port int) string {
	t.Helper()
	id := strings.Repeat("0", 31) + strconv.Itoa(int(openSeq.Add(1))%10)
	f.send(t, map[string]any{"type": "open", "id": id, "proto": "tcp", "target": target, "port": port})
	select {
	case c := <-f.dataConns:
		conn := &wsConn{ws: c}
		defer conn.Close()
		_, _ = conn.Write([]byte("ping"))
		buf := make([]byte, 4)
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
			return "no echo"
		}
		return "ok"
	case got := <-f.fails:
		if !strings.HasPrefix(got, id+" ") {
			return "fail for " + got
		}
		return "fail"
	case <-time.After(2 * time.Second):
		return "timeout"
	}
}

func (f *fakeConnectorServer) send(t *testing.T, msg map[string]any) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.ctrl.WriteJSON(msg); err != nil {
		t.Fatal(err)
	}
}

// nothing checks that the connector neither opened a data socket nor failed.
func (f *fakeConnectorServer) nothing(t *testing.T) {
	t.Helper()
	select {
	case <-f.dataConns:
		t.Fatal("a data connection was opened")
	case got := <-f.fails:
		t.Fatalf("a fail was sent: %s", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func (f *fakeConnectorServer) failReason(t *testing.T, id string) string {
	t.Helper()
	select {
	case got := <-f.fails:
		reason, ok := strings.CutPrefix(got, id+" ")
		if !ok {
			t.Fatalf("fail for another id: %s", got)
		}
		return reason
	case <-f.dataConns:
		t.Fatal("a data connection was opened")
	case <-time.After(2 * time.Second):
		t.Fatal("no fail")
	}
	return ""
}

// runConnector runs a session directly, for limits the command line cannot set.
func runConnector(t *testing.T, f *fakeConnectorServer, maxConns int, args ...string) {
	t.Helper()
	targets, err := parseConnectorTargets(args)
	if err != nil {
		t.Fatal(err)
	}
	cs := &connectorSession{cl: &Client{Base: f.srv.URL, Key: "k"}, name: "prod", instance: "inst-0123456789abcdef",
		targets: targets, stderr: io.Discard, maxConns: maxConns}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go cs.run(ctx)
	f.waitHello(t)
}

type onceListener struct {
	l       net.Listener
	port    string
	portInt int
}

func listenOnce(t *testing.T) onceListener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	p := l.Addr().(*net.TCPAddr).Port
	return onceListener{l: l, port: strconv.Itoa(p), portInt: p}
}

func TestConnectorRunAnnouncesNamesNotIPs(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	go authedCLI(t, f.srv, "connector", "run", "prod", "db=10.0.0.5:5432", "api=internal.corp:443", "cache=localhost:6379", "web=10.0.0.6.:80")
	hello := f.waitHello(t)
	for _, leak := range []string{"10.0.0.5", "localhost", "10.0.0.6"} {
		if strings.Contains(hello, leak) {
			t.Fatalf("hello carried host %s: %s", leak, hello)
		}
	}
	for _, want := range []string{`"type":"hello"`, `"name":"db","port":5432}`, `"name":"api","port":443,"host":"internal.corp"`, `"name":"cache","port":6379}`, `"instance_id"`} {
		if !strings.Contains(hello, want) {
			t.Errorf("hello missing %s: %s", want, hello)
		}
	}
}

func TestConnectorRunDialsOnlyAnnouncedTargets(t *testing.T) {
	good := listenOnce(t)
	other := listenOnce(t)
	var otherHit atomic.Bool
	go func() {
		if c, err := other.l.Accept(); err == nil {
			otherHit.Store(true)
			_ = c.Close()
		}
	}()
	go func() {
		c, err := good.l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()
	f := newFakeConnectorServer(t, true)
	go authedCLI(t, f.srv, "connector", "run", "prod", "db=127.0.0.1:"+good.port)
	f.waitHello(t)
	if got := f.open(t, "db", good.portInt); got != "ok" {
		t.Fatalf("announced target: %s", got)
	}
	if got := f.open(t, "db", other.portInt); got != "fail" {
		t.Fatalf("announced name on another port: %s", got)
	}
	if got := f.open(t, "evil", other.portInt); got != "fail" {
		t.Fatalf("unannounced name: %s", got)
	}
	time.Sleep(200 * time.Millisecond)
	if otherHit.Load() {
		t.Fatal("the second listener saw a connection")
	}
}

func TestConnectorRunReportsAFailedDial(t *testing.T) {
	down := listenOnce(t)
	_ = down.l.Close()
	f := newFakeConnectorServer(t, true)
	go authedCLI(t, f.srv, "connector", "run", "prod", "db=127.0.0.1:"+down.port)
	f.waitHello(t)
	if got := f.open(t, "db", down.portInt); got != "fail" {
		t.Fatalf("dial to a closed port: %s", got)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestConnectorRunReportsAFailedDataSocket(t *testing.T) {
	target := listenOnce(t)
	f := newFakeConnectorServer(t, true)
	f.connRefused.Store(true)
	var logs syncBuffer
	targets, err := parseConnectorTargets([]string{"db=127.0.0.1:" + target.port})
	if err != nil {
		t.Fatal(err)
	}
	cs := &connectorSession{cl: &Client{Base: f.srv.URL, Key: "k"}, name: "prod", instance: "inst-0123456789abcdef", targets: targets, stderr: &logs}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go cs.run(ctx)
	f.waitHello(t)
	id := strings.Repeat("0", 31) + "7"
	f.send(t, map[string]any{"type": "open", "id": id, "proto": "tcp", "target": "db", "port": target.portInt})
	if got := f.failReason(t, id); got != "conn" {
		t.Fatalf("fail reason %q, want conn", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "cannot open the data connection") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if lines := strings.Count(logs.String(), "cannot open the data connection"); lines != 1 {
		t.Fatalf("want one log line, got %d in %q", lines, logs.String())
	}
}

func TestConnectorRunRejectsBadTargets(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	for _, args := range [][]string{{}, {"db=10.0.0.5"}, {"DB=10.0.0.5:5432"}, {"db=10.0.0.5:0"}, {"db=:5432"},
		{"db=10.0.0.5:5432", "db=10.0.0.6:5432"}, {"--key"}} {
		code, _, errS := authedCLI(t, f.srv, append([]string{"connector", "run", "prod"}, args...)...)
		if code != ExitUsage {
			t.Errorf("%v: code %d %s", args, code, errS)
		}
	}
	if code, _, _ := authedCLI(t, f.srv, "connector", "run", "Prod", "db=10.0.0.5:5432"); code != ExitUsage {
		t.Errorf("bad connector name: code %d", code)
	}
}

func TestConnectorRefusesServerWithoutIt(t *testing.T) {
	f := newFakeConnectorServer(t, false)
	code, _, errS := authedCLI(t, f.srv, "connector", "list")
	if code == ExitOK || !strings.Contains(errS, "does not offer private access") {
		t.Fatalf("%d %s", code, errS)
	}
}

func TestConnectorListPrintsTargets(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	code, out, errS := authedCLI(t, f.srv, "connector", "list")
	if code != ExitOK || !strings.Contains(out, "prod") || !strings.Contains(out, "db:5432") {
		t.Fatalf("%d %q %s", code, out, errS)
	}
}

func TestConnectorRunWithKeyUsesSession(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	key := "pd_conn_" + strings.Repeat("a", 48)
	t.Setenv("PILLDROPS_CONNECTOR_KEY", key)
	go authedCLI(t, f.srv, "connector", "run", "prod", "db=10.0.0.5:5432")
	f.waitHello(t)
	if got := f.sessionAuth.Load(); got != "Bearer "+key {
		t.Fatalf("session auth %v", got)
	}
}

func TestSandboxGrantAndCreateWithGrant(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	if code, _, errS := authedCLI(t, f.srv, "sandbox", "grant", "sb1", "prod/db"); code != ExitOK {
		t.Fatalf("grant: %d %s", code, errS)
	}
	if got := f.grantBody.Load(); got != `sb1 {"connector":"prod","target":"db"}` {
		t.Fatalf("grant body %q", got)
	}
	if code, _, _ := authedCLI(t, f.srv, "sandbox", "grant", "sb1", "prod"); code != ExitUsage {
		t.Fatalf("grant without a target: %d", code)
	}
	code, _, errS := authedCLI(t, f.srv, "sandbox", "create", "--grant", "prod/db", "--grant", "prod/api")
	if code != ExitOK {
		t.Fatalf("create: %d %s", code, errS)
	}
	if got, _ := f.createBody.Load().(string); !strings.Contains(got, `"private":[{"connector":"prod","target":"db"},{"connector":"prod","target":"api"}]`) {
		t.Fatalf("create body %s", got)
	}
}

func TestForkKeepPrivateRegrants(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	f.parentGrants = `[{"connector":"prod","target":"db"}]`
	code, _, errS := authedCLI(t, f.srv, "sandbox", "fork", "--keep-private", "sb1")
	if code != ExitOK || f.grantCalls.Load() != 1 {
		t.Fatalf("%d %s grants=%d", code, errS, f.grantCalls.Load())
	}
	if got := f.grantBody.Load(); got != `sb2 {"connector":"prod","target":"db"}` {
		t.Fatalf("grant body %q", got)
	}
}

func TestForkKeepPrivateSaysWhichGrantsFailed(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	f.parentGrants = `[{"connector":"prod","target":"db"},{"connector":"prod","target":"api"}]`
	code, _, errS := authedCLI(t, f.srv, "sandbox", "fork", "--keep-private", "sb1")
	if code != ExitAPI || !strings.Contains(errS, "not copied: prod/api") || strings.Contains(errS, "prod/db") || f.grantCalls.Load() != 2 {
		t.Fatalf("%d %s grants=%d", code, errS, f.grantCalls.Load())
	}
}

func TestConnectorRunIgnoresABadOpenIDAndRefusesOtherProtocols(t *testing.T) {
	good := listenOnce(t)
	f := newFakeConnectorServer(t, true)
	runConnector(t, f, 0, "db=127.0.0.1:"+good.port)
	f.send(t, map[string]any{"type": "open", "id": "../../evil", "proto": "tcp", "target": "db", "port": good.portInt})
	f.nothing(t)
	id := strings.Repeat("e", 32)
	f.send(t, map[string]any{"type": "open", "id": id, "proto": "udp", "target": "db", "port": good.portInt})
	if got := f.failReason(t, id); got != "not announced" {
		t.Fatalf("udp open: %s", got)
	}
}

func TestConnectorRunBoundsOpensAndServesAnIDOnce(t *testing.T) {
	held := listenOnce(t) // the kernel accepts into the backlog, so each dial succeeds and stays open
	f := newFakeConnectorServer(t, true)
	runConnector(t, f, 1, "db=127.0.0.1:"+held.port)
	first := strings.Repeat("a", 32)
	open := map[string]any{"type": "open", "id": first, "proto": "tcp", "target": "db", "port": held.portInt}
	f.send(t, open)
	select {
	case c := <-f.dataConns:
		defer c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("the first open got no data connection")
	}
	f.send(t, open)
	f.nothing(t)
	second := strings.Repeat("b", 32)
	f.send(t, map[string]any{"type": "open", "id": second, "proto": "tcp", "target": "db", "port": held.portInt})
	if got := f.failReason(t, second); got != "busy" {
		t.Fatalf("open past the limit: %s", got)
	}
}

func TestConnectorRunRetriesARateLimitAndStopsOnARefusal(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	f.sessionCode.Store(http.StatusTooManyRequests)
	go authedCLI(t, f.srv, "connector", "run", "prod", "db=10.0.0.5:5432")
	f.waitHello(t)

	f = newFakeConnectorServer(t, true)
	f.sessionCode.Store(http.StatusForbidden)
	code, _, errS := authedCLI(t, f.srv, "connector", "run", "prod", "db=10.0.0.5:5432")
	if code != ExitAPI || !strings.Contains(errS, "the connector has stopped: session refused") {
		t.Fatalf("%d %s", code, errS)
	}
}

func TestConnectorRunStopsPromptlyDuringTheSessionCall(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	f.sessionHang = make(chan struct{})
	defer close(f.sessionHang)
	targets, _ := parseConnectorTargets([]string{"db=10.0.0.5:5432"})
	cs := &connectorSession{cl: &Client{Base: f.srv.URL, Key: "k"}, name: "prod", instance: "inst-0123456789abcdef", targets: targets, stderr: io.Discard}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan string, 1)
	go func() { done <- cs.run(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run kept waiting on the session call")
	}
}

func TestConnectorAdminRefusalSaysWhoMay(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	code, _, errS := authedCLI(t, f.srv, "connector", "create", "prod")
	if code != ExitAPI || !strings.Contains(errS, "owner or admin of this team") || !strings.Contains(errS, "owners and admins") || strings.Contains(errS, "plan/limit") {
		t.Fatalf("%d %s", code, errS)
	}
}

func TestSandboxCreateGrantHintOnlyForTeamRefusals(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	f.createDenied = teamAdminMsg
	code, _, errS := authedCLI(t, f.srv, "sandbox", "create", "--grant", "prod/db")
	if code != ExitAPI || !strings.Contains(errS, "owners and admins") {
		t.Fatalf("team refusal: %d %s", code, errS)
	}
	f.createDenied = "sandbox limit reached"
	code, _, errS = authedCLI(t, f.srv, "sandbox", "create", "--grant", "prod/db")
	if code != ExitAPI || strings.Contains(errS, "owners and admins") || !strings.Contains(errS, "plan/limit") {
		t.Fatalf("plan-limit refusal: %d %s", code, errS)
	}
}

func TestConnectorRunRetriesATunnel503AndStopsOnA410(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	f.tunnelCode.Store(http.StatusServiceUnavailable)
	go authedCLI(t, f.srv, "connector", "run", "prod", "db=10.0.0.5:5432")
	f.waitHello(t)

	f = newFakeConnectorServer(t, true)
	f.tunnelCode.Store(http.StatusGone)
	code, _, errS := authedCLI(t, f.srv, "connector", "run", "prod", "db=10.0.0.5:5432")
	if code != ExitAPI || !strings.Contains(errS, "the connector has stopped: connector revoked") || strings.Contains(errS, "retrying") {
		t.Fatalf("%d %s", code, errS)
	}
}

func TestConnectorRunNamesTheConnectorKeyWhenTheLoginIsRefused(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	f.sessionMsg = teamAdminMsg
	f.sessionCode.Store(http.StatusForbidden)
	code, _, errS := authedCLI(t, f.srv, "connector", "run", "prod", "db=10.0.0.5:5432")
	if code != ExitAPI || !strings.Contains(errS, "owner or admin of this team") || !strings.Contains(errS, "owners and admins") || !strings.Contains(errS, "PILLDROPS_CONNECTOR_KEY") {
		t.Fatalf("%d %s", code, errS)
	}

	// A 403 that is not the team refusal gets no owners-and-admins hint.
	t.Setenv("PILLDROPS_CONNECTOR_KEY", "")
	f = newFakeConnectorServer(t, true)
	f.sessionCode.Store(http.StatusForbidden)
	if _, _, errS := authedCLI(t, f.srv, "connector", "run", "prod", "db=10.0.0.5:5432"); !strings.Contains(errS, "session refused") || strings.Contains(errS, "owners and admins") {
		t.Fatalf("plan-limit 403: %s", errS)
	}

	// A connector key refused for another name gets the message alone.
	t.Setenv("PILLDROPS_CONNECTOR_KEY", "pd_conn_"+strings.Repeat("a", 48))
	f.sessionCode.Store(http.StatusForbidden)
	_, _, errS = authedCLI(t, f.srv, "connector", "run", "prod", "db=10.0.0.5:5432")
	if !strings.Contains(errS, "session refused") || strings.Contains(errS, "hint") {
		t.Fatalf("%s", errS)
	}
}

// A proxy in front of the hub may answer its pings itself, so the hub's
// keepalive messages alone must keep the control socket up.
func TestConnectorRunStaysUpOnKeepalivesAlone(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	targets, err := parseConnectorTargets([]string{"db=127.0.0.1:5432"})
	if err != nil {
		t.Fatal(err)
	}
	cs := &connectorSession{cl: &Client{Base: f.srv.URL, Key: "k"}, name: "prod", instance: "inst-0123456789abcdef",
		targets: targets, stderr: io.Discard, readWait: 300 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go cs.run(ctx)
	f.waitHello(t)
	f.mu.Lock()
	first := f.ctrl
	f.mu.Unlock()
	for range 10 {
		f.send(t, map[string]any{"type": "keepalive"})
		time.Sleep(100 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ctrl != first {
		t.Fatal("the connector reconnected while keepalives were arriving")
	}
}
