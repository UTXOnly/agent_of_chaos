package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/UTXOnly/agent_of_chaos/internal/gen"
	"github.com/UTXOnly/agent_of_chaos/internal/intake"
)

func init() {
	register(command{name: "profiles", short: "list the workloads in ./profiles, or show one resolved", run: runProfiles})
}

// Profile is one experiment shape: the log sources, the intake's fault
// timeline, the agent's settings and the measured window. profiles/README.md
// documents the format; `aoc run --set` and `aoc ab --set` change a
// generator setting without editing a file.
type Profile struct {
	Name        string               `yaml:"name,omitempty"`
	Description string               `yaml:"description,omitempty"`
	Duration    gen.Duration         `yaml:"duration"`
	Warmup      gen.Duration         `yaml:"warmup,omitempty"`
	Drain       gen.Duration         `yaml:"drain,omitempty"`
	Agent       ProfileAgent         `yaml:"agent,omitempty"`
	Generators  map[string]Generator `yaml:"generators"`
	Faults      []FaultStep          `yaml:"faults,omitempty"`
	Compose     []string             `yaml:"compose,omitempty"` // extra compose files stacked on docker-compose.yml
}

type ProfileAgent struct {
	Image string            `yaml:"image,omitempty"`
	Env   map[string]string `yaml:"env,omitempty"`
}

// FaultStep is one change to the intake's faults, at an offset from the
// start of the measured window.
type FaultStep struct {
	At    gen.Duration   `yaml:"at"`
	Set   map[string]any `yaml:"set,omitempty"`
	Clear bool           `yaml:"clear,omitempty"`
}

// Generator is one log source: a container running `aoc generate`. Format,
// output, replicas and labels are the harness's; every other key is an
// `aoc generate` flag (streams, rate, rotate-bytes, multiline-rate, mode…)
// and goes to the container as is.
type Generator struct {
	Format   string            // plain | json | both
	Output   string            // file | stdout
	Replicas int               // containers with these settings: <name>-01, <name>-02, …
	Labels   map[string]string // docker labels on the container; {n} is the replica number
	Settings map[string]any
}

// Keys a profile may not set: the harness wires them.
var reservedSettings = map[string]string{
	"name":    "the generator's name is its key under generators:",
	"log-dir": "files go under /var/log/agent-of-chaos/<name>, where the agent tails them",
	"intake":  "the intake is the stack's",
}

func (g *Generator) UnmarshalYAML(n *yaml.Node) error {
	var raw map[string]any
	if err := n.Decode(&raw); err != nil {
		return err
	}
	*g = Generator{Settings: map[string]any{}}
	known := generateSettings()
	for k, v := range raw {
		switch k {
		case "format":
			g.Format = fmt.Sprint(v)
		case "output":
			g.Output = fmt.Sprint(v)
		case "replicas":
			r, ok := v.(int)
			if !ok || r < 1 {
				return fmt.Errorf("replicas: %v is not a positive integer", v)
			}
			g.Replicas = r
		case "labels":
			m, ok := v.(map[string]any)
			if !ok {
				return errors.New("labels: expected a mapping")
			}
			g.Labels = map[string]string{}
			for lk, lv := range m {
				g.Labels[lk] = fmt.Sprint(lv)
			}
		default:
			if why, ok := reservedSettings[k]; ok {
				return fmt.Errorf("%s: %s", k, why)
			}
			if !known[k] {
				return fmt.Errorf("%s is not an aoc generate flag (aoc generate -h lists them)", k)
			}
			g.Settings[k] = v
		}
	}
	return nil
}

// MarshalYAML writes the generator the way profiles are written: format
// and output first, then the settings, labels last; one line when there
// are no labels.
func (g Generator) MarshalYAML() (any, error) {
	node := &yaml.Node{Kind: yaml.MappingNode}
	add := func(k string, v any) {
		var vn yaml.Node
		if err := vn.Encode(v); err != nil {
			return
		}
		node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, &vn)
	}
	add("format", g.Format)
	add("output", g.Output)
	if g.Replicas > 1 {
		add("replicas", g.Replicas)
	}
	for _, k := range []string{"streams", "rate"} {
		if v, ok := g.Settings[k]; ok {
			add(k, v)
		}
	}
	for _, k := range sortedKeys(g.Settings) {
		if k != "streams" && k != "rate" {
			add(k, g.Settings[k])
		}
	}
	if len(g.Labels) > 0 {
		add("labels", g.Labels)
	} else {
		node.Style = yaml.FlowStyle
	}
	return node, nil
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func loadProfile(path string) (*Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := parseProfile(b, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// parseProfile decodes a profile and fills in its defaults; name is used
// when the profile has none. Unknown keys are errors.
func parseProfile(b []byte, name string) (*Profile, error) {
	var p Profile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, err
	}
	if p.Name == "" {
		p.Name = name
	}
	if err := p.normalize(); err != nil {
		return nil, err
	}
	return &p, nil
}

// normalize fills the defaults in and validates what is left.
func (p *Profile) normalize() error {
	if p.Duration <= 0 {
		return errors.New("duration must be set")
	}
	if p.Drain <= 0 {
		p.Drain = gen.Duration(45 * time.Second)
	}
	if len(p.Generators) == 0 {
		return errors.New("generators: at least one is needed")
	}
	for name, g := range p.Generators {
		if name == "" || strings.ContainsAny(name, "/ \t.") {
			return fmt.Errorf("generators: %q is not a valid name (letters, digits, dashes)", name)
		}
		if g.Format == "" {
			g.Format = string(gen.FormatPlain)
		}
		if g.Output == "" {
			g.Output = string(gen.OutputFile)
		}
		if g.Replicas < 1 {
			g.Replicas = 1
		}
		if g.Settings == nil {
			g.Settings = map[string]any{}
		}
		switch gen.Format(g.Format) {
		case gen.FormatPlain, gen.FormatJSON, gen.FormatBoth:
		default:
			return fmt.Errorf("generators.%s.format: %q is not plain, json or both", name, g.Format)
		}
		switch gen.Output(g.Output) {
		case gen.OutputFile, gen.OutputStdout:
		default:
			return fmt.Errorf("generators.%s.output: %q is not file or stdout", name, g.Output)
		}
		p.Generators[name] = g
	}
	for i, st := range p.Faults {
		if !st.Clear {
			if err := checkFaults(st.Set); err != nil {
				return fmt.Errorf("faults[%d]: %w", i, err)
			}
		}
	}
	return nil
}

// set applies --set KEY=VALUE pairs. KEY is a generator setting for every
// generator (rate=20000), or <generator>.<setting> for one (json.rate=0);
// format, output and replicas work the same way.
func (p *Profile) set(pairs []string) error {
	kv, err := parseKV(pairs)
	if err != nil {
		return err
	}
	known := generateSettings()
	for _, key := range sortedStringKeys(kv) {
		raw := kv[key]
		target, setting := "", key
		if i := strings.IndexByte(key, '.'); i > 0 {
			target, setting = key[:i], key[i+1:]
		}
		if why, ok := reservedSettings[setting]; ok {
			return fmt.Errorf("%s: %s", key, why)
		}
		if setting != "format" && setting != "output" && setting != "replicas" && !known[setting] {
			return fmt.Errorf("%s: %s is not an aoc generate flag (aoc generate -h lists them)", key, setting)
		}
		var value any
		if yaml.Unmarshal([]byte(raw), &value) != nil {
			value = raw
		}
		applied := false
		for _, name := range p.generatorNames() {
			if target != "" && target != name {
				continue
			}
			g := p.Generators[name]
			switch setting {
			case "format":
				g.Format = raw
			case "output":
				g.Output = raw
			case "replicas":
				n, err := strconv.Atoi(raw)
				if err != nil || n < 1 {
					return fmt.Errorf("%s: %q is not a positive integer", key, raw)
				}
				g.Replicas = n
			default:
				g.Settings[setting] = value
			}
			p.Generators[name] = g
			applied = true
		}
		if !applied {
			return fmt.Errorf("%s: no generator named %q (the profile has %s)", key, target, strings.Join(p.generatorNames(), ", "))
		}
	}
	return p.normalize()
}

func (p *Profile) generatorNames() []string {
	names := make([]string, 0, len(p.Generators))
	for n := range p.Generators {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// service is one generator container of a run.
type service struct {
	Name    string // compose service: gen-<generator>[-NN]
	GenName string // AOC_NAME: <generator>[-NN]
	Replica int    // 1-based; 0 for a single container
	Gen     Generator
}

// services expands replicas into the containers a run starts.
func (p *Profile) services() []service {
	var out []service
	for _, name := range p.generatorNames() {
		g := p.Generators[name]
		if g.Replicas <= 1 {
			out = append(out, service{Name: "gen-" + name, GenName: name, Gen: g})
			continue
		}
		for i := 1; i <= g.Replicas; i++ {
			gn := fmt.Sprintf("%s-%02d", name, i)
			out = append(out, service{Name: "gen-" + gn, GenName: gn, Replica: i, Gen: g})
		}
	}
	return out
}

// budgeted reports whether a generator stops after a fixed number of
// records (max-lines), so the window has to be open before it starts.
func (p *Profile) budgeted() bool {
	for _, g := range p.Generators {
		if n, err := strconv.ParseInt(yamlScalar(g.Settings["max-lines"]), 10, 64); err == nil && n > 0 {
			return true
		}
	}
	return false
}

func (p *Profile) serviceNames() []string {
	var names []string
	for _, s := range p.services() {
		names = append(names, s.Name)
	}
	return names
}

// summary is the one-line description of the generators for listings and
// the plan: "json: 8 streams, 5000 lines/s, json files; plain: …".
func (p *Profile) summary() string {
	var parts []string
	for _, name := range p.generatorNames() {
		g := p.Generators[name]
		s := name
		if g.Replicas > 1 {
			s += fmt.Sprintf(" ×%d", g.Replicas)
		}
		var what []string
		if mode, ok := g.Settings["mode"]; ok && mode != gen.ModeSteady {
			m := fmt.Sprint(mode)
			if sc, ok := g.Settings["scenario"]; ok {
				m += " " + fmt.Sprint(sc)
			}
			what = append(what, m)
		}
		if st, ok := g.Settings["streams"]; ok {
			what = append(what, fmt.Sprintf("%v streams", st))
		}
		if r, ok := g.Settings["rate"]; ok {
			if fmt.Sprint(r) == "0" {
				what = append(what, "flat out")
			} else {
				what = append(what, fmt.Sprintf("%v lines/s", r))
			}
		}
		where := g.Format + " files"
		if g.Output == string(gen.OutputStdout) {
			where = g.Format + " on stdout"
		}
		parts = append(parts, s+": "+strings.Join(append(what, where), ", "))
	}
	return strings.Join(parts, "; ")
}

// Compose rendering. The base docker-compose.yml has the intake and the
// agent; every generator container comes from the profile.

const (
	aocImage   = "agent-of-chaos:latest"
	logRoot    = "/var/log/agent-of-chaos"
	startDelay = "20s" // the agent's docker launcher tails a new container from the moment it notices it
)

type composeService struct {
	Image         string                       `yaml:"image,omitempty"`
	PullPolicy    string                       `yaml:"pull_policy,omitempty"`
	ContainerName string                       `yaml:"container_name,omitempty"`
	Restart       string                       `yaml:"restart,omitempty"`
	StopGrace     string                       `yaml:"stop_grace_period,omitempty"`
	DependsOn     map[string]map[string]string `yaml:"depends_on,omitempty"`
	Volumes       []string                     `yaml:"volumes,omitempty"`
	Environment   map[string]string            `yaml:"environment,omitempty"`
	Labels        map[string]string            `yaml:"labels,omitempty"`
}

// overrideCompose renders the compose override for one run: the agent's
// image and settings, and one service per generator container. It returns
// the YAML and the generator service names.
func overrideCompose(p *Profile, agent agentSpec, stack string) (string, []string) {
	services := map[string]composeService{}
	env := map[string]string{}
	for k, v := range p.Agent.Env {
		env[k] = v
	}
	for k, v := range agent.Env {
		env[k] = v
	}
	if agent.Image != "" || agent.Pull != "" || len(env) > 0 {
		services["datadog-agent"] = composeService{Image: agent.Image, PullPolicy: agent.Pull, Environment: env}
	}
	var names []string
	for _, s := range p.services() {
		names = append(names, s.Name)
		services[s.Name] = s.compose(stack)
	}
	out, _ := yaml.Marshal(map[string]any{"services": services})
	return "# generated by aoc run for this run; the profile is the source\n" + string(out), names
}

func (s service) compose(stack string) composeService {
	g := s.Gen
	env := map[string]string{
		"AOC_INTAKE": "http://intake:8282",
		"AOC_NAME":   s.GenName,
		"AOC_FORMAT": g.Format,
		"AOC_OUTPUT": g.Output,
	}
	for k, v := range g.Settings {
		env[envKey(k)] = yamlScalar(v)
	}
	cs := composeService{
		Image:         aocImage,
		ContainerName: stack + "-" + s.Name,
		// Not unless-stopped: a generator with a budget (max-lines) exits 0
		// when done, and a restart rewrites the same seqs and re-bases the
		// intake's ledger. Chaos crashes are in-process.
		Restart:     "on-failure",
		StopGrace:   "20s",
		DependsOn:   map[string]map[string]string{"intake": {"condition": "service_healthy"}},
		Environment: env,
	}
	if g.Output == string(gen.OutputStdout) {
		if _, ok := env["AOC_START_DELAY"]; !ok {
			env["AOC_START_DELAY"] = startDelay
		}
		rules := ""
		if g.Format != string(gen.FormatJSON) {
			rules = `,"log_processing_rules":[{"type":"multi_line","name":"timestamp_start","pattern":"^\\d{4}-\\d{2}-\\d{2}T\\d{2}:\\d{2}:\\d{2}"}]`
		}
		cs.Labels = map[string]string{"com.datadoghq.ad.logs": fmt.Sprintf(`[{"source":"agent-of-chaos","service":"%s"%s}]`, s.GenName, rules)}
	} else {
		env["AOC_LOG_DIR"] = logRoot + "/" + s.GenName
		cs.Volumes = []string{"aoc-logs:" + logRoot}
	}
	for k, v := range g.Labels {
		if cs.Labels == nil {
			cs.Labels = map[string]string{}
		}
		cs.Labels[k] = strings.ReplaceAll(v, "{n}", fmt.Sprintf("%02d", s.Replica))
	}
	return cs
}

// envKey turns a setting into the generator's env var: "rotate-bytes" →
// AOC_ROTATE_BYTES.
func envKey(k string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(k, "-", "_"))
}

func yamlScalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	default:
		return fmt.Sprint(v)
	}
}

// Faults from the command line.

// parseFaults turns --fault K=V values into the intake's fault fields
// (latency_ms=300, drop_rate=0.05, outage=true), rejecting unknown fields
// and out-of-range values before anything starts.
func parseFaults(list []string) (map[string]any, error) {
	kv, err := parseKV(list)
	if err != nil || len(kv) == 0 {
		return nil, err
	}
	set := make(map[string]any, len(kv))
	for k, v := range kv {
		var val any
		if json.Unmarshal([]byte(v), &val) != nil {
			val = v // free text: note=…
		}
		set[k] = val
	}
	return set, checkFaults(set)
}

func checkFaults(set map[string]any) error {
	body, _ := json.Marshal(set)
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var f intake.Faults
	if err := dec.Decode(&f); err != nil {
		return fmt.Errorf("%v (fields: latency_ms, jitter_ms, error_rate, error_status, drop_rate, outage, read_bps, flap_on_ms, flap_off_ms, note)", err)
	}
	return f.Validate()
}

// describeFaults is the one-line summary of a fault set.
func describeFaults(set map[string]any) string {
	body, _ := json.Marshal(set)
	var f intake.Faults
	_ = json.Unmarshal(body, &f)
	return f.String()
}

// faultsSummary describes a timeline: "none", "latency 300ms (whole
// window)" or "2 steps: HTTP 503 100% at 45s, cleared at 1m30s".
func (p *Profile) faultsSummary() string {
	switch len(p.Faults) {
	case 0:
		return "none"
	case 1:
		if p.Faults[0].At == 0 && !p.Faults[0].Clear {
			return describeFaults(p.Faults[0].Set) + " (whole window)"
		}
	}
	var parts []string
	for _, st := range p.Faults {
		if st.Clear {
			parts = append(parts, fmt.Sprintf("cleared at %s", st.At))
		} else {
			parts = append(parts, fmt.Sprintf("%s at %s", describeFaults(st.Set), st.At))
		}
	}
	return fmt.Sprintf("%d steps: %s", len(p.Faults), strings.Join(parts, ", "))
}

// faultsBeyondWindow lists the fault steps that fall at or after the end of
// the measured window: they never run, which matters when --duration cut a
// timeline short (a 2 min look at intake-outage never reaches its 429 phase).
func (p *Profile) faultsBeyondWindow() []string {
	var out []string
	for _, st := range p.Faults {
		if st.At < p.Duration || st.Clear { // the run clears faults at the window's end anyway
			continue
		}
		out = append(out, fmt.Sprintf("%s at %s", describeFaults(st.Set), st.At))
	}
	return out
}

// lastFaultStep is the offset just past the last `set` step of the timeline,
// the shortest window that applies every fault.
func (p *Profile) lastFaultStep() gen.Duration {
	var last gen.Duration
	for _, st := range p.Faults {
		if !st.Clear && st.At >= last {
			last = st.At + gen.Duration(time.Second)
		}
	}
	return last
}

// runOverhead is what compose up, the agent's health check, profile
// collection and the report add to one run, on a warm host with images
// present. A rough figure for planning, not a measurement.
const runOverhead = 90 * time.Second

// wallTime estimates one run end to end: warmup, window, drain and overhead.
func (p *Profile) wallTime() time.Duration {
	return p.Warmup.D() + p.Duration.D() + p.Drain.D() + runOverhead
}

// planWarnings are the things worth saying before a run starts: fault steps
// the window will not reach.
func (p *Profile) planWarnings() []string {
	var w []string
	if beyond := p.faultsBeyondWindow(); len(beyond) > 0 {
		w = append(w, fmt.Sprintf("the %s window ends before %s; --duration %s or longer runs the whole timeline",
			p.Duration, strings.Join(beyond, ", "), p.lastFaultStep()))
	}
	return w
}

// profilePath resolves a workload name to profiles/<name>.yaml; a path
// (anything with a separator or a .yaml suffix) is used as it is.
func profilePath(v string) string {
	if strings.ContainsAny(v, `/\`) || strings.HasSuffix(v, ".yaml") || strings.HasSuffix(v, ".yml") {
		return v
	}
	return filepath.Join("profiles", v+".yaml")
}

func runProfiles(args []string) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Println("Usage:\n  aoc profiles [DIR]        list the workloads (default ./profiles)\n  aoc profiles show NAME    print one with its defaults filled in")
		return 0
	}
	if len(args) > 1 && args[0] == "show" {
		p, err := loadProfile(profilePath(args[1]))
		if err != nil {
			return fail("%v", err)
		}
		out, _ := yaml.Marshal(p)
		os.Stdout.Write(out)
		return 0
	}
	dir := "profiles"
	if len(args) > 0 {
		dir = args[0]
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if len(matches) == 0 {
		return fail("no profiles in %s (run from the repo root, or pass a directory)", dir)
	}
	fmt.Printf("\nWorkloads in %s (aoc ab --workload NAME, aoc run --profile PATH):\n\n", dir)
	for _, m := range matches {
		p, err := loadProfile(m)
		if err != nil {
			fmt.Printf("  %-20s invalid: %v\n\n", filepath.Base(m), err)
			continue
		}
		fmt.Printf("  %-20s %s\n", p.Name, wrap(p.Description, 90, 23))
		fmt.Printf("  %-20s window %s, warmup %s, drain %s\n", "", p.Duration, p.Warmup, p.Drain)
		fmt.Printf("  %-20s generators: %s\n", "", p.summary())
		fmt.Printf("  %-20s faults: %s\n\n", "", p.faultsSummary())
	}
	fmt.Println("Change a setting without editing a file: --set rate=20000, --set json.streams=16, --fault latency_ms=300.")
	return 0
}

func wrap(s string, width, indent int) string {
	words := strings.Fields(s)
	var lines []string
	var cur string
	for _, w := range words {
		if len(cur)+len(w)+1 > width && cur != "" {
			lines = append(lines, cur)
			cur = w
			continue
		}
		if cur == "" {
			cur = w
		} else {
			cur += " " + w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return strings.Join(lines, "\n"+strings.Repeat(" ", indent))
}
