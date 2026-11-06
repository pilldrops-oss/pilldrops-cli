package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/pilldrops-oss/pilldrops-cli/pkg/mcp"
)

// cmdMCP runs a local MCP server on stdio using the stored login, so agent
// harnesses get sandbox tools with zero config:
//
//	claude mcp add pilldrops -- pilldrops mcp
func cmdMCP(c *cmdCtx, _ []string) int {
	cl, err := c.client()
	if err != nil {
		// A stdio server has no one to ask: serve, and let each tool explain.
		base, _ := resolveCreds(c.apiURL)
		if base == "" {
			base = DefaultAPIURL
		}
		cl = &Client{Base: base}
		fmt.Fprintln(c.stderr, "pilldrops mcp: not logged in — tools will fail until `pilldrops login` (serving anyway)")
	}

	be := func(method, path string, body []byte, ctype string) (int, []byte, error) {
		resp, err := cl.Raw(method, path, body, ctype)
		if err != nil {
			if apiErr, ok := err.(*APIError); ok {
				return apiErr.Status, []byte(`{"error":` + jsonString(apiErr.Msg) + `}`), nil
			}
			return 0, nil, err
		}
		return 200, resp, nil
	}

	fmt.Fprintf(c.stderr, "pilldrops mcp %s serving on stdio (api: %s)\n", mcp.ServerVersion, cl.Base)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 16<<20) // generated files can be large
	w := bufio.NewWriter(c.stdout)
	for in.Scan() {
		line := in.Bytes()
		if len(line) == 0 {
			continue
		}
		if resp := mcp.HandleMessage(line, be); resp != nil {
			_, _ = w.Write(resp)
			_ = w.WriteByte('\n')
			_ = w.Flush()
		}
	}
	return ExitOK
}

// jsonString safely quotes s as a JSON string literal.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
