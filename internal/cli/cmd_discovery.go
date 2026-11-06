package cli

import (
	"fmt"
	"io"
	"sort"
)

// cmdCapabilities reads GET /v1/capabilities. Both tiers answer it: a control
// plane returns the fleet aggregate ("scope":"fleet"), a bare node its own
// ("scope":"node"). Ask before requesting something the deployment would refuse
// — a create naming network tags where tag_networks is false is a 501 the
// caller could have predicted.
func cmdCapabilities(c *cmdCtx, _ []string) int {
	return getAndPrint(c, "/v1/capabilities")
}

// cmdLimits reads GET /v1/limits: the plan ceilings this credential is subject
// to, plus what it has used against them.
func cmdLimits(c *cmdCtx, _ []string) int {
	return getAndPrint(c, "/v1/limits")
}

// getAndPrint fetches one JSON object and renders it as sorted key/value rows.
// Both answers are flat-ish maps whose fields change with the deployment, so a
// fixed column set would hide exactly the field a caller is checking for.
func getAndPrint(c *cmdCtx, path string) int {
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	var out map[string]any
	if err := cl.JSON("GET", path, nil, &out); err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, out)
		return ExitOK
	}
	printKV(c.stdout, out)
	return ExitOK
}

// printKV renders an object as sorted key/value rows.
func printKV(w io.Writer, obj map[string]any) {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rows := [][]string{}
	for _, k := range keys {
		rows = append(rows, []string{k, renderValue(obj[k])})
	}
	table(w, rows)
}

// renderValue keeps a nested object on one line rather than pretty-printing a
// tree into a two-column table; --json is there for the full shape.
func renderValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case []any:
		s := ""
		for i, e := range t {
			if i > 0 {
				s += ","
			}
			s += str(e)
		}
		return s
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		s := ""
		for i, k := range keys {
			if i > 0 {
				s += " "
			}
			s += fmt.Sprintf("%s=%v", k, t[k])
		}
		return s
	}
	return str(v)
}
