package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	register(command{name: "ab", short: "A/B test a build against the release on one workload (--b IMAGE)", run: runAB})
}

// ABConfig is aoc.yaml: the settings a test runs with — the control image,
// the shared agent env, the thresholds, the default workload. What a given
// test is about (the image under test, its feature flag, the workload, the
// focus) comes from the aoc ab flags. The config as it ran is written next
// to the results, so --config repeats the test.
type ABConfig struct {
	Name       string             `yaml:"name,omitempty"`
	Profile    ProfileRef         `yaml:"profile"`
	Duration   gen.Duration       `yaml:"duration,omitempty"`
	Warmup     gen.Duration       `yaml:"warmup,omitempty"`
	Drain      gen.Duration       `yaml:"drain,omitempty"`
	Runs       int                `yaml:"runs"`
	Pause      gen.Duration       `yaml:"pause,omitempty"`
	Parallel   *bool              `yaml:"parallel,omitempty"` // both sides at once (default), or one after the other
	A          Variant            `yaml:"a"`
	B          Variant            `yaml:"b"`
	Env        map[string]string  `yaml:"env,omitempty"`
	Results    string             `yaml:"results"`
	Notebook   *bool              `yaml:"notebook,omitempty"`
	Compose    []string           `yaml:"compose,omitempty"`
	Threshold  float64            `yaml:"threshold"`            // percent; a headline change below it is noise
	Source     string             `yaml:"source,omitempty"`     // a checkout of the agent's repository, for the code section
	Focus      string             `yaml:"focus,omitempty"`      // the question this test answers; usually from --focus
	Code       []string           `yaml:"code,omitempty"`       // packages under test, e.g. pkg/logs/sender
	Watch      []string           `yaml:"watch,omitempty"`      // extra compare.md metrics promoted to the headline
	Thresholds map[string]float64 `yaml:"thresholds,omitempty"` // percent per signal: throughput, saturation, cpu, memory
	Faults     map[string]any     `yaml:"faults,omitempty"`     // intake faults for the whole window, both sides; replaces the workload's timeline
}

// Variant is one side of the test: an image and what applies to it only.
type Variant struct {
	Name  string            `yaml:"name"`
	Image string            `yaml:"image"`
	Pull  string            `yaml:"pull"`
	Env   map[string]string `yaml:"env,omitempty"`
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

func (p ProfileRef) MarshalYAML() (any, error) {
	if p.Inline != nil {
		return p.Inline, nil
	}
	return p.Path, nil
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
// The image under test and the workload can come from the flags instead,
// so validate checks for those once the flags are in.
func loadABConfig(path string) (*ABConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c ABConfig
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
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
	if sanitize(c.A.Name) == sanitize(c.B.Name) {
		return nil, fmt.Errorf("%s: a.name and b.name must differ", path)
	}
	for _, v := range []Variant{c.A, c.B} {
		if err := checkPull(v.Pull); err != nil {
			return nil, fmt.Errorf("%s: %s.pull: %v", path, v.Name, err)
		}
	}
	if err := checkThresholds(c.Thresholds); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	return &c, nil
}

// thresholdSignals are the signals `thresholds` can give a percent to;
// `threshold` covers the ones it leaves out.
var thresholdSignals = []string{"throughput", "saturation", "cpu", "memory"}

func checkThresholds(t map[string]float64) error {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys) // so a config with two bad keys always reports the same one
	for _, k := range keys {
		known := false
		for _, s := range thresholdSignals {
			known = known || k == s
		}
		if !known {
			return fmt.Errorf("thresholds: %q is not a signal (%s)", k, strings.Join(thresholdSignals, ", "))
		}
	}
	return nil
}

// thresholdSummary is the percent per signal, `threshold` filling in the
// signals `thresholds` does not name.
func (c *ABConfig) thresholdSummary() string {
	parts := make([]string, 0, len(thresholdSignals))
	for _, s := range thresholdSignals {
		v, ok := c.Thresholds[s]
		if !ok {
			v = c.Threshold
		}
		parts = append(parts, fmt.Sprintf("%s %s%%", s, strconv.FormatFloat(v, 'g', -1, 64)))
	}
	return strings.Join(parts, " · ")
}

// validate reports what neither the config nor the flags supplied.
func (c *ABConfig) validate(path string) error {
	switch {
	case c.B.Image == "":
		return fmt.Errorf("no image to test: pass --b IMAGE, or set b.image in %s", path)
	case c.Profile.Path == "" && c.Profile.Inline == nil:
		return fmt.Errorf("no workload: pass --workload NAME, or set profile in %s", path)
	}
	if err := checkFaults(c.Faults); len(c.Faults) > 0 && err != nil {
		return fmt.Errorf("faults in %s: %w", path, err)
	}
	return nil
}

// workloadRef resolves --workload: a name is a profile in profiles/, a
// path is taken as it is.
func workloadRef(v string) ProfileRef {
	if strings.Contains(v, "/") || strings.HasSuffix(v, ".yaml") || strings.HasSuffix(v, ".yml") {
		return ProfileRef{Path: v}
	}
	return ProfileRef{Path: filepath.Join("profiles", v+".yaml")}
}

// imageTag is an image reference's tag, or its last path element when it
// carries none.
func imageTag(image string) string {
	if i := strings.LastIndex(image, ":"); i >= 0 && !strings.Contains(image[i+1:], "/") {
		return image[i+1:]
	}
	if i := strings.LastIndex(image, "/"); i >= 0 {
		return image[i+1:]
	}
	return image
}

// experimentName names a test that neither --name nor the config named:
// the tag under test and the workload.
func experimentName(bImage, workload string) string {
	return sanitize(imageTag(bImage) + "-" + workload)
}

// packagingNote warns when one image is a -full or -jmx build and the
// other is not: those run more processes, so container memory is not
// comparable between the sides.
func packagingNote(a, b Variant) string {
	pa, pb := imagePackaging(a.Image), imagePackaging(b.Image)
	if pa == pb {
		return ""
	}
	packaged, plain, kind := a, b, pa
	if pa == "" {
		packaged, plain, kind = b, a, pb
	}
	return fmt.Sprintf("warning: %s is a %s image and %s is not; container memory is not comparable", packaged.Name, strings.TrimPrefix(kind, "-"), plain.Name)
}

func imagePackaging(image string) string {
	tag := imageTag(image)
	for _, s := range []string{"-full", "-jmx"} {
		if strings.HasSuffix(tag, s) {
			return s
		}
	}
	return ""
}

// parseKV turns K=V flags into a map. The value keeps whatever follows
// the first =, so a JSON feature flag survives.
func parseKV(list []string) (map[string]string, error) {
	if len(list) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(list))
	for _, s := range list {
		k, v, ok := strings.Cut(s, "=")
		if k = strings.TrimSpace(k); !ok || k == "" {
			return nil, fmt.Errorf("%q is not K=V", s)
		}
		out[k] = v
	}
	return out, nil
}

// mergeEnv lays add over base without touching either.
func mergeEnv(base, add map[string]string) map[string]string {
	if len(add) == 0 {
		return base
	}
	out := make(map[string]string, len(base)+len(add))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range add {
		out[k] = v
	}
	return out
}

// listFlag is a repeatable flag; with split, one value may also be a
// comma-separated list.
type listFlag struct {
	vals  []string
	split bool
}

func (l *listFlag) String() string {
	if l == nil {
		return ""
	}
	return strings.Join(l.vals, ",")
}

func (l *listFlag) Set(s string) error {
	if !l.split {
		l.vals = append(l.vals, s)
		return nil
	}
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			l.vals = append(l.vals, v)
		}
	}
	return nil
}

// list registers a repeatable flag.
func (fs *flagSet) list(name string, split bool, usage string) *listFlag {
	l := &listFlag{split: split}
	fs.Var(l, name, usage)
	fs.track(name)
	return l
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
	if len(c.Faults) > 0 {
		p.Faults = []FaultStep{{Set: c.Faults}}
	}
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
	Focus      string             `json:"focus,omitempty"`
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
		"A/B test two Datadog Agent images on one workload: the build under test\n"+
			"(--b) against the control (the latest public release), on the workload\n"+
			"--workload names, optionally for several alternating rounds. aoc.yaml\n"+
			"holds the settings both tests share; the flags say what this one is\n"+
			"about. Out come findings.md, compare.md, ab.json, a Datadog notebook\n"+
			"with both agents overlaid, and metrics tagged experiment:<name>\n"+
			"variant:<side>.",
		"  aoc ab --b datadog/agent-dev:my-branch-py3 --workload baseline --focus \"is the new sender slower?\"\n"+
			"  aoc ab --b datadog/agent-dev:my-branch-py3 --b-env DD_LOGS_CONFIG_TAG_FILTERS='{\"exclude\":[\"dirname:*\"]}'\n"+
			"  aoc ab --b … --workload baseline --fault latency_ms=500,drop_rate=0.05   # both sides behind a slow, lossy intake\n"+
			"  aoc ab --b … --plan             # validate and show what would run\n"+
			"  aoc ab --only b                 # rebuilt the dev image? re-run b, reuse a's results\n"+
			"  aoc ab --config results/<name>/aoc.yaml --compare-only   # re-render a finished test")
	fs.section("The test")
	bImage := fs.Str("b", "", "the image under test (the config's `b.image` otherwise)")
	aImage := fs.Str("a", "", "the control image (the config's `a.image` otherwise, else "+defaultAImage+")")
	workload := fs.Str("workload", "", "the workload both sides run: a name in profiles/, or a path (the config's `profile` otherwise)")
	focus := fs.Str("focus", "", "the question this test answers; it opens the brief and names the notebook")
	code := fs.list("code", true, "a package under test, e.g. pkg/logs/sender; repeatable, commas allowed")
	watch := fs.list("watch", true, "a compare.md metric to promote to the headline; repeatable, commas allowed")
	bEnv := fs.list("b-env", false, "K=V only the image under test gets, e.g. the feature flag; repeatable")
	env := fs.list("env", false, "K=V both sides get; repeatable")
	fault := fs.list("fault", true, "an intake fault both sides get for the whole window, e.g. latency_ms=300 or drop_rate=0.05; replaces the workload's fault timeline; repeatable, commas allowed")
	config := fs.Str("config", "aoc.yaml", "the settings to run with (the effective config is kept with the results)")
	name := fs.Str("name", "", "experiment name (default: the config's `name`, else <b image tag>-<workload>)")
	fs.section("The run")
	duration := fs.Duration("duration", 0, "override the measured window for both sides")
	runs := fs.Int("runs", 0, "override `runs`: rounds per side")
	sequential := fs.Bool("sequential", false, "run the sides one after the other instead of alongside each other (the config's `parallel: false`)")
	threshold := fs.Float("threshold", 0, "percent change that counts as a finding, for every signal (overrides `threshold` and `thresholds`)")
	only := fs.Str("only", "", "run only this side (a, b, or a side's name); the other side's existing results are reused")
	plan := fs.Bool("plan", false, "print the resolved plan and exit without running anything")
	compareOnly := fs.Bool("compare-only", false, "skip the runs; rebuild compare.md, ab.json and the notebook from the results on disk")
	fs.section("Plumbing")
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
	cfg, err := loadABConfig(*config)
	if err != nil {
		return fail("ab: %v", err)
	}
	if *aImage != "" {
		cfg.A.Image = *aImage
	}
	if *bImage != "" {
		cfg.B.Image = *bImage
	}
	if *workload != "" {
		cfg.Profile = workloadRef(*workload)
	}
	if *focus != "" {
		cfg.Focus = *focus
	}
	if len(code.vals) > 0 {
		cfg.Code = code.vals
	}
	if len(watch.vals) > 0 {
		cfg.Watch = watch.vals
	}
	shared, err := parseKV(env.vals)
	if err != nil {
		return fail("ab: --env %v", err)
	}
	candidate, err := parseKV(bEnv.vals)
	if err != nil {
		return fail("ab: --b-env %v", err)
	}
	cfg.Env, cfg.B.Env = mergeEnv(cfg.Env, shared), mergeEnv(cfg.B.Env, candidate)
	if len(fault.vals) > 0 {
		if cfg.Faults, err = parseFaults(fault.vals); err != nil {
			return fail("ab: --fault: %v", err)
		}
	}
	if *runs > 0 {
		cfg.Runs = *runs
	}
	if *duration > 0 {
		cfg.Duration = gen.Duration(*duration)
	}
	if *threshold > 0 {
		cfg.Threshold = *threshold
		cfg.Thresholds = map[string]float64{}
		for _, sig := range thresholdSignals {
			cfg.Thresholds[sig] = *threshold
		}
	}
	if cfg.Notebook != nil && !flagGiven(fs, "notebook") {
		*mkNotebook = *cfg.Notebook
	}
	parallel := cfg.Parallel == nil || *cfg.Parallel
	if *sequential {
		parallel = false
	}
	cfg.Parallel = &parallel
	if err := cfg.validate(*config); err != nil {
		return fail("ab: %v", err)
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
		experiment = experimentName(cfg.B.Image, p.Name)
	}
	experiment = sanitize(experiment)
	cfg.Name = experiment
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

	order := "sides alongside each other"
	if !parallel {
		order = "sides one after the other"
	}
	fmt.Fprintf(os.Stderr, "\naoc ab  %s  profile=%s  window=%s  warmup=%s  drain=%s  rounds=%d  %s\n", experiment, cfg.Profile, p.Duration, p.Warmup, p.Drain, cfg.Runs, order)
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
	if note := packagingNote(cfg.A, cfg.B); note != "" {
		fmt.Fprintf(os.Stderr, "  %s\n", note)
	}
	if len(cfg.Faults) > 0 {
		fmt.Fprintf(os.Stderr, "  faults: %s, both sides, whole window\n", describeFaults(cfg.Faults))
	}
	if cfg.Focus != "" {
		fmt.Fprintf(os.Stderr, "  focus: %s\n", cfg.Focus)
	}
	if len(cfg.Code) > 0 {
		fmt.Fprintf(os.Stderr, "  code: %s\n", strings.Join(cfg.Code, " "))
	}
	if len(cfg.Watch) > 0 {
		fmt.Fprintf(os.Stderr, "  watch: %s\n", strings.Join(cfg.Watch, ", "))
	}
	fmt.Fprintf(os.Stderr, "  thresholds: %s\n", cfg.thresholdSummary())
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
	if eff, err := yaml.Marshal(cfg); err == nil {
		header := "# The test as it ran, flags included: aoc ab --config <this file> repeats it.\n"
		os.WriteFile(filepath.Join(root, "aoc.yaml"), append([]byte(header), eff...), 0o644)
	} else {
		fmt.Fprintf(os.Stderr, "  aoc.yaml: %v\n", err)
	}
	started := time.Now()
	ctx, cancel := signalContext()
	defer cancel()
	logf := newLogf()
	spec := func(r abRun, noBuild bool) runSpec {
		return runSpec{
			profile: p, profileSrc: profileSrc, name: r.name,
			agent:      agentSpec{Image: r.variant.Image, Pull: r.variant.Pull, Env: r.variant.Env},
			resultsDir: r.dir,
			composeCmd: *composeCmd, composeFile: *composeFile, intake: *intake,
			noBuild: noBuild,
			tags:    []string{"experiment:" + experiment, "variant:" + sanitize(r.variant.Name)},
			logf:    logf,
		}
	}
	summary := func(r abRun, rep *report.Report) {
		fmt.Fprintf(os.Stderr, "\n  %s: agent %s · delivered %s · lost %s · dup %s · e2e p99 %s · container cpu %.0f%%\n\n",
			r.variant.Name, report.AgentLabel(rep), pct(rep.Delivery.Unique, rep.Delivery.GeneratedRecords), fmtInt(rep.Delivery.Missing), fmtInt(rep.Delivery.Duplicates), secs(rep.Latency.EndToEnd.P99), rep.Resources.ContainerCPUAvg)
	}
	pause := func(more bool) bool {
		if more && cfg.Pause > 0 {
			logf("pausing %s before the next round", cfg.Pause)
			return sleepCtx(ctx, cfg.Pause.D())
		}
		return true
	}
	if parallel {
		// Both sides of a round at once, each in its own compose project
		// (aoc-a, aoc-b: separate containers, volumes and host ports) so
		// they share the host but nothing else; the aoc image is built once
		// before any of them starts.
		if len(todo) > 0 && !*noBuild {
			build := &compose{cmd: strings.Fields(*composeCmd), files: []string{*composeFile}, dir: ".", log: logf}
			if err := build.run(ctx, "build", "intake"); err != nil {
				if ctx.Err() != nil {
					fmt.Fprintln(os.Stderr, "\naoc: interrupted")
					return 130
				}
				return fail("ab: build: %v", err)
			}
		}
		rounds := byRound(todo)
		for ri, round := range rounds {
			var names []string
			for _, r := range round {
				names = append(names, fmt.Sprintf("%s (%s)", r.variant.Name, r.variant.Image))
			}
			fmt.Fprintf(os.Stderr, "── round %d/%d: %s ──\n", ri+1, len(rounds), strings.Join(names, "  ‖  "))
			type outcome struct {
				r   abRun
				rep *report.Report
				err error
			}
			results := make([]outcome, len(round))
			var wg sync.WaitGroup
			for i, r := range round {
				sp := spec(r, true)
				sp.stack = "aoc-" + r.side
				sp.intakePort, sp.tcpPort = intakeBasePort+sideIndex(r.side), tcpBasePort+sideIndex(r.side)
				sp.intake = withPort(*intake, sp.intakePort)
				sp.logf = newLogfPrefix(r.variant.Name)
				wg.Add(1)
				go func(i int, r abRun, sp runSpec) {
					defer wg.Done()
					rep, err := executeRun(ctx, sp)
					results[i] = outcome{r, rep, err}
				}(i, r, sp)
			}
			wg.Wait()
			for _, o := range results {
				if errors.Is(o.err, errInterrupted) {
					fmt.Fprintln(os.Stderr, "\naoc: interrupted")
					return 130
				}
				if o.err != nil {
					return fail("ab: run %s: %v", o.r.name, o.err)
				}
			}
			for _, o := range results {
				summary(o.r, o.rep)
			}
			if !pause(ri < len(rounds)-1) {
				fmt.Fprintln(os.Stderr, "\naoc: interrupted")
				return 130
			}
		}
	} else {
		for i, r := range todo {
			fmt.Fprintf(os.Stderr, "── run %d/%d: %s (%s) ──\n", i+1, len(todo), r.variant.Name, r.variant.Image)
			rep, err := executeRun(ctx, spec(r, *noBuild || i > 0))
			if errors.Is(err, errInterrupted) {
				fmt.Fprintln(os.Stderr, "\naoc: interrupted")
				return 130
			}
			if err != nil {
				return fail("ab: run %s: %v", r.name, err)
			}
			summary(r, rep)
			if !pause(i < len(todo)-1) {
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
	workloadLabel := "`" + p.Name + "`"
	if d := firstSentence(p.Description); d != "" {
		workloadLabel += " (" + d + ")"
	}
	conclusion := ""
	if b, err := os.ReadFile(filepath.Join(root, "conclusion.md")); err == nil {
		conclusion = strings.TrimSpace(stripConclusionHeader(string(b)))
	}
	in := findings.Input{Experiment: experiment, Cols: cols, Captures: abCaptures(cols, all), Threshold: cfg.Threshold, AppURL: ddapi.AppURLFromEnv(), ResultsDir: root,
		Workload: workloadLabel, ConfigDiff: cfg.configDiff(), Source: findings.DetectSource(cfg.Source), Conclusion: conclusion,
		Focus: cfg.Focus, Code: cfg.Code, Watch: cfg.Watch, Thresholds: cfg.Thresholds}
	res := findings.Build(in)
	findingsMD := findings.Markdown(res, in)
	os.WriteFile(filepath.Join(root, "findings.md"), []byte(findingsMD), 0o644)
	if b, err := json.MarshalIndent(res, "", "  "); err == nil {
		os.WriteFile(filepath.Join(root, "findings.json"), b, 0o644)
	} else {
		fmt.Fprintf(os.Stderr, "  findings.json: %v\n", err)
	}

	title := fmt.Sprintf("aoc A/B %s: %s vs %s", experiment, cfg.A.Name, cfg.B.Name)
	opts := notebook.Options{Site: *ddSite, AppURL: ddapi.AppURLFromEnv(), Experiment: experiment, Findings: res, Name: cfg.Focus}
	nb := notebook.ForAB(title, cols, opts)
	nbFile := filepath.Join(root, "notebook.json")
	nbURL, nbErr := createNotebook(context.Background(), nb, nbFile, ddapi.FromEnv(), !*mkNotebook)

	headline := res.Headline
	sum := abSummary{Experiment: experiment, Focus: cfg.Focus, Config: *config, Profile: cfg.Profile.String(), Started: started, Finished: time.Now(), Notebook: nbURL,
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
	} else {
		fmt.Fprintf(os.Stderr, "  ab.json: %v\n", err)
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
		if len(rep.Profiles) == 0 || rep.Agent.Commit == "" {
			// Reports written by an older aoc still have the pprof files
			// next to them: analyse them and read the build's commit.
			if caps, err := prof.LoadDir(r.dir); err == nil && len(caps) > 0 {
				if len(rep.Profiles) == 0 {
					rep.Profiles = prof.Summaries(prof.Overlapping(caps, rep.WindowStart, rep.WindowEnd), 40)
				}
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

// Host ports of the parallel stacks: side a on the compose defaults, side
// b one above.
const (
	intakeBasePort = 8282
	tcpBasePort    = 10516
)

func sideIndex(side string) int {
	if side == "b" {
		return 1
	}
	return 0
}

// withPort is the intake URL with its port replaced.
func withPort(intake string, port int) string {
	u, err := url.Parse(intake)
	if err != nil {
		return intake
	}
	u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(port))
	return u.String()
}

// byRound groups the planned runs by round, in plan order.
func byRound(runs []abRun) [][]abRun {
	var out [][]abRun
	idx := map[int]int{}
	for _, r := range runs {
		i, ok := idx[r.round]
		if !ok {
			i = len(out)
			idx[r.round] = i
			out = append(out, nil)
		}
		out[i] = append(out[i], r)
	}
	return out
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
