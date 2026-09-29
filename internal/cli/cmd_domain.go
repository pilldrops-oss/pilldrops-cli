package cli

import (
	"bufio"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// domain mirrors the /v1/domains wire shape.
type domain struct {
	Host      string `json:"host"`
	SandboxID string `json:"sandbox_id"`
	Port      int    `json:"port"`
	Wildcard  bool   `json:"wildcard"`
	Status    string `json:"status"`
	Error     string `json:"error"`
	Record    string `json:"record"`
	Target    string `json:"cname_target"`
}

const domainUsage = `usage: pilldrops domain add <host> --sandbox <id> [--port N] [--confirm]
       pilldrops domain add '*.vms.example.com' [--confirm]   (every sandbox, by label)
       pilldrops domain list
       pilldrops domain rm <host>`

// domainPollFor bounds the wait for DNS to propagate after the record is made;
// domainPollEvery is how often the status is re-read. Vars so a test does not
// take two minutes to watch the give-up path.
var (
	domainPollFor   = 2 * time.Minute
	domainPollEvery = 5 * time.Second
)

// cmdDomain attaches customer hostnames to public sandboxes. add prints the one
// DNS record to create, waits for it, and exits non-zero if it never appears —
// so a script does not go on believing the host is live.
func cmdDomain(c *cmdCtx, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, domainUsage)
		return ExitUsage
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	switch args[0] {
	case "add":
		return domainAdd(c, cl, args[1:])
	case "list", "ls":
		var page struct {
			Domains []domain `json:"domains"`
		}
		if err := cl.JSON("GET", "/v1/domains", nil, &page); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, page.Domains)
			return ExitOK
		}
		rows := [][]string{{"HOST", "SANDBOX", "PORT", "STATUS", "DETAIL"}}
		for _, d := range page.Domains {
			port := ""
			if d.Port != 0 {
				port = strconv.Itoa(d.Port)
			}
			rows = append(rows, []string{d.Host, d.SandboxID, port, d.Status, d.Error})
		}
		table(c.stdout, rows)
		return ExitOK
	case "rm", "delete":
		if len(args) != 2 {
			fmt.Fprintln(c.stderr, domainUsage)
			return ExitUsage
		}
		if err := cl.JSON("DELETE", "/v1/domains/"+url.PathEscape(args[1]), nil, nil); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, map[string]any{"removed": args[1]})
		} else if !c.quiet {
			fmt.Fprintf(c.stdout, "removed %s\n", args[1])
		}
		return ExitOK
	}
	fmt.Fprintln(c.stderr, domainUsage)
	return ExitUsage
}

func domainAdd(c *cmdCtx, cl *Client, args []string) int {
	var host, sandbox string
	port, confirm := 0, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--sandbox", "-s":
			if i+1 >= len(args) {
				fmt.Fprintln(c.stderr, domainUsage)
				return ExitUsage
			}
			sandbox, i = args[i+1], i+1
		case "--port", "-p":
			if i+1 >= len(args) {
				fmt.Fprintln(c.stderr, domainUsage)
				return ExitUsage
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				fmt.Fprintln(c.stderr, "--port must be a number")
				return ExitUsage
			}
			port, i = n, i+1
		case "--confirm", "-y":
			confirm = true
		default:
			if host != "" {
				fmt.Fprintln(c.stderr, domainUsage)
				return ExitUsage
			}
			host = args[i]
		}
	}
	if host == "" {
		fmt.Fprintln(c.stderr, domainUsage)
		return ExitUsage
	}

	body := map[string]any{"host": host}
	if sandbox != "" {
		body["sandbox_id"] = sandbox
	}
	if port != 0 {
		body["port"] = port
	}
	var d domain
	if err := cl.JSON("POST", "/v1/domains", body, &d); err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, d)
	} else {
		fmt.Fprintf(c.stdout, "Create this DNS record:\n\n    %s\n\n", d.Record)
	}
	if !confirm {
		fmt.Fprintln(c.stdout, "Press Enter once the record exists (Ctrl-C to check later with `pilldrops domain list`).")
		_, _ = bufio.NewReader(stdin).ReadString('\n')
	}

	// Poll rather than trust the create: nothing routes until the verifier has
	// seen the record, so "added" on its own would be a lie.
	deadline := time.Now().Add(domainPollFor)
	last := d
	for {
		var page struct {
			Domains []domain `json:"domains"`
		}
		if err := cl.JSON("GET", "/v1/domains", nil, &page); err != nil {
			return c.fail(err)
		}
		for _, row := range page.Domains {
			if row.Host == d.Host {
				last = row
			}
		}
		if last.Status == "active" {
			if !c.quiet {
				fmt.Fprintf(c.stdout, "%s is live\n", last.Host)
			}
			return ExitOK
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(domainPollEvery)
	}
	detail := last.Error
	if detail == "" {
		detail = "the record has not appeared yet"
	}
	fmt.Fprintf(c.stderr, "%s is still pending: %s\n", last.Host, detail)
	return ExitAPI
}
