// Command aoc is the agent_of_chaos Datadog logs-agent testing harness:
// a log generator, a fake logs intake that reports to Datadog, and an
// experiment runner that turns a run into a report and a Datadog notebook.
package main

import (
	"os"

	"github.com/UTXOnly/agent_of_chaos/internal/cli"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	os.Exit(cli.Main(os.Args[1:], version))
}
