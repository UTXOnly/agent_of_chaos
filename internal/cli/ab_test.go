package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadABConfigDefaults(t *testing.T) {
	prof := writeTemp(t, "baseline.yaml", "duration: 2m\ngenerators:\n  gen-plain: { streams: 4, rate: 100 }\n")
	cfgPath := writeTemp(t, "aoc.yaml", "profile: "+prof+"\nb:\n  image: datadog/agent-dev:my-branch-py3\n  pull: never\n")
	c, src, err := loadABConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(src) == 0 {
		t.Error("source not returned")
	}
	if c.A.Image != defaultAImage || c.A.Name != "a" || c.A.Pull != "always" {
		t.Errorf("a defaults: %+v", c.A)
	}
	if c.B.Name != "b" || c.B.Pull != "never" {
		t.Errorf("b: %+v", c.B)
	}
	if c.Runs != 1 || c.Pause.D() != 10*time.Second || c.Results != "results" || c.Threshold != 10 {
		t.Errorf("defaults: runs=%d pause=%s results=%s threshold=%v", c.Runs, c.Pause, c.Results, c.Threshold)
	}
	p, psrc, err := c.workload()
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "baseline" || p.Duration.D() != 2*time.Minute || p.Drain.D() != 45*time.Second || len(psrc) == 0 {
		t.Errorf("workload: %+v", p)
	}
	runs := c.plan("ab-" + p.Name)
	if len(runs) != 2 || runs[0].name != "ab-baseline-a" || runs[1].name != "ab-baseline-b" || runs[1].dir != filepath.Join("results", "ab-baseline", "b") {
		t.Errorf("plan: %+v", runs)
	}
}

func TestLoadABConfigInlineProfileAndOverrides(t *testing.T) {
	cfgPath := writeTemp(t, "aoc.yaml", `
name: tag filters
profile:
  duration: 1m
  warmup: 5s
  agent:
    env: { DD_LOG_LEVEL: debug }
  generators:
    gen-plain: { streams: 2, rate: 10 }
duration: 90s
runs: 2
env: { DD_LOGS_CONFIG_CONTAINER_COLLECT_ALL: "false" }
a: { name: release }
b: { name: candidate, image: datadog/agent-dev:x, env: { DD_LOGS_CONFIG_TAG_FILTERS: '{"exclude":["env:*"]}' } }
`)
	c, _, err := loadABConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	p, src, err := c.workload()
	if err != nil {
		t.Fatal(err)
	}
	if c.Profile.String() != "inline" || p.Name != "inline" || p.Duration.D() != 90*time.Second || p.Warmup.D() != 5*time.Second {
		t.Errorf("inline profile: %s %+v", c.Profile, p)
	}
	if !strings.Contains(string(src), "gen-plain") {
		t.Errorf("inline source not kept: %s", src)
	}
	if p.Agent.Env["DD_LOG_LEVEL"] != "debug" || p.Agent.Env["DD_LOGS_CONFIG_CONTAINER_COLLECT_ALL"] != "false" {
		t.Errorf("shared env not merged: %v", p.Agent.Env)
	}
	runs := c.plan(sanitize("tag filters"))
	want := []string{"tag-filters-release", "tag-filters-candidate", "tag-filters-release-2", "tag-filters-candidate-2"}
	for i, r := range runs {
		if r.name != want[i] {
			t.Errorf("run %d = %s, want %s", i, r.name, want[i])
		}
	}
	if runs[2].dir != filepath.Join("results", "tag-filters", "release-2") || runs[2].side != "a" || runs[2].round != 2 {
		t.Errorf("round 2: %+v", runs[2])
	}
	// The candidate's env lands on top of the profile's in the compose override.
	override, _ := overrideCompose(p, agentSpec{Image: c.B.Image, Pull: c.B.Pull, Env: c.B.Env})
	for _, s := range []string{`image: "datadog/agent-dev:x"`, "pull_policy: always", `DD_LOG_LEVEL: "debug"`, `DD_LOGS_CONFIG_TAG_FILTERS: "{\"exclude\":[\"env:*\"]}"`, `DD_LOGS_CONFIG_CONTAINER_COLLECT_ALL: "false"`} {
		if !strings.Contains(override, s) {
			t.Errorf("override missing %s:\n%s", s, override)
		}
	}
}

func TestLoadABConfigErrors(t *testing.T) {
	cases := map[string]string{
		"unknown key":  "profile: p.yaml\nb: { image: x }\nrunz: 3\n",
		"no b image":   "profile: p.yaml\n",
		"no profile":   "b: { image: x }\n",
		"same names":   "profile: p.yaml\na: { name: same }\nb: { name: same, image: x }\n",
		"bad pull":     "profile: p.yaml\nb: { image: x, pull: sometimes }\n",
		"profile list": "profile: [a, b]\nb: { image: x }\n",
	}
	for name, content := range cases {
		if _, _, err := loadABConfig(writeTemp(t, "aoc.yaml", content)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
