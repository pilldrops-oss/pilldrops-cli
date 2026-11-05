package cli

import (
	"encoding/json"
	"errors"
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

	data, err := tarContext(dir)
	if err != nil {
		return c.fail(err)
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	// A Docker build takes minutes and fans out to every node — long timeout.
	cl.HTTP = &http.Client{Timeout: 25 * time.Minute}
	if !c.quiet {
		fmt.Fprintf(c.stderr, "building %q from %s/ on the fleet (this can take a few minutes)…\n", name, dir)
	}
	body, err := cl.Raw("POST", "/v1/images/build?name="+url.QueryEscape(name), data, "application/octet-stream")
	// A failed build answers non-2xx with the build's own report; any other
	// refusal (bad key, oversize context) has no "ok" and is an ordinary error.
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		var report struct {
			OK *bool `json:"ok"`
		}
		if json.Unmarshal(apiErr.Body, &report) == nil && report.OK != nil {
			body, err = apiErr.Body, nil
		}
	}
	if err != nil {
		return c.fail(err)
	}
	var res imageBuildResult
	_ = json.Unmarshal(body, &res)
	if c.jsonOut {
		printJSON(c.stdout, json.RawMessage(body))
		if res.OK {
			return ExitOK
		}
		return ExitAPI
	}
	if !res.OK {
		if tail := logTail(res.Log, 40); tail != "" {
			fmt.Fprintln(c.stderr, tail)
		}
		fmt.Fprintln(c.stderr, "pilldrops: image build failed: "+res.reason())
		return ExitAPI
	}
	fmt.Fprintf(c.stdout, "built %s (on %d node(s))\n", res.Tag, len(res.Nodes))
	fmt.Fprintf(c.stdout, "launch it:  pilldrops sandbox create --image %s\n", res.Tag)
	return ExitOK
}

type buildStep struct {
	Error string `json:"error"`
}

// imageBuildResult is POST /v1/images/build's report. A node answering directly
// says why in error, a control plane in build and publish, an older one in nodes.
type imageBuildResult struct {
	OK      bool        `json:"ok"`
	Tag     string      `json:"tag"`
	Log     string      `json:"log"`
	Error   string      `json:"error"`
	Build   buildStep   `json:"build"`
	Publish buildStep   `json:"publish"`
	Nodes   []buildStep `json:"nodes"`
}

func (r imageBuildResult) reason() string {
	reasons := []string{r.Error, r.Build.Error, r.Publish.Error}
	for _, n := range r.Nodes {
		reasons = append(reasons, n.Error)
	}
	for _, why := range reasons {
		if why != "" {
			return why
		}
	}
	return "the server gave no reason"
}

// logTail is the last n lines of a build log, which is where docker says what broke.
func logTail(log string, n int) string {
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
