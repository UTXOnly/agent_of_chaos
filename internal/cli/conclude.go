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
)

func init() {
	register(command{name: "conclude", short: "record the conclusion of an A/B investigation in Datadog (event) and next to its results", run: runConclude})
}

// runConclude is the last step of an investigation: the human or agent that
// read findings.md and dug into the profiles writes down what the change
// actually did, and that text becomes a source:aoc event tagged with the
// experiment (next to the "finished" event) and conclusion.md next to
// ab.json. Adding it to the notebook is one MCP call (edit_datadog_notebook)
// away; the URL is printed.
func runConclude(args []string) int {
	fs := newFlagSet("conclude",
		"Record the conclusion of an A/B test: what the difference between the\n"+
			"two agents turned out to be, and why. Writes conclusion.md into the\n"+
			"results directory and posts it as a Datadog event tagged\n"+
			"experiment:<name> so it sits next to the test's metrics and profiles.",
		"  aoc conclude --results results/tag-filter \"container memory +160% is python3 check runners, not the logs pipeline; core RSS and heap flat\"\n"+
			"  aoc conclude --results results/tag-filter --file notes.md")
	results := fs.Str("results", "", "the experiment's results directory (the one with ab.json)")
	file := fs.Str("file", "", "read the conclusion from this Markdown file instead of the arguments")
	verdict := fs.Str("verdict", "", "one word for the event: pass, fail or inconclusive (default: from the text, else info)")
	if !fs.parse(args) {
		return 2
	}
	if *results == "" {
		return fail("conclude: --results is required")
	}
	var text string
	if *file != "" {
		b, err := os.ReadFile(*file)
		if err != nil {
			return fail("conclude: %v", err)
		}
		text = string(b)
	} else {
		text = strings.Join(fs.Args(), " ")
	}
	if strings.TrimSpace(text) == "" {
		return fail("conclude: give the conclusion as arguments or with --file")
	}
	b, err := os.ReadFile(filepath.Join(*results, "ab.json"))
	if err != nil {
		return fail("conclude: %v (is %s an A/B results directory?)", err, *results)
	}
	var sum abSummary
	if err := json.Unmarshal(b, &sum); err != nil {
		return fail("conclude: ab.json: %v", err)
	}
	out := filepath.Join(*results, "conclusion.md")
	body := fmt.Sprintf("# aoc A/B %s — conclusion\n\n%s\n\n_%s_\n", sum.Experiment, strings.TrimSpace(text), time.Now().UTC().Format("2006-01-02 15:04 UTC"))
	if err := os.WriteFile(out, []byte(body), 0o644); err != nil {
		return fail("conclude: %v", err)
	}
	fmt.Printf("wrote %s\n", out)

	alert := "info"
	switch strings.ToLower(*verdict) {
	case "pass":
		alert = "success"
	case "fail":
		alert = "error"
	case "inconclusive":
		alert = "warning"
	}
	client := ddapi.FromEnv()
	if !client.Configured() {
		fmt.Println("DD_API_KEY not set (env or .env): the conclusion was not posted to Datadog")
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ev := ddapi.Event{
		Title: fmt.Sprintf("aoc: A/B %s conclusion", sum.Experiment), Text: "%%% \n" + strings.TrimSpace(text) + "\n%%%",
		Tags: []string{"source:aoc", "harness:aoc", "experiment:" + sum.Experiment}, AlertType: alert, SourceTypeName: "aoc", DateHappened: time.Now().Unix(),
	}
	if len(ev.Text) > 3990 {
		ev.Text = ev.Text[:3900] + "\n…\n%%%"
	}
	if err := client.PostEvent(ctx, ev); err != nil {
		return fail("conclude: event: %v", err)
	}
	fmt.Printf("posted event \"%s\" (experiment:%s)\n", ev.Title, sum.Experiment)
	nbURL := sum.Notebook
	if nbURL == "" {
		if b, err := os.ReadFile(filepath.Join(*results, "notebook.url")); err == nil {
			nbURL = strings.TrimSpace(string(b))
		}
	}
	switch {
	case nbURL == "":
		fmt.Println("no notebook recorded for this test; `aoc ab --compare-only` (with DD_APP_KEY) creates one with the conclusion on top")
	case client.AppKey == "":
		fmt.Printf("notebook: %s — no DD_APP_KEY, so add the conclusion as its first cell yourself (Datadog MCP edit_datadog_notebook, or `aoc conclude` again with the key)\n", nbURL)
	default:
		cell := "## Conclusion\n\n" + strings.TrimSpace(text) + "\n"
		if err := client.PrependNotebookCell(ctx, ddapi.NotebookID(nbURL), cell); err != nil {
			return fail("conclude: notebook: %v", err)
		}
		fmt.Printf("notebook: %s — conclusion added as the first cell\n", nbURL)
	}
	fmt.Println("`aoc ab --compare-only` re-renders findings.md with the conclusion on top")
	return 0
}
