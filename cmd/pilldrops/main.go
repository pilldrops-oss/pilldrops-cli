// sandforge is the SandForge customer CLI. See internal/cli.
package main

import (
	"os"

	"github.com/pilldrops-oss/pilldrops-cli/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
