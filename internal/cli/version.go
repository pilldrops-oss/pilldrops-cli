package cli

import (
	"fmt"
	"runtime"
)

// Version is the CLI release version, printed by `pilldrops version` and
// reported by doctor. Bump on every user-visible CLI change — installed
// binaries update themselves from /dl/ via `pilldrops upgrade`.
// Keep in step with internal/mcp.ServerVersion.
const Version = "1.11.0"

func cmdVersion(c *cmdCtx, _ []string) int {
	if c.jsonOut {
		printJSON(c.stdout, map[string]string{
			"version": Version, "os": runtime.GOOS, "arch": runtime.GOARCH,
		})
		return ExitOK
	}
	fmt.Fprintf(c.stdout, "pilldrops %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
	return ExitOK
}
