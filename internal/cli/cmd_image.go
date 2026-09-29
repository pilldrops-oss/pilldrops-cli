package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// cmdImage handles `pilldrops image build` — build a custom image from a local
// context (Dockerfile at its root) so you can launch it with
// `pilldrops sandbox create --image <tag>`. The build runs on the fleet's nodes.
func cmdImage(c *cmdCtx, args []string) int {
	if len(args) == 0 || args[0] != "build" {
		fmt.Fprintln(c.stderr, "usage: pilldrops image build <context-dir> -t <name>")
		return ExitUsage
	}
	// Order-independent parse so both `build <dir> -t name` and `build -t name
	// <dir>` work (Go's flag package would stop at the first positional).
	var name, dir string
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "-t", "--tag":
			if i+1 < len(rest) {
				name = rest[i+1]
				i++
			}
		default:
			if dir == "" && !strings.HasPrefix(rest[i], "-") {
				dir = rest[i]
			}
		}
	}
	if dir == "" || name == "" {
		fmt.Fprintln(c.stderr, "usage: pilldrops image build <context-dir> -t <name>")
		fmt.Fprintln(c.stderr, "  the context dir must contain a Dockerfile at its root")
		return ExitUsage
	}

	data, err := tarDir(dir) // Dockerfile must be at the context root
	if err != nil {
		return c.fail(err)
	}
	base, key := resolveCreds(c.apiURL)
	if key == "" {
		return c.fail(errNotLoggedIn)
	}
	// A Docker build takes minutes and fans out to every node — long timeout.
	cl := &Client{Base: base, Key: key, HTTP: &http.Client{Timeout: 25 * time.Minute}}
	if !c.quiet {
		fmt.Fprintf(c.stderr, "building %q from %s/ on the fleet (this can take a few minutes)…\n", name, dir)
	}
	body, err := cl.Raw("POST", "/v1/images/build?name="+url.QueryEscape(name), data, "application/octet-stream")
	if err != nil {
		return c.fail(err)
	}
	var res struct {
		OK    bool             `json:"ok"`
		Tag   string           `json:"tag"`
		Log   string           `json:"log"`
		Nodes []map[string]any `json:"nodes"`
	}
	_ = json.Unmarshal(body, &res)
	if c.jsonOut {
		printJSON(c.stdout, json.RawMessage(body))
		if res.OK {
			return ExitOK
		}
		return ExitAPI
	}
	if !res.OK {
		if res.Log != "" {
			fmt.Fprintln(c.stderr, res.Log)
		}
		fmt.Fprintln(c.stderr, "pilldrops: image build failed")
		return ExitAPI
	}
	fmt.Fprintf(c.stdout, "built %s (on %d node(s))\n", res.Tag, len(res.Nodes))
	fmt.Fprintf(c.stdout, "launch it:  pilldrops sandbox create --image %s\n", res.Tag)
	return ExitOK
}
