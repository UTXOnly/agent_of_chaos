package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/UTXOnly/agent_of_chaos/internal/ddapi"
	"github.com/UTXOnly/agent_of_chaos/internal/notebook"
	"github.com/UTXOnly/agent_of_chaos/internal/report"
)

func init() {
	register(command{name: "notebook", short: "prepare evidence for an MCP-authored notebook, or record its URL", run: runNotebook})
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

// notebookResult records prepared evidence and any previously linked notebook.
type notebookResult struct {
	File string // notebook.json sidecar
	URL  string // empty when not created
}

// makeNotebook prepares reusable evidence cells; the coding agent authors and
// publishes the analysis through MCP. This path never makes a network call.
func makeNotebook(reports []*report.Report, opts notebook.Options, outFile string) (notebookResult, error) {
	var nb *notebook.Notebook
	if len(reports) == 1 {
		nb = notebook.ForRun(reports[0], opts)
	} else {
		nb = notebook.ForCompare(reports, opts)
	}
	return saveNotebook(nb, outFile)
}

func saveNotebook(nb *notebook.Notebook, outFile string) (notebookResult, error) {
	res := notebookResult{File: outFile, URL: recordedNotebookURL(outFile)}
	if err := os.MkdirAll(filepath.Dir(outFile), 0o755); err != nil {
		return res, err
	}
	if err := os.WriteFile(outFile, nb.File(), 0o644); err != nil {
		return res, err
	}
	if err := os.WriteFile(strings.TrimSuffix(outFile, ".json")+".md", []byte(nb.Handoff(filepath.Base(outFile))), 0o644); err != nil {
		return res, err
	}
	return res, nil
}

func recordedNotebookURL(outFile string) string {
	if b, err := os.ReadFile(strings.TrimSuffix(outFile, ".json") + ".url"); err == nil {
		return strings.TrimSpace(string(b))
	}
	// Older results sometimes carry the link only in ab.json.
	var sum abSummary
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(outFile), "ab.json")); err == nil && json.Unmarshal(b, &sum) == nil {
		return sum.Notebook
	}
	return ""
}

func explainNotebook(res notebookResult) {
	fmt.Printf("notebook evidence: %s (local only)\n", strings.TrimSuffix(res.File, ".json")+".md")
	if res.URL != "" {
		fmt.Printf("notebook: %s (existing; update through Datadog MCP)\n", res.URL)
	} else {
		fmt.Println("notebook: not published; the coding agent writes the analysis and publishes through Datadog MCP (docs/notebooks.md)")
	}
}

// linkNotebook records the URL returned by MCP without fetching or publishing.
func linkNotebook(dir, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("--link needs the HTTPS notebook URL returned by Datadog MCP")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "notebook" {
		return fmt.Errorf("--link needs a /notebook/<id> URL")
	}
	if id, err := strconv.ParseUint(parts[1], 10, 64); err != nil || id == 0 {
		return fmt.Errorf("--link needs a numeric notebook id")
	}
	// Require an existing result. Preserve unknown ab.json fields for tooling.
	abFile := filepath.Join(dir, "ab.json")
	if b, err := os.ReadFile(abFile); err == nil {
		var sum map[string]json.RawMessage
		if err := json.Unmarshal(b, &sum); err != nil || sum == nil {
			return fmt.Errorf("invalid ab.json in %s", dir)
		}
		sum["notebook"], _ = json.Marshal(rawURL)
		b, err = json.MarshalIndent(sum, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(abFile, b, 0o644); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if _, err := loadReport(dir); err != nil {
		return fmt.Errorf("--link needs an existing run or A/B results directory: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, "notebook.url"), []byte(rawURL+"\n"), 0o644)
}

func runNotebook(args []string) int {
	fs := newFlagSet("notebook",
		"Prepare local notebook evidence for a run or A/B test. The coding agent\n"+
			"reads notebook.md, writes a concise analysis and publishes through\n"+
			"Datadog MCP. No app key or network call is used by this command.\n"+
			"Use --link to save the notebook URL returned by MCP.",
		"  aoc notebook --results results/my-change\n"+
			"  aoc notebook --results results/baseline --results results/candidate\n"+
			"  aoc notebook --results results/my-change --link https://app.datadoghq.com/notebook/123")
	var results stringList
	fs.Var(&results, "results", "results directory (or report.json); repeat to compare")
	name := fs.Str("name", "", "notebook title (default: derived from the run names)")
	site := fs.Str("site", envOr("DD_SITE", "datadoghq.com"), "Datadog site (env DD_SITE)")
	subdomain := fs.Str("subdomain", os.Getenv("DD_SUBDOMAIN"), "your org's custom subdomain for links, e.g. bhartford (env DD_SUBDOMAIN)")
	appURL := fs.Str("app-url", os.Getenv("DD_APP_URL"), "full browser base URL for links, overrides --site/--subdomain (env DD_APP_URL)")
	out := fs.Str("out", "", "where to write the notebook JSON (default <first results dir>/notebook.json)")
	fs.Bool("dry-run", false, "deprecated: notebook preparation is always local")
	link := fs.Str("link", "", "record the notebook URL returned by MCP; requires one results directory")
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
	if *link != "" {
		if len(results) != 1 || *out != "" {
			return fail("notebook: --link requires one results directory and no --out")
		}
		if err := linkNotebook(results[0], *link); err != nil {
			return fail("notebook: %v", err)
		}
		fmt.Printf("recorded notebook: %s\n", *link)
		return 0
	}
	if len(results) == 1 {
		if _, err := os.Stat(filepath.Join(results[0], "ab.json")); err == nil {
			b, err := os.ReadFile(filepath.Join(results[0], "notebook.json"))
			if err != nil {
				return fail("notebook: %v; regenerate evidence with aoc ab --config %s --compare-only", err, filepath.Join(results[0], "aoc.yaml"))
			}
			var nb notebook.Notebook
			if err := json.Unmarshal(b, &nb); err != nil {
				return fail("notebook: %v", err)
			}
			if *name != "" {
				nb.Name = *name
			}
			outFile := *out
			if outFile == "" {
				outFile = filepath.Join(results[0], "notebook.json")
			}
			res, err := saveNotebook(&nb, outFile)
			if err != nil {
				return fail("notebook: %v", err)
			}
			explainNotebook(res)
			return 0
		}
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
	opts := notebook.Options{Site: *site, AppURL: ddapi.AppURLFor(*site, *subdomain, *appURL), Name: *name, Prefix: *prefix, AgentContainer: *agentContainer, AgentHost: *agentHost}
	res, err := makeNotebook(reports, opts, outFile)
	if err != nil {
		return fail("notebook: %v", err)
	}
	explainNotebook(res)
	return 0
}
