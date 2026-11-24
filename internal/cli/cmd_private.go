package cli

import (
	"flag"
	"fmt"
	"net/url"
)

const privateUsage = `usage: pilldrops private grant --tag <tag> ` + grantArgUsage + ` | grants [--tag <tag>] | revoke <grant-id>`

// cmdPrivate manages grants to network tags: every sandbox in the tag reaches
// what the grant names, including sandboxes that join it later.
func cmdPrivate(c *cmdCtx, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, privateUsage)
		return ExitUsage
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	if code, ok := requirePrivateAccess(c, cl); !ok {
		return code
	}
	fs := flag.NewFlagSet("private "+args[0], flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	tag := fs.String("tag", "", "network tag")
	switch args[0] {
	case "grant":
		if fs.Parse(args[1:]) != nil || *tag == "" || fs.NArg() != 1 {
			break
		}
		body, ok := privateGrantArg(fs.Arg(0))
		if !ok {
			break
		}
		body["tag"] = *tag
		var out map[string]any
		if err := cl.JSON("POST", "/v1/private/grants", body, &out); err != nil {
			return c.fail(adminOnly(err))
		}
		if c.jsonOut {
			printJSON(c.stdout, out)
			return ExitOK
		}
		fmt.Fprintf(c.stdout, "%s\t%s\t%s\n", str(out["id"]), str(out["tag"]), str(out["value"]))
		printShadowed(c, out)
		if un, ok := out["unsupported"].([]any); ok && len(un) > 0 {
			fmt.Fprintf(c.stderr, "%d sandbox(es) in the tag run on a node that cannot use this grant yet:", len(un))
			for _, id := range un {
				fmt.Fprintf(c.stderr, " %s", str(id))
			}
			fmt.Fprintln(c.stderr)
		}
		return ExitOK
	case "grants", "list", "ls":
		if fs.Parse(args[1:]) != nil || fs.NArg() != 0 {
			break
		}
		path := "/v1/private/grants"
		if *tag != "" {
			path += "?" + url.Values{"tag": {*tag}}.Encode()
		}
		if c.jsonOut {
			var raw map[string]any
			if err := cl.JSON("GET", path, nil, &raw); err != nil {
				return c.fail(err)
			}
			printJSON(c.stdout, raw)
			return ExitOK
		}
		var out struct {
			Grants []struct{ ID, Tag, Connector, Kind, Value string } `json:"grants"`
		}
		if err := cl.JSON("GET", path, nil, &out); err != nil {
			return c.fail(err)
		}
		rows := [][]string{{"ID", "TAG", "CONNECTOR", "KIND", "VALUE"}}
		for _, g := range out.Grants {
			rows = append(rows, []string{g.ID, g.Tag, g.Connector, g.Kind, g.Value})
		}
		table(c.stdout, rows)
		return ExitOK
	case "revoke", "rm":
		if len(args) != 2 {
			break
		}
		if err := cl.JSON("DELETE", "/v1/private/grants/"+url.PathEscape(args[1]), nil, nil); err != nil {
			return c.fail(adminOnly(err))
		}
		if c.jsonOut {
			printJSON(c.stdout, map[string]any{"status": "revoked", "id": args[1]})
		} else if !c.quiet {
			fmt.Fprintf(c.stdout, "revoked %s\n", args[1])
		}
		return ExitOK
	}
	fmt.Fprintln(c.stderr, privateUsage)
	return ExitUsage
}
