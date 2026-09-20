package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/ddapi"
	"github.com/UTXOnly/agent_of_chaos/internal/notebook"
	"github.com/UTXOnly/agent_of_chaos/internal/report"
)

func init() {
	register(command{name: "notebook", short: "create a Datadog notebook for a run, or one comparing several runs", run: runNotebook})
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(s string) error { *l = append(*l, s); return nil }

// loadReport reads report.json from a results directory or a file path.
func loadReport(path string) (*report.Report, error) {
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		path = filepath.Join(path, "report.json")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r report.Report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if r.Name == "" || r.Name == "aoc" {
		r.Name = filepath.Base(filepath.Dir(path))
	}
	return &r, nil
}

// notebookResult is what creating (or preparing) a notebook produced.
type notebookResult struct {
	File string // notebook.json sidecar
	URL  string // empty when not created
}

// makeNotebook builds the notebook, writes the sidecar and, when an
// application key is available and dryRun is false, creates it in Datadog.
func makeNotebook(ctx context.Context, reports []*report.Report, opts notebook.Options, outFile string, client *ddapi.Client, dryRun bool) (notebookResult, error) {
	var nb *notebook.Notebook
	if len(reports) == 1 {
		nb = notebook.ForRun(reports[0], opts)
	} else {
		nb = notebook.ForCompare(reports, opts)
	}
	res := notebookResult{File: outFile}
	if err := os.MkdirAll(filepath.Dir(outFile), 0o755); err != nil {
		return res, err
	}
	if err := os.WriteFile(outFile, nb.File(), 0o644); err != nil {
		return res, err
	}
	if dryRun || client == nil || client.AppKey == "" {
		return res, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	_, url, err := client.CreateNotebook(cctx, nb.Body())
	if err != nil {
		return res, err
	}
	res.URL = url
	os.WriteFile(strings.TrimSuffix(outFile, ".json")+".url", []byte(url+"\n"), 0o644)
	return res, nil
}

func explainNotebook(res notebookResult, hasAppKey bool) {
	if res.URL != "" {
		fmt.Printf("notebook created: %s\n", res.URL)
		return
	}
	fmt.Printf("wrote %s\n", res.File)
	if !hasAppKey {
		fmt.Printf("no DD_APP_KEY, so the notebook was not created. Either:\n")
		fmt.Printf("  DD_APP_KEY=… aoc notebook --results %s\n", filepath.Dir(res.File))
		fmt.Printf("  or hand the cells in %s to the Datadog MCP's create_datadog_notebook (name, cells, absolute start/end are all in the file)\n", filepath.Base(res.File))
	}
}

func runNotebook(args []string) int {
	fs := newFlagSet("notebook",
		"Create a Datadog notebook from a run's report: a findings summary, the\n"+
			"aoc.* metrics the intake submitted, the agent's own integration metrics\n"+
			"and profiler/event links, all scoped to the run's time window. With two\n"+
			"or more results directories it writes the comparison table and each\n"+
			"run's key charts pinned to its own window.",
		"  aoc notebook --results results/baseline\n"+
			"  aoc notebook --results results/baseline --results results/candidate --name \"tag filter A/B\"\n"+
			"  aoc notebook --results results/baseline --dry-run     # just write notebook.json (for the Datadog MCP)")
	var results stringList
	fs.Var(&results, "results", "results directory (or report.json); repeat to compare")
	name := fs.Str("name", "", "notebook title (default: derived from the run names)")
	site := fs.Str("site", envOr("DD_SITE", "datadoghq.com"), "Datadog site (env DD_SITE)")
	apiKey := fs.Str("api-key", os.Getenv("DD_API_KEY"), "API key (env DD_API_KEY)")
	appKey := fs.Str("app-key", os.Getenv("DD_APP_KEY"), "application key (env DD_APP_KEY); required to create, not to write the JSON")
	out := fs.Str("out", "", "where to write the notebook JSON (default <first results dir>/notebook.json)")
	dryRun := fs.Bool("dry-run", false, "write the JSON only")
	agentContainer := fs.Str("agent-container", "", "agent container name for docker integration queries (default: from the report)")
	agentHost := fs.Str("agent-host", "", "agent hostname for profiler/infrastructure links (default: from the report)")
	prefix := fs.Str("prefix", "aoc", "metric prefix the intake used")
	if !fs.parse(args) {
		return 2
	}
	results = append(results, fs.Args()...)
	if len(results) == 0 {
		return fail("notebook: --results DIR is required")
	}
	var reports []*report.Report
	for _, p := range results {
		r, err := loadReport(p)
		if err != nil {
			return fail("notebook: %v", err)
		}
		reports = append(reports, r)
	}
	outFile := *out
	if outFile == "" {
		first := results[0]
		if st, err := os.Stat(first); err == nil && !st.IsDir() {
			first = filepath.Dir(first)
		}
		outFile = filepath.Join(first, "notebook.json")
		if len(reports) > 1 {
			outFile = filepath.Join(first, "notebook-compare.json")
		}
	}
	client := ddapi.New(*site, *apiKey, *appKey)
	opts := notebook.Options{Site: *site, Name: *name, Prefix: *prefix, AgentContainer: *agentContainer, AgentHost: *agentHost}
	res, err := makeNotebook(context.Background(), reports, opts, outFile, client, *dryRun)
	if err != nil {
		return fail("notebook: %v", err)
	}
	explainNotebook(res, *appKey != "")
	return 0
}
