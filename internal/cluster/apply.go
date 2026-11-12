package cluster

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

var (
	//go:embed control.sh
	controlScript string
	//go:embed node.sh
	nodeScript string

	scripts = template.Must(template.New("control").Funcs(template.FuncMap{"sq": shellQuote}).Parse(controlScript))
	_       = template.Must(scripts.New("node").Parse(nodeScript))
)

// Waits, as vars so a test can shorten them.
var (
	healthWait = 20 * time.Minute // includes the operator adding DNS records
	nodeWait   = 5 * time.Minute
	pollEvery  = 5 * time.Second
)

// Apply brings the fleet described by the cluster.toml at path to life, or
// back in line with it. Every step checks before it acts, so it can run again.
//
// A joined node on another release is upgraded in place. With prune, a node
// the control plane knows that cluster.toml no longer lists is drained and
// removed; without it, apply only names such nodes.
func Apply(ctx context.Context, path string, out io.Writer, prune bool) error {
	cfg, err := LoadConfig(path)
	if err != nil {
		return err
	}
	secretsPath := filepath.Join(filepath.Dir(path), "cluster.secrets")
	sec, created, err := LoadSecrets(secretsPath)
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(out, "── wrote new secrets to %s; back it up, a fleet cannot be recovered without it\n", secretsPath)
	}
	a := &applier{cfg: cfg, sec: sec, out: out, ssh: sshRun}

	fmt.Fprintln(out, "── checking hosts")
	if err := a.checkHosts(ctx); err != nil {
		return err
	}
	fmt.Fprintf(out, "── checking the signature of %s\n", cfg.Version)
	sums, err := fetchSums(cfg.Version)
	if err != nil {
		return err
	}
	a.sums = sums

	fmt.Fprintf(out, "── control host %s\n", cfg.Control.Host)
	script, err := a.controlScript()
	if err != nil {
		return err
	}
	if err := a.ssh(ctx, cfg.Target(cfg.Control), script, out); err != nil {
		return fmt.Errorf("control host %s: %w", cfg.Control.Host, err)
	}

	fmt.Fprintf(out, "── waiting for https://%s\n   point these DNS records at %s if they do not already:\n      %s\n      *.%s\n",
		cfg.Domain, cfg.Control.Host[strings.LastIndex(cfg.Control.Host, "@")+1:], cfg.Domain, cfg.Domain)
	if err := a.waitHealthy(ctx); err != nil {
		return err
	}

	registered, err := a.waitRegistered(ctx)
	if err != nil {
		return err
	}
	for _, n := range cfg.Nodes {
		var err error
		switch view, ok := registered[advertiseURL(n)]; {
		case !n.joined:
			fmt.Fprintf(out, "── node %s (%s)\n", n.Host.Host, n.Runtime)
			err = a.joinNode(ctx, n)
		case ok && view.Version != a.nodeVersion():
			fmt.Fprintf(out, "── node %s: upgrading %s to %s\n", n.Host.Host, view.Version, a.nodeVersion())
			err = a.upgradeNode(ctx, n, view)
		default:
			fmt.Fprintf(out, "── node %s already joined; checking its firewall and domain\n", n.Host.Host)
			err = a.runNodeScript(ctx, n, "", false)
		}
		if err != nil {
			return fmt.Errorf("node %s: %w", n.Host.Host, err)
		}
	}
	if err := a.removeUnlisted(ctx, registered, prune); err != nil {
		return err
	}

	fmt.Fprintln(out, "── checking every node reports healthy")
	if err := a.waitNodes(ctx); err != nil {
		return err
	}
	fmt.Fprintf(out, `
✓ The fleet is running: %d node(s).

  Dashboard   https://%s/   sign in as %s with ADMIN_PASSWORD
  Operator    ORCHESTRATOR_API_KEY, for the API and CLI
  Both are in %s
`, len(cfg.Nodes), cfg.Domain, cfg.AdminEmail, secretsPath)
	return nil
}

type applier struct {
	cfg  *Config
	sec  Secrets
	sums []byte
	out  io.Writer
	ssh  func(ctx context.Context, target, script string, out io.Writer) error
	http *http.Client
}

// sshRun pipes script into a root shell on target. The script, secrets
// included, travels on stdin, so it never appears in a process list.
func sshRun(ctx context.Context, target, script string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", target, "sudo", "bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

// checkHosts reaches every host before anything is changed, and resolves each
// node's runtime "auto" to what the host can run.
func (a *applier) checkHosts(ctx context.Context) error {
	if err := a.ssh(ctx, a.cfg.Target(a.cfg.Control), "grep -q 'ID=ubuntu' /etc/os-release", io.Discard); err != nil {
		return fmt.Errorf("control host %s: ssh with sudo to an Ubuntu host failed: %w", a.cfg.Control.Host, err)
	}
	for i := range a.cfg.Nodes {
		n := &a.cfg.Nodes[i]
		// Joined is read from the host, not the control plane: a control plane
		// without Redis forgets its nodes on a restart until they heartbeat.
		probe := "grep -q 'ID=ubuntu' /etc/os-release && echo \"pilldrops-probe" +
			" kvm=$([ -e /dev/kvm ] && echo y || echo n)" +
			" joined=$(grep -qx 'ORCHESTRATOR_URL=https://" + a.cfg.Domain + "' /etc/sandboxd.env 2>/dev/null && echo y || echo n)\""
		var buf bytes.Buffer
		if err := a.ssh(ctx, a.cfg.Target(n.Host), probe, &buf); err != nil {
			return fmt.Errorf("node %s: ssh with sudo to an Ubuntu host failed: %w", n.Host.Host, err)
		}
		// sudo may warn on stderr first, such as about an unresolvable
		// hostname, and both streams arrive in one buffer.
		var hasKVM bool
		for _, line := range strings.Split(buf.String(), "\n") {
			if f := strings.Fields(line); len(f) == 3 && f[0] == "pilldrops-probe" {
				hasKVM, n.joined = f[1] == "kvm=y", f[2] == "joined=y"
			}
		}
		switch {
		case n.Runtime == "firecracker" && !hasKVM:
			return fmt.Errorf("node %s: runtime firecracker needs /dev/kvm, which this host does not have", n.Host.Host)
		case n.Runtime == "auto" && hasKVM:
			n.Runtime = "firecracker"
		case n.Runtime == "auto":
			n.Runtime = "docker"
		}
	}
	return nil
}

func (a *applier) base() map[string]any {
	return map[string]any{
		"Tag":         a.cfg.Version,
		"Domain":      a.cfg.Domain,
		"Zone":        a.cfg.Zone,
		"Secrets":     a.sec,
		"SumsB64":     base64.StdEncoding.EncodeToString(a.sums),
		"ReleaseBase": releaseBase + a.cfg.Version + "/",
	}
}

func (a *applier) controlScript() (string, error) {
	d := a.base()
	d["Control"] = a.cfg.Control
	d["Nodes"] = a.cfg.Nodes
	d["TLS"] = a.cfg.TLS
	d["AdminEmail"] = a.cfg.AdminEmail
	if a.cfg.TLS.Mode == "cloudflare" {
		d["CloudflareToken"] = os.Getenv(a.cfg.TLS.TokenEnv)
	} else {
		for key, file := range map[string]string{"CertB64": a.cfg.TLS.CertFile, "KeyB64": a.cfg.TLS.KeyFile} {
			b, err := os.ReadFile(file)
			if err != nil {
				return "", err
			}
			d[key] = base64.StdEncoding.EncodeToString(b)
		}
	}
	return render("control", d)
}

func (a *applier) nodeScript(n Node, token string, upgrade bool) (string, error) {
	d := a.base()
	d["Upgrade"] = upgrade
	d["Runtime"] = n.Runtime
	d["ControlIP"] = a.cfg.Control.PrivateIP
	d["Token"] = token
	d["OrchestratorURL"] = "https://" + a.cfg.Domain
	d["AdvertiseURL"] = advertiseURL(n)
	return render("node", d)
}

func render(name string, data any) (string, error) {
	var b strings.Builder
	if err := scripts.ExecuteTemplate(&b, name, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// advertiseURL is how the control plane reaches a node, and how apply
// recognises one that has already joined.
func advertiseURL(n Node) string { return "http://" + n.PrivateIP + ":18080" }

func (a *applier) joinNode(ctx context.Context, n Node) error {
	var tok struct {
		Token string `json:"token"`
	}
	if err := a.api(ctx, "POST", "/v1/join-tokens", &tok); err != nil {
		return fmt.Errorf("mint a join token: %w", err)
	}
	return a.runNodeScript(ctx, n, tok.Token, false)
}

func (a *applier) runNodeScript(ctx context.Context, n Node, token string, upgrade bool) error {
	script, err := a.nodeScript(n, token, upgrade)
	if err != nil {
		return err
	}
	return a.ssh(ctx, a.cfg.Target(n.Host), script, a.out)
}

// nodeVersion is how a node on cfg.Version reports itself in /v1/nodes.
func (a *applier) nodeVersion() string { return strings.TrimPrefix(a.cfg.Version, "sandboxd/") }

// upgradeNode swaps a node's binaries while it is cordoned, so nothing new is
// placed on it during the restart. Its sandboxes are paused by the restart
// and resumed after it, on the same node. A node the operator cordoned or is
// draining stays that way.
func (a *applier) upgradeNode(ctx context.Context, n Node, view nodeView) error {
	ours := view.Status != "cordoned" && view.Status != "draining"
	if ours {
		if err := a.api(ctx, "POST", "/v1/nodes/"+view.ID+"/cordon", nil); err != nil {
			return fmt.Errorf("cordon: %w", err)
		}
	}
	if err := a.runNodeScript(ctx, n, "", true); err != nil {
		return fmt.Errorf("%w; it is still cordoned: uncordon it with POST /v1/nodes/%s/uncordon once fixed", err, view.ID)
	}
	want := a.nodeVersion()
	err := poll(ctx, nodeWait, func() (bool, error) {
		got, err := a.nodes(ctx)
		v := got[advertiseURL(n)]
		return err == nil && v.Healthy && v.Version == want, nil
	}, func() error {
		return fmt.Errorf("did not come back healthy on %s within %v; check `journalctl -u sandboxd`", want, nodeWait)
	})
	if err != nil || !ours {
		return err
	}
	return a.api(ctx, "POST", "/v1/nodes/"+view.ID+"/uncordon", nil)
}

// waitRegistered lists the fleet once every node apply found joined has
// heartbeated: a control plane without Redis starts with an empty list.
func (a *applier) waitRegistered(ctx context.Context) (map[string]nodeView, error) {
	var got map[string]nodeView
	err := poll(ctx, nodeWait, func() (bool, error) {
		var err error
		if got, err = a.nodes(ctx); err != nil {
			return false, nil
		}
		for _, n := range a.cfg.Nodes {
			if _, ok := got[advertiseURL(n)]; n.joined && !ok {
				return false, nil
			}
		}
		return true, nil
	}, func() error {
		return fmt.Errorf("the control plane has not heard from every joined node within %v; check `journalctl -u sandboxd` on them", nodeWait)
	})
	return got, err
}

// removeUnlisted drains and deletes registered nodes cluster.toml no longer
// lists. A drain moves each sandbox to another node; if any could not move,
// the node is left in the fleet rather than deleted with sandboxes on it.
func (a *applier) removeUnlisted(ctx context.Context, registered map[string]nodeView, prune bool) error {
	listed := map[string]bool{}
	for _, n := range a.cfg.Nodes {
		listed[advertiseURL(n)] = true
	}
	for url, view := range registered {
		if listed[url] {
			continue
		}
		if !prune {
			fmt.Fprintf(a.out, "── node %s (%s) is in the fleet but not in cluster.toml; apply --prune drains and removes it\n", view.ID, url)
			continue
		}
		fmt.Fprintf(a.out, "── node %s (%s): draining, then removing\n", view.ID, url)
		var drain struct {
			Remaining int `json:"sandboxes_remaining"`
		}
		if err := a.api(ctx, "POST", "/v1/nodes/"+view.ID+"/drain?migrate=1", &drain); err != nil {
			return fmt.Errorf("drain node %s: %w", view.ID, err)
		}
		if drain.Remaining > 0 {
			return fmt.Errorf("node %s still has %d sandbox(es) that could not move to another node; it is draining and was not removed", view.ID, drain.Remaining)
		}
		if err := a.api(ctx, "DELETE", "/v1/nodes/"+view.ID, nil); err != nil {
			return fmt.Errorf("remove node %s: %w", view.ID, err)
		}
		fmt.Fprintf(a.out, "   removed; %s still runs sandboxd, stop it there\n", url)
	}
	return nil
}

func (a *applier) waitHealthy(ctx context.Context) error {
	url := "https://" + a.cfg.Domain + "/health"
	return poll(ctx, healthWait, func() (bool, error) {
		hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(hctx, "GET", url, nil)
		if err != nil {
			return false, err
		}
		resp, err := a.client().Do(req)
		if err != nil {
			return false, nil
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK, nil
	}, func() error {
		return fmt.Errorf("%s did not answer within %v; check the DNS records above and `journalctl -u caddy -u pilldrops-orchestrator` on the control host", url, healthWait)
	})
}

type nodeView struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Healthy bool   `json:"healthy"`
	Status  string `json:"status"`
	Version string `json:"version"`
}

func (a *applier) nodes(ctx context.Context) (map[string]nodeView, error) {
	var page struct {
		Nodes []nodeView `json:"nodes"`
	}
	if err := a.api(ctx, "GET", "/v1/nodes", &page); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	m := map[string]nodeView{}
	for _, n := range page.Nodes {
		m[n.URL] = n
	}
	return m, nil
}

func (a *applier) waitNodes(ctx context.Context) error {
	var unhealthy []string
	return poll(ctx, nodeWait, func() (bool, error) {
		got, err := a.nodes(ctx)
		if err != nil {
			return false, nil // a node restarting can briefly fail the listing
		}
		unhealthy = unhealthy[:0]
		for _, n := range a.cfg.Nodes {
			if !got[advertiseURL(n)].Healthy {
				unhealthy = append(unhealthy, n.Host.Host)
			}
		}
		return len(unhealthy) == 0, nil
	}, func() error {
		return fmt.Errorf("not healthy after %v: %s; check `journalctl -u sandboxd` on each", nodeWait, strings.Join(unhealthy, ", "))
	})
}

func (a *applier) api(ctx context.Context, method, path string, into any) error {
	// A drain moves every sandbox before it answers.
	limit := 30 * time.Second
	if strings.Contains(path, "/drain") {
		limit = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "https://"+a.cfg.Domain+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.sec["ORCHESTRATOR_API_KEY"])
	resp, err := a.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(body)))
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(body, into)
}

func (a *applier) client() *http.Client {
	if a.http != nil {
		return a.http
	}
	return &http.Client{}
}

func poll(ctx context.Context, limit time.Duration, done func() (bool, error), timeout func() error) error {
	deadline := time.Now().Add(limit)
	for {
		ok, err := done()
		if err != nil || ok {
			return err
		}
		if time.Now().After(deadline) {
			return timeout()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}

// shellQuote makes s one single-quoted shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
