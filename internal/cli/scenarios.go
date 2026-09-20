package cli

import (
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/UTXOnly/agent_of_chaos/internal/fmtutil"
	"github.com/UTXOnly/agent_of_chaos/internal/gen"
)

func init() {
	register(command{name: "scenarios", short: "list built-in workload scenarios, or print one as YAML to customise", run: runScenarios})
}

func runScenarios(args []string) int {
	if len(args) == 0 || args[0] == "list" {
		listScenarios(os.Stdout)
		return 0
	}
	if args[0] == "-h" || args[0] == "--help" {
		fmt.Println("Usage:\n  aoc scenarios            list built-in scenarios\n  aoc scenarios show NAME  print a scenario as YAML (edit it, then --scenario my.yaml)")
		return 0
	}
	if args[0] == "show" && len(args) > 1 {
		sc, err := gen.LoadScenario(args[1])
		if err != nil {
			return fail("%v", err)
		}
		out, _ := yaml.Marshal(sc)
		os.Stdout.Write(out)
		return 0
	}
	return fail("scenarios: unknown arguments %v (try: aoc scenarios show wave)", args)
}

func listScenarios(w io.Writer) {
	fmt.Fprintf(w, "\nBuilt-in scenarios (aoc generate --mode scenario --scenario NAME [--scenario-speed N]):\n\n")
	for _, name := range gen.BuiltinNames() {
		sc := gen.Builtin[name]
		fmt.Fprintf(w, "  %-14s %s\n", name, sc.Description)
		fmt.Fprintf(w, "  %-14s phases=%d  cycle=%s  loop=%v\n\n", "", len(sc.Phases), fmtutil.Duration(sc.TotalDuration()), sc.Loop)
	}
	fmt.Fprintf(w, "Print one as YAML to customise: aoc scenarios show wave > my.yaml\n\n")
}
