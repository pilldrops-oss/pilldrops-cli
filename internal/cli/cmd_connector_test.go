package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/dns/dnsmessage"
)

const teamAdminMsg = "only an owner or admin of this team can change connectors and grants; sign in to the dashboard to do it"

// fakeConnectorServer is the API and the connector hub in one, driven by the test.
type fakeConnectorServer struct {
	srv                                 *httptest.Server
	mu                                  sync.Mutex
	ctrl                                *websocket.Conn
	hello                               string
	fails                               chan string
	dnsAnswers                          chan map[string]any
	dataConns                           chan *websocket.Conn
	sessionAuth                         atomic.Value
	parentGrants                        string
	grantCalls                          atomic.Int32
	grantBody                           atomic.Value
	createBody                          atomic.Value
	sessionCode                         atomic.Int32 // answered once in place of a session
	sessionHang                         chan struct{}
	tunnelCode                          atomic.Int32 // answered once in place of the control socket
	sessionMsg                          string       // the error of a refused session; set before the run starts
	connRefused                         atomic.Bool  // refuse the data sockets
	createDenied                        string       // when set, POST /v1/sandboxes answers 403 with this error
	tagBody, tagQuery, revoked, deleted atomic.Value
}

func newFakeConnectorServer(t *testing.T, private bool) *fakeConnectorServer {
	t.Helper()
	f := &fakeConnectorServer{fails: make(chan string, 8), dnsAnswers: make(chan map[string]any, 8), dataConns: make(chan *websocket.Conn, 8), parentGrants: `[]`}
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
			if msg["type"] == "dns_answer" {
				f.dnsAnswers <- msg
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
	mux.HandleFunc("DELETE /v1/sandboxes/{id}/private/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		f.deleted.Store(r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/private/grants", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.tagBody.Store(string(raw))
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"pg_9","tag":"backend","connector":"prod","kind":"subnet","value":"10.0.5.0/24","sandboxes":2,"unsupported":["sb7"],"shadowed":["10.128.0.0/16"]}`)
	})
	mux.HandleFunc("GET /v1/private/grants", func(w http.ResponseWriter, r *http.Request) {
		f.tagQuery.Store(r.URL.RawQuery)
		_, _ = io.WriteString(w, `{"grants":[{"id":"pg_9","tag":"backend","connector":"prod","kind":"subnet","value":"10.0.5.0/24"}]}`)
	})
	mux.HandleFunc("DELETE /v1/private/grants/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.revoked.Store(r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
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

// open asks the connector for a connection to a named target.
func (f *fakeConnectorServer) open(t *testing.T, target string, port int) string {
	t.Helper()
	id := strings.Repeat("0", 31) + strconv.Itoa(int(openSeq.Add(1))%10)
	f.send(t, map[string]any{"type": "open", "id": id, "proto": "tcp", "target": target, "port": port})
	return f.outcome(t, id)
}

// openDest asks for a connection to dest, as the hub does for a subnet or DNS grant.
func (f *fakeConnectorServer) openDest(t *testing.T, dest, name string) string {
	t.Helper()
	id := strings.Repeat("0", 31) + strconv.Itoa(int(openSeq.Add(1))%10)
	msg := map[string]any{"type": "open", "id": id, "proto": "tcp", "dest": dest}
	if name != "" {
		msg["name"] = name
	}
	f.send(t, msg)
	return f.outcome(t, id)
}

// outcome reports "ok" when a data socket arrives and echoes, "fail" when the
// connector refuses the open first.
func (f *fakeConnectorServer) outcome(t *testing.T, id string) string {
	t.Helper()
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

func (f *fakeConnectorServer) askDNS(t *testing.T, name, qtype string) map[string]any {
	t.Helper()
	id := fmt.Sprintf("%032x", openSeq.Add(1))
	f.send(t, map[string]any{"type": "dns", "id": id, "name": name, "qtype": qtype})
	select {
	case m := <-f.dnsAnswers:
		if m["id"] != id {
			t.Fatalf("answer for another id: %v", m)
		}
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("no dns_answer")
		return nil
	}
}

// fakeDNS answers wiki.corp.internal with a CNAME to web.corp.internal and an
// A record for addr, TTL 60, and anything else with NXDOMAIN.
func fakeDNS(t *testing.T, addr string) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var m dnsmessage.Message
			if m.Unpack(buf[:n]) != nil || len(m.Questions) != 1 {
				continue
			}
			q := m.Questions[0]
			m.Response = true
			if q.Name.String() == "wiki.corp.internal." {
				web := dnsmessage.MustNewName("web.corp.internal.")
				m.Answers = []dnsmessage.Resource{
					{Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.CNAMEResource{CNAME: web}},
					{Header: dnsmessage.ResourceHeader{Name: web, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.AResource{A: netip.MustParseAddr(addr).As4()}},
				}
			} else {
				m.RCode = dnsmessage.RCodeNameError
			}
			out, _ := m.Pack()
			_, _ = pc.WriteTo(out, from)
		}
	}()
	return pc.LocalAddr().String()
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
	runConnectorWith(t, f, func(cs *connectorSession) { cs.maxConns = maxConns }, args...)
}

// runConnectorWith runs a session directly; set adjusts it first.
func runConnectorWith(t *testing.T, f *fakeConnectorServer, set func(*connectorSession), args ...string) {
	t.Helper()
	var targets []connectorTarget
	if len(args) > 0 {
		var err error
		if targets, err = parseConnectorTargets(args); err != nil {
			t.Fatal(err)
		}
	}
	cs := &connectorSession{cl: &Client{Base: f.srv.URL, Key: "k"}, name: "prod", instance: "inst-0123456789abcdef",
		targets: targets, stderr: io.Discard, dialLocalAnswers: true}
	set(cs)
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

func TestConnectorRunAnnouncesSubnetsAndSuffixes(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	go authedCLI(t, f.srv, "connector", "run", "prod", "--subnet", "10.0.0.0/16", "--dns", "Corp.Internal.")
	hello := f.waitHello(t)
	for _, want := range []string{`"subnets":["10.0.0.0/16"]`, `"dns":{"suffixes":["corp.internal"]}`, `"targets":[]`} {
		if !strings.Contains(hello, want) {
			t.Errorf("hello missing %s: %s", want, hello)
		}
	}
}

// The dashboard's run hint puts the flags after a target, and flags may come
// before the name too.
func TestConnectorRunTakesRouteFlagsAnywhere(t *testing.T) {
	for _, args := range [][]string{
		{"prod", "db=10.0.0.5:5432", "--subnet", "10.0.0.0/16", "--dns", "corp.internal"},
		{"--subnet", "10.0.0.0/16", "prod", "--dns", "corp.internal", "db=10.0.0.5:5432"},
		{"prod", "--subnet=10.0.0.0/16", "db=10.0.0.5:5432", "--dns=corp.internal", "--key=k"},
	} {
		f := newFakeConnectorServer(t, true)
		go authedCLI(t, f.srv, append([]string{"connector", "run"}, args...)...)
		hello := f.waitHello(t)
		for _, want := range []string{`"subnets":["10.0.0.0/16"]`, `"dns":{"suffixes":["corp.internal"]}`, `"targets":[{"name":"db","port":5432}]`} {
			if !strings.Contains(hello, want) {
				t.Errorf("%v: hello missing %s: %s", args, want, hello)
			}
		}
		if args[len(args)-1] == "--key=k" && f.sessionAuth.Load() != "Bearer k" {
			t.Errorf("%v: session auth %v", args, f.sessionAuth.Load())
		}
	}
}

func TestConnectorRunRejectsBadRoutes(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	for _, args := range [][]string{{"--subnet", "0.0.0.0/0"}, {"--subnet", "10.0.5.1/24"}, {"--subnet", "fd00::/8"},
		{"--subnet", "10.0.0.0/7"}, {"--subnet=10.0.5.1/24"}, {"--dns", "internal"}, {"--dns", "x.pd"}, {"--dns", "10.0.0.1"}, {"--subnet"}, {"--dns"}} {
		if code, _, errS := authedCLI(t, f.srv, append([]string{"connector", "run", "prod"}, args...)...); code != ExitUsage {
			t.Errorf("%v: code %d %s", args, code, errS)
		}
	}
}

// Whatever the server sends, a dest outside the connector's own --subnet ranges is never dialled.
func TestConnectorDialsADestOnlyInsideItsOwnSubnets(t *testing.T) {
	good := listenOnce(t)
	echoOnce(good.l)
	f := newFakeConnectorServer(t, true)
	runConnectorWith(t, f, func(cs *connectorSession) { cs.subnets = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")} })
	if got := f.openDest(t, "127.0.0.1:"+good.port, ""); got != "ok" {
		t.Fatalf("inside its subnet: %s", got)
	}
	for _, dest := range []string{"10.9.9.9:" + good.port, "127.0.0.1:0", "[::1]:" + good.port, "[::ffff:127.0.0.1]:" + good.port, "nonsense"} {
		if got := f.openDest(t, dest, ""); got != "fail" {
			t.Errorf("%s: %s", dest, got)
		}
	}
}

func TestConnectorAnswersDNSAndDialsWhatItAnswered(t *testing.T) {
	good := listenOnce(t)
	echoOnce(good.l)
	f := newFakeConnectorServer(t, true)
	runConnectorWith(t, f, func(cs *connectorSession) {
		cs.suffixes, cs.dnsServers = []string{"corp.internal"}, []string{fakeDNS(t, "127.0.0.1")}
	})
	m := f.askDNS(t, "wiki.corp.internal", "A")
	answers, _ := m["answers"].([]any)
	if m["rcode"] != float64(0) || len(answers) != 2 ||
		answers[0].(map[string]any)["type"] != "CNAME" || answers[0].(map[string]any)["value"] != "web.corp.internal" ||
		answers[1].(map[string]any)["value"] != "127.0.0.1" || answers[1].(map[string]any)["ttl"] != float64(60) {
		t.Fatalf("answer %v", m)
	}
	if got := f.openDest(t, "127.0.0.1:"+good.port, ""); got != "ok" {
		t.Fatalf("an address it answered: %s", got)
	}
	for _, c := range []struct {
		name, qtype string
		rcode       float64
	}{
		{"evil.example", "A", 5}, {"wiki.corp.internal", "AAAA", 0}, {"wiki.corp.internal", "MX", 4}, {"nope.corp.internal", "A", 3},
	} {
		if m := f.askDNS(t, c.name, c.qtype); m["rcode"] != c.rcode {
			t.Errorf("%s %s: %v, want rcode %v", c.name, c.qtype, m, c.rcode)
		}
	}
}

// Another instance answered the lookup: the open carries the name, and this
// one resolves it itself before dialling.
func TestConnectorResolvesANameItDidNotAnswer(t *testing.T) {
	good := listenOnce(t)
	echoOnce(good.l)
	f := newFakeConnectorServer(t, true)
	runConnectorWith(t, f, func(cs *connectorSession) {
		cs.suffixes, cs.dnsServers = []string{"corp.internal"}, []string{fakeDNS(t, "127.0.0.1")}
	})
	if got := f.openDest(t, "127.0.0.1:"+good.port, "evil.example"); got != "fail" {
		t.Fatalf("a name outside its suffixes: %s", got)
	}
	if got := f.openDest(t, "127.0.0.1:"+good.port, "wiki.corp.internal"); got != "ok" {
		t.Fatalf("a name it resolves to the dest: %s", got)
	}
}

func TestConnectorRefusesAnExpiredAnswerUnlessItResolvesAgain(t *testing.T) {
	good := listenOnce(t)
	echoOnce(good.l)
	f := newFakeConnectorServer(t, true)
	runConnectorWith(t, f, func(cs *connectorSession) {
		cs.suffixes, cs.dnsServers = []string{"corp.internal"}, []string{fakeDNS(t, "127.0.0.1")}
		cs.answered = map[netip.Addr]time.Time{netip.MustParseAddr("127.0.0.1"): time.Now().Add(-time.Second)}
	})
	if got := f.openDest(t, "127.0.0.1:"+good.port, ""); got != "fail" {
		t.Fatalf("an expired answer: %s", got)
	}
	if got := f.openDest(t, "127.0.0.1:"+good.port, "wiki.corp.internal"); got != "ok" {
		t.Fatalf("an expired answer with a name that resolves to it: %s", got)
	}
}

func TestConnectorRefusesADestItsNameDoesNotResolveTo(t *testing.T) {
	good := listenOnce(t)
	f := newFakeConnectorServer(t, true)
	runConnectorWith(t, f, func(cs *connectorSession) {
		cs.suffixes, cs.dnsServers = []string{"corp.internal"}, []string{fakeDNS(t, "127.0.0.2")}
	})
	if got := f.openDest(t, "127.0.0.1:"+good.port, "wiki.corp.internal"); got != "fail" {
		t.Fatalf("a name that resolves elsewhere: %s", got)
	}
}

// A resolver that never answers must not hold up other opens or DNS answers.
func TestConnectorSlowResolverDoesNotStallTheControlChannel(t *testing.T) {
	silent, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })
	good := listenOnce(t)
	echoOnce(good.l)
	f := newFakeConnectorServer(t, true)
	runConnectorWith(t, f, func(cs *connectorSession) {
		cs.subnets = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
		cs.suffixes, cs.dnsServers = []string{"corp.internal"}, []string{silent.LocalAddr().String()}
	})
	stuck := strings.Repeat("c", 32)
	f.send(t, map[string]any{"type": "open", "id": stuck, "proto": "tcp", "dest": "10.1.1.1:80", "name": "wiki.corp.internal"})
	start := time.Now()
	if m := f.askDNS(t, "evil.example", "A"); m["rcode"] != float64(5) {
		t.Fatalf("dns answer %v", m)
	}
	if got := f.openDest(t, "127.0.0.1:"+good.port, ""); got != "ok" {
		t.Fatalf("second open: %s", got)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the control channel waited %s on the resolver", took)
	}
	select {
	case got := <-f.fails:
		if got != stuck+" not announced" {
			t.Fatalf("the unresolved open: %s", got)
		}
	case <-time.After(4 * time.Second): // past the resolver's 2.5 s deadline
		t.Fatal("the unresolved open was never failed")
	}
}

func TestConnectorKeepsAtMostTheAnsweredLimit(t *testing.T) {
	cs := &connectorSession{}
	first := netip.MustParseAddr("10.0.0.0")
	for i, a := 0, first; i <= connectorMaxAnswered; i, a = i+1, a.Next() {
		cs.remember(a, 60)
	}
	if len(cs.answered) != connectorMaxAnswered || len(cs.answerOrder) != connectorMaxAnswered || cs.answeredRecently(first) {
		t.Fatalf("kept %d (%d ordered), oldest kept %v", len(cs.answered), len(cs.answerOrder), cs.answeredRecently(first))
	}
}

func TestSandboxGrantSubnetAndDNS(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	for arg, want := range map[string]string{
		"prod/subnet:10.0.5.0/24": `sb1 {"connector":"prod","subnet":"10.0.5.0/24"}`,
		"prod/dns:corp.internal":  `sb1 {"connector":"prod","dns":"corp.internal"}`,
		"prod/db":                 `sb1 {"connector":"prod","target":"db"}`,
	} {
		if code, _, errS := authedCLI(t, f.srv, "sandbox", "grant", "sb1", arg); code != ExitOK {
			t.Fatalf("%s: %d %s", arg, code, errS)
		}
		if got := f.grantBody.Load(); got != want {
			t.Errorf("%s: body %q", arg, got)
		}
	}
	for _, bad := range []string{"prod/subnet:", "prod/dns:", "/db", "prod"} {
		if code, _, _ := authedCLI(t, f.srv, "sandbox", "grant", "sb1", bad); code != ExitUsage {
			t.Errorf("%s: %d", bad, code)
		}
	}
}

func TestSandboxRevokeFindsItsOwnGrant(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	f.parentGrants = `[{"id":"pg_1","connector":"prod","kind":"subnet","value":"10.0.5.0/24","source":"sandbox"},` +
		`{"id":"pg_2","connector":"prod","kind":"dns","value":"corp.internal","source":"tag:backend"}]`
	if code, _, errS := authedCLI(t, f.srv, "sandbox", "revoke", "sb1", "prod/subnet:10.0.5.0/24"); code != ExitOK || f.deleted.Load() != "/v1/sandboxes/sb1/private/pg_1" {
		t.Fatalf("subnet: %d %s %v", code, errS, f.deleted.Load())
	}
	if code, _, errS := authedCLI(t, f.srv, "sandbox", "revoke", "sb1", "prod/subnet:10.0.5.9/24"); code != ExitOK || f.deleted.Load() != "/v1/sandboxes/sb1/private/pg_1" {
		t.Fatalf("non-canonical subnet: %d %s %v", code, errS, f.deleted.Load())
	}
	if code, _, errS := authedCLI(t, f.srv, "sandbox", "revoke", "sb1", "prod/dns:corp.internal"); code == ExitOK || !strings.Contains(errS, "private revoke") {
		t.Fatalf("a tag's grant: %d %s", code, errS)
	}
	if code, _, _ := authedCLI(t, f.srv, "sandbox", "revoke", "sb1", "prod/db"); code != ExitOK || f.deleted.Load() != "/v1/sandboxes/sb1/private/prod/db" {
		t.Fatalf("target: %d %v", code, f.deleted.Load())
	}
}

func TestForkKeepPrivateCopiesOwnGrantsOnly(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	f.parentGrants = `[{"connector":"prod","target":"db","kind":"target","value":"db","source":"sandbox"},` +
		`{"connector":"prod","kind":"subnet","value":"10.0.5.0/24","source":"sandbox"},` +
		`{"connector":"prod","kind":"dns","value":"corp.internal","source":"tag:backend"}]`
	code, _, errS := authedCLI(t, f.srv, "sandbox", "fork", "--keep-private", "sb1")
	if code != ExitOK || f.grantCalls.Load() != 2 {
		t.Fatalf("%d %s grants=%d", code, errS, f.grantCalls.Load())
	}
	if got := f.grantBody.Load(); got != `sb2 {"connector":"prod","subnet":"10.0.5.0/24"}` {
		t.Fatalf("last grant body %q", got)
	}
}

func TestConnectorRunExplainsTheHostPortPlaceholder(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	code, _, errS := authedCLI(t, f.srv, "connector", "run", "prod", "db=HOST:PORT", "--subnet", "10.0.0.0/16")
	if code != ExitUsage || !strings.Contains(errS, "replace HOST:PORT with the target's address") {
		t.Fatalf("%d %s", code, errS)
	}
}

// A record under a suffix pointing at loopback or link-local (instance
// metadata) is neither answered nor dialled.
func TestConnectorNeverDialsLoopbackOrLinkLocalAnswers(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "169.254.169.254"} {
		good := listenOnce(t)
		f := newFakeConnectorServer(t, true)
		runConnectorWith(t, f, func(cs *connectorSession) {
			cs.suffixes, cs.dnsServers, cs.dialLocalAnswers = []string{"corp.internal"}, []string{fakeDNS(t, addr)}, false
		})
		m := f.askDNS(t, "wiki.corp.internal", "A")
		answers, _ := m["answers"].([]any)
		if m["rcode"] != float64(0) || len(answers) != 1 || answers[0].(map[string]any)["type"] != "CNAME" {
			t.Errorf("%s: answer %v", addr, m)
		}
		for _, name := range []string{"", "wiki.corp.internal"} {
			if got := f.openDest(t, addr+":"+good.port, name); got != "fail" {
				t.Errorf("%s with name %q: %s", addr, name, got)
			}
		}
	}
}

// dnsServerFunc answers every query on one UDP and TCP port with reply(query, overTCP).
func dnsServerFunc(t *testing.T, reply func(q dnsmessage.Message, tcp bool) dnsmessage.Message) string {
	t.Helper()
	var pc net.PacketConn
	var ln net.Listener
	for i := 0; ln == nil; i++ {
		var err error
		if pc, err = net.ListenPacket("udp", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		if ln, err = net.Listen("tcp", pc.LocalAddr().String()); err != nil {
			_ = pc.Close()
			if i == 20 {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() { _ = pc.Close(); _ = ln.Close() })
	respond := func(raw []byte, tcp bool) []byte {
		var m dnsmessage.Message
		if m.Unpack(raw) != nil {
			return nil
		}
		r := reply(m, tcp)
		r.ID, r.Response, r.Questions = m.ID, true, m.Questions
		out, _ := r.Pack()
		return out
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if out := respond(buf[:n], false); out != nil {
				_, _ = pc.WriteTo(out, from)
			}
		}
	}()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				var n [2]byte
				if _, err := io.ReadFull(conn, n[:]); err != nil {
					return
				}
				raw := make([]byte, int(n[0])<<8|int(n[1]))
				if _, err := io.ReadFull(conn, raw); err != nil {
					return
				}
				if out := respond(raw, true); out != nil {
					_, _ = conn.Write(append([]byte{byte(len(out) >> 8), byte(len(out))}, out...))
				}
			}()
		}
	}()
	return pc.LocalAddr().String()
}

func aRecord(name, ip string) dnsmessage.Resource {
	return dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
		Body: &dnsmessage.AResource{A: netip.MustParseAddr(ip).As4()}}
}

func TestConnectorResolveRetriesATruncatedAnswerOverTCP(t *testing.T) {
	var tcpQueries atomic.Int32
	server := dnsServerFunc(t, func(q dnsmessage.Message, tcp bool) dnsmessage.Message {
		if !tcp {
			return dnsmessage.Message{Header: dnsmessage.Header{Truncated: true}}
		}
		tcpQueries.Add(1)
		return dnsmessage.Message{Answers: []dnsmessage.Resource{aRecord("wiki.corp.internal.", "10.0.0.7")}}
	})
	cs := &connectorSession{dnsServers: []string{server}}
	answers, rcode := cs.resolve(context.Background(), "wiki.corp.internal", "A")
	if rcode != dnsmessage.RCodeSuccess || len(answers) != 1 || answers[0].Value != "10.0.0.7" || tcpQueries.Load() != 1 {
		t.Fatalf("%v %v tcp=%d", answers, rcode, tcpQueries.Load())
	}
}

func TestConnectorResolveTriesNameserversInOrder(t *testing.T) {
	silent, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })
	fixed := func(rcode dnsmessage.RCode, rrs ...dnsmessage.Resource) string {
		return dnsServerFunc(t, func(dnsmessage.Message, bool) dnsmessage.Message {
			return dnsmessage.Message{Header: dnsmessage.Header{RCode: rcode}, Answers: rrs}
		})
	}
	good := fixed(dnsmessage.RCodeSuccess, aRecord("wiki.corp.internal.", "10.0.0.7"))
	nxdomain := fixed(dnsmessage.RCodeNameError)
	for _, tc := range []struct {
		name    string
		servers []string
		rcode   dnsmessage.RCode
		answers int
	}{
		{"timeout then good", []string{silent.LocalAddr().String(), good}, dnsmessage.RCodeSuccess, 1},
		{"servfail then good", []string{fixed(dnsmessage.RCodeServerFailure), good}, dnsmessage.RCodeSuccess, 1},
		{"refused then good", []string{fixed(dnsmessage.RCodeRefused), good}, dnsmessage.RCodeSuccess, 1},
		{"nxdomain ends it", []string{nxdomain, good}, dnsmessage.RCodeNameError, 0},
		{"all refuse", []string{fixed(dnsmessage.RCodeRefused), fixed(dnsmessage.RCodeRefused)}, dnsmessage.RCodeRefused, 0},
	} {
		cs := &connectorSession{dnsServers: tc.servers}
		answers, rcode := cs.resolve(context.Background(), "wiki.corp.internal", "A")
		if rcode != tc.rcode || len(answers) != tc.answers {
			t.Errorf("%s: %v %v", tc.name, answers, rcode)
		}
	}
}

func TestSystemNameserversListsThreeInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte("search x\nnameserver 10.0.0.1\nnameserver fe80::1%eth0\nnameserver 10.0.0.3\nnameserver 10.0.0.4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := resolvConfPath
	resolvConfPath = path
	t.Cleanup(func() { resolvConfPath = old })
	got := strings.Join(systemNameservers(), " ")
	if got != "10.0.0.1:53 [fe80::1]:53 10.0.0.3:53" {
		t.Fatalf("%q", got)
	}
}

func TestConnectorResolveTrimsAnswersToTheHubsReplyLimit(t *testing.T) {
	server := dnsServerFunc(t, func(dnsmessage.Message, bool) dnsmessage.Message {
		var rrs []dnsmessage.Resource
		for i := 0; i < 4; i++ {
			target := dnsmessage.MustNewName(strings.Repeat("a", 60) + strconv.Itoa(i) + ".corp.internal.")
			rrs = append(rrs, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName("wiki.corp.internal."), Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 60},
				Body: &dnsmessage.CNAMEResource{CNAME: target}})
		}
		for i := 0; i < 30; i++ {
			rrs = append(rrs, aRecord("wiki.corp.internal.", "10.0.1."+strconv.Itoa(i+1)))
		}
		return dnsmessage.Message{Answers: rrs}
	})
	cs := &connectorSession{dnsServers: []string{server}}
	answers, rcode := cs.resolve(context.Background(), "wiki.corp.internal", "A")
	reply, _ := json.Marshal(map[string]any{"type": "dns_answer", "id": "dns-1", "rcode": int(rcode), "answers": answers})
	if len(answers) == 0 || len(answers) >= 34 || len(reply) > connectorMaxReplyBytes {
		t.Fatalf("%d answers, reply %d bytes", len(answers), len(reply))
	}
}

func TestConnectorRunWarnsWhenDNSHasNoNameserver(t *testing.T) {
	old := resolvConfPath
	resolvConfPath = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { resolvConfPath = old })
	f := newFakeConnectorServer(t, false)
	_, _, errS := authedCLI(t, f.srv, "connector", "run", "prod", "--dns", "corp.internal")
	if !strings.Contains(errS, "no nameserver found") {
		t.Fatalf("stderr %q", errS)
	}
	_, _, errS = authedCLI(t, f.srv, "connector", "run", "prod", "--subnet", "10.0.0.0/16")
	if strings.Contains(errS, "no nameserver found") {
		t.Fatalf("stderr %q", errS)
	}
}

func TestConnectorRunRefusesDuplicateAndReservedRoutes(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	longSuffix := strings.Repeat(strings.Repeat("a", 50)+".", 5) + "com"
	for _, args := range [][]string{
		{"--subnet", "10.0.0.0/16", "--subnet", "10.0.0.0/16"}, {"--dns", "corp.internal", "--dns", "Corp.Internal."},
		{"--dns", longSuffix}, {"--dns", strings.Repeat("a", 64) + ".com"},
		{"--subnet", "10.128.0.0/16"}, {"--subnet", "10.128.4.0/24"}, {"--subnet", "127.0.0.0/8"}, {"--subnet", "100.64.0.0/10"},
		{"--subnet", "192.168.30.0/24"}, {"--subnet", "169.254.169.254/32"},
	} {
		if code, _, errS := authedCLI(t, f.srv, append([]string{"connector", "run", "prod"}, args...)...); code != ExitUsage || errS == "" {
			t.Errorf("%v: code %d %s", args, code, errS)
		}
	}
	for _, ok := range []string{"10.0.0.0/8", "192.168.0.0/16", "100.0.0.0/8"} {
		if _, err := parseConnectorSubnets([]string{ok}); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}

func TestPrivateGrantTakesFlagsAnywhere(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	for _, args := range [][]string{
		{"private", "grant", "prod/subnet:10.0.5.0/24", "--tag", "backend"},
		{"private", "grant", "prod/subnet:10.0.5.0/24", "--tag=backend"},
	} {
		if code, _, errS := authedCLI(t, f.srv, args...); code != ExitOK {
			t.Fatalf("%v: %d %s", args, code, errS)
		}
		if got := f.tagBody.Load(); got != `{"connector":"prod","subnet":"10.0.5.0/24","tag":"backend"}` {
			t.Fatalf("%v: body %q", args, got)
		}
	}
	if code, _, _ := authedCLI(t, f.srv, "private", "grants", "--tag", "backend", "extra"); code != ExitUsage {
		t.Errorf("grants with a positional: %d", code)
	}
}

func TestPrivateGrantsOnAServerWithoutThemSaysSo(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	old := http.NewServeMux()
	old.Handle("/v1/capabilities", f.srv.Config.Handler)
	old.HandleFunc("/v1/private/grants", http.NotFound)
	srv := httptest.NewServer(old)
	t.Cleanup(srv.Close)
	for _, args := range [][]string{{"private", "grant", "--tag", "b", "prod/db"}, {"private", "grants"}, {"--json", "private", "grants"}} {
		if code, _, errS := authedCLI(t, srv, args...); code == ExitOK || !strings.Contains(errS, "predates tag grants") {
			t.Errorf("%v: %d %q", args, code, errS)
		}
	}
}
