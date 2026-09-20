package gen

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/UTXOnly/agent_of_chaos/internal/fmtutil"
)

// Conductor owns the stream pool and drives it through a scenario.
type Conductor struct {
	cfg *Config
	pc  *PhaseConfig
	sc  *Scenario

	mu     sync.Mutex
	states map[string]*streamState
	active map[string]*Stream
	pool   []string
	hostN  int

	phase     atomic.Pointer[string]
	startedAt time.Time
	stopCh    chan struct{}
	stopOnce  sync.Once
	scDone    chan struct{}
	out       *os.File // status + events (stderr)

	push *pusher
	proc procSampler
}

// New validates cfg, resolves the scenario and prepares (but does not start)
// the stream pool.
func New(cfg Config) (*Conductor, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	c := &Conductor{
		cfg: &cfg, pc: &PhaseConfig{},
		states: map[string]*streamState{}, active: map[string]*Stream{},
		stopCh: make(chan struct{}), scDone: make(chan struct{}), out: os.Stderr,
	}
	c.setPhase("—")
	sc, err := c.buildScenario()
	if err != nil {
		return nil, err
	}
	if err := sc.Validate(); err != nil {
		return nil, err
	}
	c.sc = sc

	// Stream name pool: the service names, shuffled (seeded when
	// deterministic), then extended with numeric suffixes for large counts.
	c.pool = append([]string(nil), ServicePool...)
	var rng *rand.Rand
	if cfg.Deterministic {
		rng = rand.New(rand.NewPCG(cfg.Seed, 0x9e3779b97f4a7c15))
	} else {
		rng = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
	rng.Shuffle(len(c.pool), func(i, j int) { c.pool[i], c.pool[j] = c.pool[j], c.pool[i] })
	need := c.maxStreams()
	for k := 2; len(c.pool) < need; k++ {
		for _, s := range ServicePool {
			c.pool = append(c.pool, fmt.Sprintf("%s-%d", s, k))
			if len(c.pool) >= need {
				break
			}
		}
	}
	c.pc.SetMultilineRate(cfg.MultilineRate)
	c.pc.SetWideLineRate(cfg.WideLineRate)
	c.pc.SetBurstSize(cfg.BurstSize)
	c.pc.SetBurstInterval(cfg.BurstInterval)
	c.pc.SetAggregateRate(cfg.Rate)
	if cfg.IntakeURL != "" {
		c.push = newPusher(cfg.IntakeURL, c.event)
	}
	return c, nil
}

// Scenario returns the resolved scenario (built-in, file, or mode-derived).
func (c *Conductor) Scenario() *Scenario { return c.sc }

func (c *Conductor) maxStreams() int {
	n := c.cfg.Streams
	for _, p := range c.sc.Phases {
		if p.Streams != nil && *p.Streams > n {
			n = *p.Streams
		}
	}
	return n
}

func (c *Conductor) buildScenario() (*Scenario, error) {
	switch c.cfg.Mode {
	case ModeSteady, ModeChaos:
		return steadyScenario(c.cfg), nil
	case ModeRamp:
		return rampScenario(c.cfg), nil
	case ModeSpike:
		return spikeScenario(c.cfg), nil
	case ModePulse:
		return pulseScenario(c.cfg), nil
	case ModeScenario:
		return LoadScenario(c.cfg.Scenario)
	}
	return nil, fmt.Errorf("unknown mode %q", c.cfg.Mode)
}

// LoadScenario resolves a built-in name or a YAML/JSON file path.
func LoadScenario(nameOrPath string) (*Scenario, error) {
	if sc, ok := Builtin[nameOrPath]; ok {
		cp := *sc
		return &cp, nil
	}
	data, err := os.ReadFile(nameOrPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("scenario %q: not a built-in (%s) and no such file", nameOrPath, strings.Join(BuiltinNames(), ", "))
		}
		return nil, err
	}
	var sc Scenario
	if strings.HasSuffix(nameOrPath, ".json") {
		err = json.Unmarshal(data, &sc)
	} else {
		err = yaml.Unmarshal(data, &sc)
	}
	if err != nil {
		return nil, fmt.Errorf("scenario %s: %w", nameOrPath, err)
	}
	if sc.Name == "" {
		sc.Name = strings.TrimSuffix(filepath.Base(nameOrPath), filepath.Ext(nameOrPath))
	}
	return &sc, nil
}

func (c *Conductor) setPhase(name string) { c.phase.Store(&name) }
func (c *Conductor) Phase() string        { return *c.phase.Load() }

func (c *Conductor) event(msg string) {
	if c.cfg.Quiet {
		return
	}
	fmt.Fprintf(c.out, "  %s  %s\n", time.Now().Format("15:04:05"), msg)
}

// Stop asks Run to wind down. Safe to call more than once.
func (c *Conductor) Stop() { c.stopOnce.Do(func() { close(c.stopCh) }) }

// Run executes the scenario until it completes, the duration/line cap is
// hit, Stop is called or ctx is cancelled. It returns the final totals.
func (c *Conductor) Run(ctx context.Context) (*Report, error) {
	cfg := c.cfg
	if cfg.Output == OutputFile {
		if err := ensureDir(cfg.LogDir); err != nil {
			return nil, fmt.Errorf("log dir: %w", err)
		}
	}
	c.startedAt = time.Now()
	c.banner()

	// Per-stream quotas make --max-lines byte-exact in steady mode; other
	// modes enforce it as a global cap from the stats loop.
	if cfg.MaxLines > 0 && (cfg.Mode == ModeSteady || cfg.Mode == ModeChaos) {
		n := cfg.Streams
		base, rem := cfg.MaxLines/int64(n), cfg.MaxLines%int64(n)
		for i := 0; i < n; i++ {
			st := c.state(c.pool[i])
			st.quota = base
			if int64(i) < rem {
				st.quota++
			}
		}
	}

	stopStats := make(chan struct{})
	statsDone := make(chan struct{})
	go c.statsLoop(stopStats, statsDone)

	go c.runScenario()
	var chaosDone chan struct{}
	if cfg.Mode == ModeChaos {
		chaosDone = make(chan struct{})
		go c.chaosLoop(chaosDone)
	}

	var durC <-chan time.Time
	if cfg.Duration > 0 && cfg.Mode != ModeSteady {
		durC = time.After(cfg.Duration)
	}
	quotaC := c.quotaWatcher()

	reason := ""
	select {
	case <-ctx.Done():
		reason = "interrupted"
	case <-c.stopCh:
		reason = "stopped"
	case <-durC:
		reason = "duration reached"
	case <-c.scDone:
		reason = "scenario complete"
	case <-quotaC:
		reason = "line cap reached"
	}
	c.Stop()
	c.event("[shutdown] " + reason)
	if chaosDone != nil {
		<-chaosDone
	}
	<-c.scDone
	c.stopAllStreams()
	close(stopStats)
	<-statsDone

	rep := c.report(true)
	c.summary(rep)
	if c.push != nil {
		c.push.send(rep, true)
	}
	return rep, nil
}

func (c *Conductor) banner() {
	cfg := c.cfg
	if cfg.Quiet {
		return
	}
	fmt.Fprintf(c.out, "\nagent_of_chaos generate  name=%s  mode=%s  scenario=%s\n", cfg.Name, cfg.Mode, c.sc.Name)
	if cfg.Output == OutputFile {
		fmt.Fprintf(c.out, "  output=%s  format=%s  rotate=%s×%d (%s)\n", cfg.LogDir, cfg.Format, fmtutil.Bytes(cfg.RotateBytes), cfg.RotateKeep, cfg.RotateMode)
	} else {
		fmt.Fprintf(c.out, "  output=stdout  format=%s\n", cfg.Format)
	}
	rate := "unlimited"
	if cfg.Rate > 0 {
		rate = fmtutil.Rate(cfg.Rate)
	}
	fmt.Fprintf(c.out, "  streams=%d  rate=%s  multiline=%.0f%%  wide=%.0f%%×%s  burst=%d/%s  deterministic=%v seed=%d\n",
		cfg.Streams, rate, cfg.MultilineRate*100, cfg.WideLineRate*100, fmtutil.Bytes(int64(cfg.WideLineBytes)),
		cfg.BurstSize, fmtutil.Duration(cfg.BurstInterval), cfg.Deterministic, cfg.Seed)
	if cfg.Mode == ModeScenario || cfg.Mode == ModePulse {
		fmt.Fprintf(c.out, "  phases=%d  loop=%v  cycle=%s  speed=%gx\n", len(c.sc.Phases), c.sc.Loop,
			fmtutil.Duration(time.Duration(float64(c.sc.TotalDuration())/cfg.ScenarioSpeed)), cfg.ScenarioSpeed)
	}
	if cfg.IntakeURL != "" {
		fmt.Fprintf(c.out, "  stats → %s\n", cfg.IntakeURL)
	}
	if cfg.Duration > 0 {
		fmt.Fprintf(c.out, "  duration=%s", fmtutil.Duration(cfg.Duration))
		if cfg.MaxLines > 0 {
			fmt.Fprintf(c.out, "  max-lines=%s", fmtutil.Int(cfg.MaxLines))
		}
		fmt.Fprintln(c.out)
	} else if cfg.MaxLines > 0 {
		fmt.Fprintf(c.out, "  max-lines=%s\n", fmtutil.Int(cfg.MaxLines))
	}
	fmt.Fprintln(c.out)
}

// ── stream pool ──────────────────────────────────────────────────────────────

func (c *Conductor) state(name string) *streamState {
	if st, ok := c.states[name]; ok {
		return st
	}
	c.hostN++
	short := name
	if len(short) > 8 {
		short = short[:8]
	}
	st := newStreamState(c.cfg, name, fmt.Sprintf("%s-%02d", short, c.hostN))
	c.states[name] = st
	return st
}

func (c *Conductor) addStream(name string) {
	c.mu.Lock()
	if _, ok := c.active[name]; ok {
		c.mu.Unlock()
		return
	}
	st := c.state(name)
	if st.quotaReached() {
		c.mu.Unlock()
		return
	}
	s, err := newStream(c.cfg, c.pc, st, c.event)
	if err != nil {
		c.mu.Unlock()
		c.event(fmt.Sprintf("[!] cannot start %s: %v", name, err))
		return
	}
	c.active[name] = s
	n := len(c.active)
	c.pc.activeStreams.Store(int64(n))
	s.start()
	c.mu.Unlock()
	c.event(fmt.Sprintf("[+] %-20s (%d streams, %d files)", name, n, n*c.cfg.FilesPerStream()))
}

func (c *Conductor) removeStream(name string) {
	c.mu.Lock()
	s, ok := c.active[name]
	if ok {
		delete(c.active, name)
	}
	n := len(c.active)
	c.pc.activeStreams.Store(int64(max(n, 1)))
	c.mu.Unlock()
	if !ok {
		return
	}
	s.stopAndWait()
	c.event(fmt.Sprintf("[-] %-20s (%d streams)", name, n))
}

func (c *Conductor) activeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.active)
}

// applyStreamCount adds streams from the pool (in pool order) or removes the
// oldest ones until exactly target are running.
func (c *Conductor) applyStreamCount(target int) {
	c.mu.Lock()
	cur := len(c.active)
	var add, remove []string
	if target > cur {
		for _, n := range c.pool {
			if len(add) == target-cur {
				break
			}
			if _, ok := c.active[n]; !ok && !c.state(n).quotaReached() {
				add = append(add, n)
			}
		}
	} else if target < cur {
		type aged struct {
			name string
			t    time.Time
		}
		var all []aged
		for n, s := range c.active {
			all = append(all, aged{n, s.startedAt})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
		for _, a := range all[:cur-target] {
			remove = append(remove, a.name)
		}
	}
	c.mu.Unlock()
	for _, n := range add {
		c.addStream(n)
	}
	for _, n := range remove {
		c.removeStream(n)
	}
}

func (c *Conductor) stopAllStreams() {
	c.mu.Lock()
	streams := make([]*Stream, 0, len(c.active))
	for _, s := range c.active {
		streams = append(streams, s)
	}
	c.active = map[string]*Stream{}
	c.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range streams {
		wg.Add(1)
		go func(s *Stream) { defer wg.Done(); s.stopAndWait() }(s)
	}
	wg.Wait()
}

// quotaWatcher fires once every quota-bearing stream has finished, or the
// global line cap is reached.
func (c *Conductor) quotaWatcher() <-chan struct{} {
	ch := make(chan struct{})
	if c.cfg.MaxLines <= 0 {
		return ch
	}
	go func() {
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-c.stopCh:
				return
			case <-t.C:
			}
			c.mu.Lock()
			total := int64(0)
			pending := false
			for _, st := range c.states {
				total += st.stats.Records.Load()
				if st.quota > 0 && !st.quotaReached() {
					pending = true
				}
			}
			started := len(c.states) > 0
			c.mu.Unlock()
			if (started && !pending && c.cfg.Mode == ModeSteady) || total >= c.cfg.MaxLines {
				close(ch)
				return
			}
		}
	}()
	return ch
}

// ── scenario runner ──────────────────────────────────────────────────────────

func (c *Conductor) runScenario() {
	defer close(c.scDone)
	sc, speed := c.sc, c.cfg.ScenarioSpeed
	for cycle := 1; ; cycle++ {
		label := ""
		if sc.Loop {
			label = fmt.Sprintf(" (cycle %d)", cycle)
		}
		c.event(fmt.Sprintf("[scenario] %s%s — %d phases", sc.Name, label, len(sc.Phases)))
		for i := range sc.Phases {
			if c.stopped() {
				return
			}
			c.executePhase(&sc.Phases[i], speed)
		}
		if !sc.Loop || c.stopped() {
			return
		}
	}
}

func (c *Conductor) stopped() bool {
	select {
	case <-c.stopCh:
		return true
	default:
		return false
	}
}

func (c *Conductor) executePhase(p *Phase, speed float64) {
	duration := time.Duration(float64(p.Duration.D()) / speed)
	trans := time.Duration(float64(p.TransitionSec.D()) / speed)
	c.setPhase(p.Name)

	var desc []string
	if p.Streams != nil {
		desc = append(desc, fmt.Sprintf("streams=%d", *p.Streams))
	}
	if p.Rate != nil {
		if *p.Rate <= 0 {
			desc = append(desc, "rate=unlimited")
		} else {
			desc = append(desc, "rate="+fmtutil.Rate(*p.Rate))
		}
	}
	if p.BurstSize != nil {
		desc = append(desc, fmt.Sprintf("burst=%d", *p.BurstSize))
	}
	if p.BurstInterval != nil {
		desc = append(desc, "burst_interval="+fmtutil.Duration(p.BurstInterval.D()))
	}
	if p.MultilineRate != nil {
		desc = append(desc, fmt.Sprintf("multiline=%.0f%%", *p.MultilineRate*100))
	}
	if p.WideLineRate != nil {
		desc = append(desc, fmt.Sprintf("wide=%.0f%%", *p.WideLineRate*100))
	}
	dur := "∞"
	if duration > 0 {
		dur = fmtutil.Duration(duration)
	}
	if p.Transition == "gradual" && trans > 0 {
		desc = append(desc, "gradual over "+fmtutil.Duration(trans))
	}
	c.event(fmt.Sprintf("[phase:%s] duration=%s  %s", p.Name, dur, strings.Join(desc, "  ")))

	if p.Transition == "gradual" && trans > 0 {
		go c.gradual(p, trans)
	} else {
		c.applyPhase(p)
	}

	if duration <= 0 {
		<-c.stopCh
		return
	}
	select {
	case <-c.stopCh:
	case <-time.After(duration):
	}
}

func (c *Conductor) applyPhase(p *Phase) {
	if p.Rate != nil {
		c.pc.SetAggregateRate(*p.Rate)
	}
	if p.BurstSize != nil {
		c.pc.SetBurstSize(*p.BurstSize)
	}
	if p.BurstInterval != nil {
		c.pc.SetBurstInterval(p.BurstInterval.D())
	}
	if p.MultilineRate != nil {
		c.pc.SetMultilineRate(*p.MultilineRate)
	}
	if p.WideLineRate != nil {
		c.pc.SetWideLineRate(*p.WideLineRate)
	}
	if p.Streams != nil {
		c.applyStreamCount(*p.Streams)
	}
}

// gradual interpolates every set knob linearly over trans and spreads stream
// additions/removals evenly across the window, one step per second.
func (c *Conductor) gradual(p *Phase, trans time.Duration) {
	start := c.pc.Snapshot()
	startStreams := c.activeCount()
	steps := int(math.Max(2, trans.Seconds()))
	interval := trans / time.Duration(steps)
	lerp := func(a, b float64, t float64) float64 { return a + (b-a)*t }
	for i := 1; i <= steps; i++ {
		if c.stopped() {
			return
		}
		t := float64(i) / float64(steps)
		if p.Rate != nil {
			c.pc.SetAggregateRate(lerp(start.Rate, *p.Rate, t))
		}
		if p.BurstSize != nil {
			c.pc.SetBurstSize(int(math.Round(lerp(float64(start.BurstSize), float64(*p.BurstSize), t))))
		}
		if p.BurstInterval != nil {
			c.pc.SetBurstInterval(time.Duration(lerp(float64(start.BurstInterval), float64(p.BurstInterval.D()), t)))
		}
		if p.MultilineRate != nil {
			c.pc.SetMultilineRate(lerp(start.MultilineRate, *p.MultilineRate, t))
		}
		if p.WideLineRate != nil {
			c.pc.SetWideLineRate(lerp(start.WideLineRate, *p.WideLineRate, t))
		}
		if p.Streams != nil {
			c.applyStreamCount(int(math.Round(lerp(float64(startStreams), float64(*p.Streams), t))))
		}
		select {
		case <-c.stopCh:
			return
		case <-time.After(interval):
		}
	}
	c.applyPhase(p) // snap to exact targets
}

// chaosLoop kills random streams (closing their files) and restarts them
// after ChaosRestartDelay, resuming their sequence numbers.
func (c *Conductor) chaosLoop(done chan struct{}) {
	defer close(done)
	rng := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	pPerSec := c.cfg.ChaosCrashRate / 60
	var wg sync.WaitGroup
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.stopCh:
			wg.Wait()
			return
		case <-t.C:
		}
		c.mu.Lock()
		names := make([]string, 0, len(c.active))
		for n := range c.active {
			names = append(names, n)
		}
		c.mu.Unlock()
		for _, n := range names {
			if rng.Float64() >= pPerSec {
				continue
			}
			c.event(fmt.Sprintf("[chaos] crashing %s — restart in %s", n, fmtutil.Duration(c.cfg.ChaosRestartDelay)))
			c.removeStream(n)
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				select {
				case <-c.stopCh:
				case <-time.After(c.cfg.ChaosRestartDelay):
					if !c.stopped() {
						c.addStream(name)
					}
				}
			}(n)
		}
	}
}

// ── stats ────────────────────────────────────────────────────────────────────

func (c *Conductor) statsLoop(stop, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(c.cfg.StatsInterval)
	defer t.Stop()
	var prev Totals
	prevT := time.Now()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		rep := c.report(false)
		now := time.Now()
		dt := now.Sub(prevT).Seconds()
		if dt > 0 && !c.cfg.Quiet {
			c.statusLine(rep, float64(rep.Totals.Records-prev.Records)/dt, float64(rep.Totals.Bytes-prev.Bytes)/dt)
		}
		prev, prevT = rep.Totals, now
		if c.push != nil {
			c.push.send(rep, false)
		}
	}
}

func (c *Conductor) statusLine(rep *Report, recPerSec, bytesPerSec float64) {
	target := "unlimited"
	if rep.Knobs.Rate > 0 {
		target = fmtutil.Rate(rep.Knobs.Rate)
	}
	fmt.Fprintf(c.out, "  %s  %-16s streams=%-3d target=%-10s rate=%-10s %-11s total=%-12s rotations=%-4d cpu=%3.0f%%\n",
		time.Now().Format("15:04:05"), rep.Phase, rep.ActiveStreams, target,
		fmtutil.Rate(recPerSec), fmtutil.BytesF(bytesPerSec)+"/s",
		fmtutil.Int(rep.Totals.Records), rep.Totals.Rotations, rep.CPUPercent)
}

func (c *Conductor) summary(rep *Report) {
	if c.cfg.Quiet {
		return
	}
	el := rep.TS.Sub(rep.StartedAt)
	avg := 0.0
	if el > 0 {
		avg = float64(rep.Totals.Records) / el.Seconds()
	}
	fmt.Fprintf(c.out, "\n  Done.  records=%s  lines=%s  bytes=%s  rotations=%d  elapsed=%s  avg=%s",
		fmtutil.Int(rep.Totals.Records), fmtutil.Int(rep.Totals.Lines), fmtutil.Bytes(rep.Totals.Bytes),
		rep.Totals.Rotations, fmtutil.Duration(el), fmtutil.Rate(avg))
	if rep.Totals.Errors > 0 {
		fmt.Fprintf(c.out, "  WRITE ERRORS=%d", rep.Totals.Errors)
	}
	if c.cfg.Output == OutputFile {
		fmt.Fprintf(c.out, "\n  files in: %s", c.cfg.LogDir)
	}
	fmt.Fprint(c.out, "\n\n")
}
