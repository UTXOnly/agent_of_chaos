package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/UTXOnly/agent_of_chaos/internal/ddapi"
	"github.com/UTXOnly/agent_of_chaos/internal/findings"
	"github.com/UTXOnly/agent_of_chaos/internal/gen"
	"github.com/UTXOnly/agent_of_chaos/internal/notebook"
	"github.com/UTXOnly/agent_of_chaos/internal/prof"
	"github.com/UTXOnly/agent_of_chaos/internal/report"
)

func init() {
	register(command{name: "ab", short: "A/B test two agent images on the same workload, driven by aoc.yaml", run: runAB})
}

// ABConfig is aoc.yaml: two agents, one workload, where the results go. The
// shipped aoc.yaml documents every field.
type ABConfig struct {
	Name      string            `yaml:"name"`
	Profile   ProfileRef        `yaml:"profile"`
	Duration  gen.Duration      `yaml:"duration"`
	Warmup    gen.Duration      `yaml:"warmup"`
	Drain     gen.Duration      `yaml:"drain"`
	Runs      int               `yaml:"runs"`
	Pause     gen.Duration      `yaml:"pause"`
	A         Variant           `yaml:"a"`
	B         Variant           `yaml:"b"`
	Env       map[string]string `yaml:"env"`
	Results   string            `yaml:"results"`
	Notebook  *bool             `yaml:"notebook"`
	Compose   []string          `yaml:"compose"`
	Threshold float64           `yaml:"threshold"` // percent; a headline change below it is noise
	Source    string            `yaml:"source"`    // a checkout of the agent's repository, for the code section
}

// Variant is one side of the test: an image and what applies to it only.
type Variant struct {
	Name  string            `yaml:"name"`
	Image string            `yaml:"image"`
	Pull  string            `yaml:"pull"`
	Env   map[string]string `yaml:"env"`
}

// ProfileRef is `profile:` — the path of a profile file, or the profile
// written inline.
type ProfileRef struct {
	Path   string
	Inline *Profile
	src    []byte
}

func (p *ProfileRef) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Decode(&p.Path)
	case yaml.MappingNode:
		var prof Profile
		if err := n.Decode(&prof); err != nil {
			return err
		}
		p.Inline = &prof
		p.src, _ = yaml.Marshal(n)
		return nil
	}
	return errors.New("profile: expected a path or a mapping")
}

func (p ProfileRef) String() string {
	if p.Inline != nil {
		return "inline"
	}
	return p.Path
}

const defaultAImage = "datadog/agent:7"

// loadABConfig reads and validates an aoc.yaml. Unknown keys are errors:
// a typo in a config is easier to spot now than after a 10-minute run.
func loadABConfig(path string) (*ABConfig, []byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var c ABConfig
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Runs <= 0 {
		c.Runs = 1
	}
	if c.Pause == 0 {
		c.Pause = gen.Duration(10 * time.Second)
	}
	if c.Results == "" {
		c.Results = "results"
	}
	if c.Threshold <= 0 {
		c.Threshold = 10
	}
	if c.A.Name == "" {
		c.A.Name = "a"
	}
	if c.B.Name == "" {
		c.B.Name = "b"
	}
	if c.A.Image == "" {
		c.A.Image = defaultAImage
	}
	for _, v := range []*Variant{&c.A, &c.B} {
		if v.Pull == "" {
			v.Pull = "always"
		}
	}
	switch {
	case c.Profile.Path == "" && c.Profile.Inline == nil:
		return nil, nil, fmt.Errorf("%s: profile is required (a path such as profiles/baseline.yaml, or the profile inline)", path)
	case c.B.Image == "":
		return nil, nil, fmt.Errorf("%s: b.image is required (the development image to test)", path)
	case sanitize(c.A.Name) == sanitize(c.B.Name):
		return nil, nil, fmt.Errorf("%s: a.name and b.name must differ", path)
	}
	for _, v := range []Variant{c.A, c.B} {
		if err := checkPull(v.Pull); err != nil {
			return nil, nil, fmt.Errorf("%s: %s.pull: %v", path, v.Name, err)
		}
	}
	return &c, b, nil
}

// workload resolves the profile the two sides run, with the config's
// duration/warmup/drain on top.
func (c *ABConfig) workload() (*Profile, []byte, error) {
	var p *Profile
	src := c.Profile.src
	if c.Profile.Inline != nil {
		var err error
		if p, err = parseProfile(src, "inline"); err != nil {
			return nil, nil, fmt.Errorf("profile: %w", err)
		}
	} else {
		var err error
		if p, err = loadProfile(c.Profile.Path); err != nil {
			return nil, nil, err
		}
		src, _ = os.ReadFile(c.Profile.Path)
	}
	if c.Duration > 0 {
		p.Duration = c.Duration
	}
	if c.Warmup > 0 {
		p.Warmup = c.Warmup
	}
	if c.Drain > 0 {
		p.Drain = c.Drain
	}
	if len(c.Env) > 0 {
		env := map[string]string{}
		for k, v := range p.Agent.Env {
			env[k] = v
		}
		for k, v := range c.Env {
			env[k] = v
		}
		p.Agent.Env = env
	}
	p.Compose = append(p.Compose, c.Compose...)
	return p, src, nil
}

// abRun is one planned run: a side, a round, and where it lands.
type abRun struct {
	side    string // a | b
	variant Variant
	round   int
	name    string // run:<name> — <experiment>-<variant>[-<round>]
	dir     string // <results>/<experiment>/<variant>[-<round>]
}

// plan lays out the runs: round 1 of a, round 1 of b, round 2 of a, … so
// that whatever drifts on the host over the session affects both sides.
func (c *ABConfig) plan(experiment string) []abRun {
	exp := sanitize(experiment)
	var runs []abRun
	for round := 1; round <= c.Runs; round++ {
		for _, sv := range []struct {
			side string
			v    Variant
		}{{"a", c.A}, {"b", c.B}} {
			suffix := sanitize(sv.v.Name)
			if round > 1 {
				suffix += fmt.Sprintf("-%d", round)
			}
			runs = append(runs, abRun{side: sv.side, variant: sv.v, round: round, name: exp + "-" + suffix, dir: filepath.Join(c.Results, exp, suffix)})
		}
	}
	return runs
}

// abSummary is ab.json: the identities behind the comparison, for tooling.
type abSummary struct {
	Experiment string             `json:"experiment"`
	Config     string             `json:"config"`
	Profile    string             `json:"profile"`
	Started    time.Time          `json:"started"`
	Finished   time.Time          `json:"finished"`
	Sides      []abSide           `json:"sides"`
	Threshold  float64            `json:"threshold_pct"`
	Summary    string             `json:"summary"` // "2 regression(s) in memory, cpu; 3 improvement(s)"
	Findings   []findings.Finding `json:"findings"`
	Headline   []abHeadlineRow    `json:"headline"`
	Notebook   string             `json:"notebook,omitempty"`
	Files      map[string]string  `json:"files"`
}

type abSide struct {
	Side    string      `json:"side"`
	Name    string      `json:"name"`
	Image   string      `json:"image"`
	Digest  string      `json:"digest,omitempty"`
	Version string      `json:"agent_version,omitempty"`
	Runs    []abSideRun `json:"runs"`
}

type abSideRun struct {
	Run         string    `json:"run"`
	Dir         string    `json:"dir"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	Lost        int64     `json:"lost"`
	Duplicates  int64     `json:"duplicates"`
}

type abHeadlineRow struct {
	Metric string   `json:"metric"`
	Values []string `json:"values"`
	Delta  string   `json:"delta"`
}

func runAB(args []string) int {
	fs := newFlagSet("ab",
		"A/B test two Datadog Agent images on one workload, as described by an\n"+
			"aoc.yaml: run the profile against side a (the latest public release by\n"+
			"default), then against side b (a development build), optionally for\n"+
			"several alternating rounds, and compare — a Markdown table, ab.json, a\n"+
			"Datadog notebook with both agents overlaid on every chart, and every\n"+
			"metric/event tagged experiment:<name> variant:<side>.",
		"  aoc ab                          # ./aoc.yaml\n"+
			"  aoc ab --config tests/tag-filter.yaml --runs 3\n"+
			"  aoc ab --plan                   # validate the config and show what would run\n"+
			"  aoc ab --only b                 # rebuilt the dev image? re-run b, reuse a's results")
	config := fs.Str("config", "aoc.yaml", "the test to run (a copy is kept with the results)")
	name := fs.Str("name", "", "experiment name (overrides the config's `name`)")
	duration := fs.Duration("duration", 0, "override the measured window for both sides")
	runs := fs.Int("runs", 0, "override `runs`: rounds per side, alternating a, b, a, b, …")
	threshold := fs.Float("threshold", 0, "override `threshold`: percent change on a headline metric that counts as a finding")
	only := fs.Str("only", "", "run only this side (a, b, or a side's name); the other side's existing results are reused")
	plan := fs.Bool("plan", false, "print the resolved plan and exit without running anything")
	compareOnly := fs.Bool("compare-only", false, "skip the runs; rebuild compare.md, ab.json and the notebook from the results on disk")
	composeFile := fs.Str("compose-file", "docker-compose.yml", "base compose file")
	composeCmd := fs.Str("compose-cmd", "docker compose", "compose command")
	intake := fs.Str("intake", "http://localhost:8282", "how this machine reaches the intake")
	noBuild := fs.Bool("no-build", false, "do not rebuild the aoc image before the first run")
	mkNotebook := fs.Bool("notebook", os.Getenv("DD_APP_KEY") != "", "create the A/B notebook in Datadog (default: the config's `notebook`, else when DD_APP_KEY is set; notebook.json is always written)")
	postEvent := fs.Bool("event", false, "with --compare-only: post the finished event again (it is posted after every run by default)")
	ddSite := fs.Str("dd-site", envOr("DD_SITE", "datadoghq.com"), "Datadog site (env DD_SITE)")
	if !fs.parse(args) {
		return 2
	}
	cfg, cfgSrc, err := loadABConfig(*config)
	if err != nil {
		return fail("ab: %v", err)
	}
	if *runs > 0 {
		cfg.Runs = *runs
	}
	if *duration > 0 {
		cfg.Duration = gen.Duration(*duration)
	}
	if *threshold > 0 {
		cfg.Threshold = *threshold
	}
	if cfg.Notebook != nil && !flagGiven(fs, "notebook") {
		*mkNotebook = *cfg.Notebook
	}
	p, profileSrc, err := cfg.workload()
	if err != nil {
		return fail("ab: %v", err)
	}
	experiment := *name
	if experiment == "" {
		experiment = cfg.Name
	}
	if experiment == "" {
		experiment = "ab-" + p.Name
	}
	experiment = sanitize(experiment)
	all := cfg.plan(experiment)
	todo := all
	if *compareOnly {
		todo = nil
	} else if *only != "" {
		todo = nil
		for _, r := range all {
			if r.side == *only || r.variant.Name == *only {
				todo = append(todo, r)
			}
		}
		if len(todo) == 0 {
			return fail("ab: --only %q matches neither side (a=%s, b=%s)", *only, cfg.A.Name, cfg.B.Name)
		}
	}
	root := filepath.Join(cfg.Results, experiment)

	fmt.Fprintf(os.Stderr, "\naoc ab  %s  profile=%s  window=%s  warmup=%s  drain=%s  rounds=%d\n", experiment, cfg.Profile, p.Duration, p.Warmup, p.Drain, cfg.Runs)
	if p.Description != "" {
		fmt.Fprintf(os.Stderr, "  %s\n", p.Description)
	}
	for _, sv := range []struct {
		side string
		v    Variant
	}{{"a", cfg.A}, {"b", cfg.B}} {
		fmt.Fprintf(os.Stderr, "  %s  %-12s %s  (pull %s)%s\n", sv.side, sv.v.Name, sv.v.Image, sv.v.Pull, envSummary(sv.v.Env))
	}
	if len(cfg.Env) > 0 {
		fmt.Fprintf(os.Stderr, "  both%s\n", envSummary(cfg.Env))
	}
	fmt.Fprintf(os.Stderr, "  results → %s/   tags experiment:%s variant:<side>\n\n", root, experiment)
	for i, r := range all {
		skip := ""
		if len(todo) < len(all) && !containsRun(todo, r) {
			skip = "   (not run; results reused from disk)"
		}
		fmt.Fprintf(os.Stderr, "  %2d. %s  run:%-32s → %s%s\n", i+1, r.side, r.name, r.dir, skip)
	}
	fmt.Fprintln(os.Stderr)
	if *plan {
		return 0
	}

	if err := os.MkdirAll(root, 0o755); err != nil {
		return fail("ab: %v", err)
	}
	os.WriteFile(filepath.Join(root, "aoc.yaml"), cfgSrc, 0o644)
	started := time.Now()
	ctx, cancel := signalContext()
	defer cancel()
	logf := newLogf()
	for i, r := range todo {
		fmt.Fprintf(os.Stderr, "── run %d/%d: %s (%s) ──\n", i+1, len(todo), r.variant.Name, r.variant.Image)
		spec := runSpec{
			profile: p, profileSrc: profileSrc, name: r.name,
			agent:      agentSpec{Image: r.variant.Image, Pull: r.variant.Pull, Env: r.variant.Env},
			resultsDir: r.dir,
			composeCmd: *composeCmd, composeFile: *composeFile, intake: *intake,
			noBuild: *noBuild || i > 0,
			tags:    []string{"experiment:" + experiment, "variant:" + sanitize(r.variant.Name)},
			logf:    logf,
		}
		rep, err := executeRun(ctx, spec)
		if errors.Is(err, errInterrupted) {
			fmt.Fprintln(os.Stderr, "\naoc: interrupted")
			return 130
		}
		if err != nil {
			return fail("ab: run %s: %v", r.name, err)
		}
		fmt.Fprintf(os.Stderr, "\n  %s: agent %s · delivered %s · lost %s · dup %s · e2e p99 %s · container cpu %.0f%%\n\n",
			r.variant.Name, report.AgentLabel(rep), pct(rep.Delivery.Unique, rep.Delivery.GeneratedRecords), fmtInt(rep.Delivery.Missing), fmtInt(rep.Delivery.Duplicates), secs(rep.Latency.EndToEnd.P99), rep.Resources.ContainerCPUAvg)
		if i < len(todo)-1 && cfg.Pause > 0 {
			logf("pausing %s before the next run", cfg.Pause)
			if !sleepCtx(ctx, cfg.Pause.D()) {
				fmt.Fprintln(os.Stderr, "\naoc: interrupted")
				return 130
			}
		}
	}

	// Compare whatever is on disk for the planned runs (fresh, or reused
	// with --only).
	cols, err := abColumns(cfg, all)
	if err != nil {
		return fail("ab: %v", err)
	}
	compareMD := report.CompareColumns(cols)
	os.WriteFile(filepath.Join(root, "compare.md"), []byte(compareMD), 0o644)

	// The brief: what moved, and the evidence — per-process memory, the
	// profiles diffed by function, telemetry, the agent's log.
	workload := "`" + p.Name + "`"
	if d := firstSentence(p.Description); d != "" {
		workload += " (" + d + ")"
	}
	conclusion := ""
	if b, err := os.ReadFile(filepath.Join(root, "conclusion.md")); err == nil {
		conclusion = strings.TrimSpace(stripConclusionHeader(string(b)))
	}
	in := findings.Input{Experiment: experiment, Cols: cols, Captures: abCaptures(cols, all), Threshold: cfg.Threshold, AppURL: ddapi.AppURLFromEnv(), ResultsDir: root,
		Workload: workload, ConfigDiff: cfg.configDiff(), Source: findings.DetectSource(cfg.Source), Conclusion: conclusion}
	res := findings.Build(in)
	findingsMD := findings.Markdown(res, in)
	os.WriteFile(filepath.Join(root, "findings.md"), []byte(findingsMD), 0o644)
	if b, err := json.MarshalIndent(res, "", "  "); err == nil {
		os.WriteFile(filepath.Join(root, "findings.json"), b, 0o644)
	}

	title := fmt.Sprintf("aoc A/B %s: %s vs %s", experiment, cfg.A.Name, cfg.B.Name)
	opts := notebook.Options{Site: *ddSite, AppURL: ddapi.AppURLFromEnv(), Experiment: experiment, Findings: res}
	nb := notebook.ForAB(title, cols, opts)
	nbFile := filepath.Join(root, "notebook.json")
	nbURL, nbErr := createNotebook(context.Background(), nb, nbFile, ddapi.FromEnv(), !*mkNotebook)

	headline := res.Headline
	sum := abSummary{Experiment: experiment, Config: *config, Profile: cfg.Profile.String(), Started: started, Finished: time.Now(), Notebook: nbURL,
		Threshold: cfg.Threshold, Summary: res.Summary, Findings: res.Findings,
		Files: map[string]string{"findings": filepath.Join(root, "findings.md"), "findings_json": filepath.Join(root, "findings.json"), "compare": filepath.Join(root, "compare.md"), "notebook": nbFile}}
	if res.Findings == nil {
		sum.Findings = []findings.Finding{}
	}
	for i, c := range cols {
		side := abSide{Side: []string{"a", "b"}[i], Name: c.Name, Image: c.Runs[0].Agent.Image, Digest: c.Runs[0].Agent.Digest, Version: topKey(c.Runs[0].Agent.Versions)}
		for _, r := range c.Runs {
			dir := ""
			for _, pr := range all {
				if pr.name == r.Name {
					dir = pr.dir
				}
			}
			side.Runs = append(side.Runs, abSideRun{Run: r.Name, Dir: dir, WindowStart: r.WindowStart, WindowEnd: r.WindowEnd, Lost: r.Delivery.Missing, Duplicates: r.Delivery.Duplicates})
		}
		sum.Sides = append(sum.Sides, side)
	}
	for _, h := range headline {
		sum.Headline = append(sum.Headline, abHeadlineRow{Metric: h.Metric, Values: h.Cells, Delta: strings.Join(h.Deltas, " · ")})
	}
	if b, err := json.MarshalIndent(sum, "", "  "); err == nil {
		os.WriteFile(filepath.Join(root, "ab.json"), b, 0o644)
	}

	fmt.Printf("\naoc ab %s — %s vs %s\n\n", experiment, report.AgentLabel(cols[0].Runs[0]), report.AgentLabel(cols[1].Runs[0]))
	for _, v := range res.Verdict {
		fmt.Printf("  %s\n", v)
	}
	for _, sec := range res.Sections {
		if sec.Reading != "" {
			fmt.Printf("  %s: %s\n", sec.Topic, sec.Reading)
		}
	}
	fmt.Println()
	if len(res.Changed) > 0 {
		printHeadline(cols, res.Changed)
		fmt.Printf("  (rows that moved by 5%% or more; every metric is in compare.md)\n")
	}
	fmt.Printf("\n  findings: %s   (what was tested, what differed and where, the profiles and the code behind them)\n", filepath.Join(root, "findings.md"))
	fmt.Printf("  results:  %s/ (ab.json, notebook.json, <side>/report.md, <side>/profiles/ …)\n", root)
	switch {
	case nbErr != nil:
		fmt.Printf("  notebook: %v\n", nbErr)
	case nbURL != "":
		fmt.Printf("  notebook: %s\n", nbURL)
	case os.Getenv("DD_APP_KEY") == "":
		fmt.Printf("  notebook: wrote %s (no DD_APP_KEY) — hand its cells to the Datadog MCP's create_datadog_notebook, or set the key and run `aoc ab --compare-only`\n", nbFile)
	default:
		fmt.Printf("  notebook: wrote %s (not created: notebook: false)\n", nbFile)
	}
	if len(todo) > 0 || *postEvent {
		if !ddapi.FromEnv().Configured() {
			fmt.Printf("  event: DD_API_KEY not set (env or .env) — the finished event was not posted\n")
		}
		postABEvent(experiment, cols, findings.EventText(res, in, nbURL))
	}

	bad := false
	for _, c := range cols {
		for _, r := range c.Runs {
			if r.Delivery.Missing > 0 || r.Delivery.Duplicates > 0 {
				bad = true
			}
		}
	}
	if bad {
		return 3
	}
	return 0
}

// configDiff lists what only one side had: the per-side env, and the
// images when their tags differ only there.
func (c *ABConfig) configDiff() []string {
	var out []string
	side := func(name string, mine, other map[string]string) {
		for _, k := range sortedStringKeys(mine) {
			if v, ok := other[k]; !ok || v != mine[k] {
				val := mine[k]
				if len(val) > 80 {
					val = val[:77] + "…"
				}
				out = append(out, fmt.Sprintf("only `%s` has %s=%s", name, k, val))
			}
		}
	}
	side(c.A.Name, c.A.Env, c.B.Env)
	side(c.B.Name, c.B.Env, c.A.Env)
	if len(c.Env) > 0 {
		out = append(out, fmt.Sprintf("both have %s", envSummary(c.Env)[len("  env: "):]))
	}
	return out
}

// firstSentence trims a profile description to its first sentence, at
// most 140 characters.
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ". "); i > 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(s, ".")
	if len(s) > 140 {
		s = s[:137] + "…"
	}
	return s
}

// stripConclusionHeader drops the "# aoc A/B … — conclusion" heading and
// the trailing timestamp line aoc conclude writes, leaving the text.
func stripConclusionHeader(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "# aoc A/B") || (strings.HasPrefix(t, "_") && strings.HasSuffix(t, "UTC_")) {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// abCaptures loads each side's profile captures (every run's, restricted
// to its window) from the results directories.
func abCaptures(cols []report.Column, all []abRun) [][]prof.Capture {
	out := make([][]prof.Capture, len(cols))
	for i, c := range cols {
		for _, r := range c.Runs {
			for _, pr := range all {
				if pr.name != r.Name {
					continue
				}
				caps, err := prof.LoadDir(pr.dir)
				if err != nil {
					continue
				}
				out[i] = append(out[i], prof.Overlapping(caps, r.WindowStart, r.WindowEnd)...)
			}
		}
	}
	return out
}

// abColumns loads the planned runs' reports from disk, one column per side.
func abColumns(cfg *ABConfig, all []abRun) ([]report.Column, error) {
	cols := []report.Column{{Name: cfg.A.Name}, {Name: cfg.B.Name}}
	for _, r := range all {
		rep, err := loadReport(r.dir)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "  no results for %s in %s (skipped)\n", r.name, r.dir)
				continue
			}
			return nil, err
		}
		rep.Name = r.name
		if len(rep.Profiles) == 0 {
			// Reports written before the profiles were analysed (or by an
			// older aoc) still have the pprof files next to them.
			if caps, err := prof.LoadDir(r.dir); err == nil {
				rep.Profiles = prof.Summaries(prof.Overlapping(caps, rep.WindowStart, rep.WindowEnd), 40)
				if rep.Agent.Commit == "" {
					rep.Agent.Commit, rep.Agent.Repo = profileTag(caps, "git.commit.sha"), profileTag(caps, "git.repository_url")
				}
			}
		}
		i := 0
		if r.side == "b" {
			i = 1
		}
		cols[i].Runs = append(cols[i].Runs, rep)
	}
	for _, c := range cols {
		if len(c.Runs) == 0 {
			return nil, fmt.Errorf("no results for side %s — run without --only", c.Name)
		}
	}
	return cols, nil
}

// createNotebook writes the sidecar and, unless dryRun or without an app
// key, creates the notebook in Datadog.
func createNotebook(ctx context.Context, nb *notebook.Notebook, outFile string, client *ddapi.Client, dryRun bool) (string, error) {
	if err := os.WriteFile(outFile, nb.File(), 0o644); err != nil {
		return "", err
	}
	if dryRun || client == nil || client.AppKey == "" {
		return "", nil
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	_, url, err := client.CreateNotebook(cctx, nb.Body())
	if err != nil {
		return "", err
	}
	os.WriteFile(strings.TrimSuffix(outFile, ".json")+".url", []byte(url+"\n"), 0o644)
	return url, nil
}

func printHeadline(cols []report.Column, rows []report.HeadlineRow) {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
		if len(c.Runs) > 1 {
			names[i] += fmt.Sprintf(" (median of %d)", len(c.Runs))
		}
	}
	fmt.Printf("  %-28s %-18s %-18s %s\n", "", names[0], names[1], "Δ "+cols[1].Name+" vs "+cols[0].Name)
	for _, r := range rows {
		fmt.Printf("  %-28s %-18s %-18s %s\n", r.Metric, r.Cells[0], r.Cells[1], strings.Join(r.Deltas, " · "))
	}
}

// postABEvent leaves the verdict in Datadog as an event (source:aoc).
func postABEvent(experiment string, cols []report.Column, text string) {
	client := ddapi.FromEnv()
	if !client.Configured() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := client.PostEvent(ctx, ddapi.Event{
		Title: fmt.Sprintf("aoc: A/B %s finished — %s vs %s", experiment, cols[0].Name, cols[1].Name), Text: text,
		Tags: []string{"source:aoc", "harness:aoc", "experiment:" + experiment}, AlertType: "info", SourceTypeName: "aoc", DateHappened: time.Now().Unix(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "  event: %v\n", err)
	}
}

func envSummary(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	var parts []string
	for _, k := range sortedStringKeys(env) {
		v := env[k]
		if len(v) > 40 {
			v = v[:37] + "…"
		}
		parts = append(parts, k+"="+v)
	}
	return "  env: " + strings.Join(parts, " ")
}

func containsRun(runs []abRun, r abRun) bool {
	for _, x := range runs {
		if x.name == r.name {
			return true
		}
	}
	return false
}

func topKey(m map[string]int64) string {
	best, bestN := "", int64(-1)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if m[k] > bestN {
			best, bestN = k, m[k]
		}
	}
	return best
}

// flagGiven reports whether a flag was set on the command line.
func flagGiven(fs *flagSet, name string) bool {
	given := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			given = true
		}
	})
	return given
}
