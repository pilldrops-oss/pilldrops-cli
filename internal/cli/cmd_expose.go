package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// forward mirrors the /v1/sandboxes/{id}/forwards wire shape.
type forward struct {
	GuestPort  int      `json:"guest_port"`
	PublicPort int      `json:"public_port"`
	TCP        bool     `json:"tcp"`
	UDP        bool     `json:"udp"`
	Protocols  []string `json:"protocols"`
	Endpoint   string   `json:"endpoint"`
}

const exposeUsage = `usage: pilldrops sandbox expose <id> <port> [--tcp] [--udp]
       pilldrops sandbox forwards <id>
       pilldrops sandbox unexpose <id> <port>`

// sandboxExpose publishes a guest port on a raw public TCP/UDP endpoint.
// Naming neither protocol means TCP, which is what "expose a port" means to
// anyone who has typed `ssh -L`.
func sandboxExpose(c *cmdCtx, cl *Client, args []string) int {
	id, port, rest, ok := idAndPort(c, args)
	if !ok {
		return ExitUsage
	}
	tcp, udp := false, false
	for _, a := range rest {
		switch a {
		case "--tcp":
			tcp = true
		case "--udp":
			udp = true
		default:
			fmt.Fprintln(c.stderr, exposeUsage)
			return ExitUsage
		}
	}
	body := map[string]any{"port": port, "tcp": tcp || !udp, "udp": udp}
	var f forward
	if err := cl.JSON("POST", "/v1/sandboxes/"+id+"/forwards", body, &f); err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, f)
	} else if !c.quiet {
		fmt.Fprintf(c.stdout, "%s -> %d (%s)\n", f.Endpoint, f.GuestPort, protoList(f))
	}
	return ExitOK
}

func sandboxForwards(c *cmdCtx, cl *Client, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(c.stderr, exposeUsage)
		return ExitUsage
	}
	var page struct {
		Forwards []forward `json:"forwards"`
	}
	if err := cl.JSON("GET", "/v1/sandboxes/"+args[0]+"/forwards", nil, &page); err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, page.Forwards)
		return ExitOK
	}
	rows := [][]string{{"ENDPOINT", "GUEST", "PROTO"}}
	for _, f := range page.Forwards {
		rows = append(rows, []string{f.Endpoint, strconv.Itoa(f.GuestPort), protoList(f)})
	}
	table(c.stdout, rows)
	return ExitOK
}

func sandboxUnexpose(c *cmdCtx, cl *Client, args []string) int {
	id, port, rest, ok := idAndPort(c, args)
	if !ok {
		return ExitUsage // idAndPort already said why
	}
	if len(rest) != 0 {
		fmt.Fprintln(c.stderr, exposeUsage)
		return ExitUsage
	}
	if err := cl.JSON("DELETE", "/v1/sandboxes/"+id+"/forwards/"+strconv.Itoa(port), nil, nil); err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, map[string]any{"unexposed": port, "sandbox": id})
	} else if !c.quiet {
		fmt.Fprintf(c.stdout, "unexposed %s:%d\n", id, port)
	}
	return ExitOK
}

// idAndPort parses the shared "<id> <port>" head of these verbs.
func idAndPort(c *cmdCtx, args []string) (id string, port int, rest []string, ok bool) {
	if len(args) < 2 {
		fmt.Fprintln(c.stderr, exposeUsage)
		return "", 0, nil, false
	}
	p, err := strconv.Atoi(args[1])
	if err != nil || p < 1 || p > 65535 {
		fmt.Fprintf(c.stderr, "port must be 1-65535, got %q\n", args[1])
		return "", 0, nil, false
	}
	return args[0], p, args[2:], true
}

func protoList(f forward) string {
	if len(f.Protocols) > 0 {
		return strings.Join(f.Protocols, "/")
	}
	switch {
	case f.TCP && f.UDP:
		return "tcp/udp"
	case f.UDP:
		return "udp"
	default:
		return "tcp"
	}
}
