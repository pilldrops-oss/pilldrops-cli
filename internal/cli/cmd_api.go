package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"
)

const apiUsage = "usage: pilldrops api <METHOD> </v1/path> [--data JSON | --data -]"

// cmdAPI is the escape hatch: anything without a typed verb, with the stored
// credential and the same exit codes. /v1 only, because the credential belongs
// to the API and nothing else on that host should be sent it.
func cmdAPI(c *cmdCtx, args []string) int {
	fs := flag.NewFlagSet("api", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	data := fs.String("data", "", "JSON request body, or - to read it from stdin")
	pos, err := parseMixed(fs, args)
	if err != nil || len(pos) != 2 {
		fmt.Fprintln(c.stderr, apiUsage)
		return ExitUsage
	}
	method := strings.ToUpper(pos[0])
	if !slices.Contains([]string{"GET", "POST", "PUT", "PATCH", "DELETE"}, method) {
		fmt.Fprintf(c.stderr, "pilldrops api: unknown method %q\n%s\n", pos[0], apiUsage)
		return ExitUsage
	}
	if !isV1Path(pos[1]) {
		fmt.Fprintf(c.stderr, "pilldrops api: %q is not under /v1/\n%s\n", pos[1], apiUsage)
		return ExitUsage
	}
	var body []byte
	ctype := ""
	if flagWasSet(fs, "data") {
		if body, err = readDataFlag(*data); err != nil {
			return c.fail(err)
		}
		if !json.Valid(body) {
			fmt.Fprintln(c.stderr, "pilldrops api: --data is not valid JSON")
			return ExitUsage
		}
		ctype = "application/json"
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	slow(cl)
	raw, err := cl.Raw(method, pos[1], body, ctype)
	if err != nil {
		return c.fail(err)
	}
	call := &verbCall{c: c, cl: cl}
	if c.jsonOut {
		call.printJSONBody(raw, nil)
		return ExitOK
	}
	_, _ = c.stdout.Write(raw)
	return ExitOK
}

// isV1Path judges the path the server would see, so /v1/../admin does not pass.
func isV1Path(p string) bool {
	u, err := url.Parse(p)
	return err == nil && u.Host == "" && strings.HasPrefix(p, "/v1/") &&
		strings.HasPrefix(path.Clean(u.Path)+"/", "/v1/")
}
