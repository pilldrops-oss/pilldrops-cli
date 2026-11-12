package cli

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/pilldrops-oss/pilldrops-cli/internal/cluster"
)

//go:embed cluster.example.toml
var clusterExample string

const clusterUsage = `usage: pilldrops cluster apply [-f cluster.toml] [--prune]   install, upgrade or update the fleet it describes
       pilldrops cluster example                  print a cluster.toml to start from`

// cmdCluster installs a self-hosted fleet over ssh. It talks to hosts, not to
// a configured server, so it needs no login.
func cmdCluster(c *cmdCtx, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, clusterUsage)
		return ExitUsage
	}
	switch args[0] {
	case "example":
		fmt.Fprint(c.stdout, clusterExample)
		return ExitOK
	case "apply":
		fs := flag.NewFlagSet("apply", flag.ContinueOnError)
		fs.SetOutput(c.stderr)
		file := fs.String("f", "cluster.toml", "the fleet's config file")
		prune := fs.Bool("prune", false, "drain and remove nodes that cluster.toml no longer lists")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			fmt.Fprintln(c.stderr, clusterUsage)
			return ExitUsage
		}
		if _, err := os.Stat(*file); err != nil {
			fmt.Fprintf(c.stderr, "%v\nstart one with: pilldrops cluster example > cluster.toml\n", err)
			return ExitUsage
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		// Not c.fail: its hints are about the API, and these errors are about hosts.
		if err := cluster.Apply(ctx, *file, c.stderr, *prune); err != nil {
			fmt.Fprintln(c.stderr, "pilldrops:", err)
			return ExitAPI
		}
		return ExitOK
	default:
		fmt.Fprintln(c.stderr, clusterUsage)
		return ExitUsage
	}
}
