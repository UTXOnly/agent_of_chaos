package cli

import (
	"strings"
	"testing"

	"github.com/UTXOnly/agent_of_chaos/internal/agentbuild"
)

func TestBuildSpecBase(t *testing.T) {
	cases := []struct {
		name string
		cfg  ABConfig
		want string
	}{
		{"the control image", ABConfig{A: Variant{Image: "datadog/agent:7.83.2"}, B: Variant{Src: "."}}, "datadog/agent:7.83.2"},
		{"the release when the control is built too", ABConfig{A: Variant{Image: "aoc/agent-dev:main", Src: "main"}, B: Variant{Src: "."}}, defaultAImage},
		{"build.base over both", ABConfig{A: Variant{Image: "datadog/agent:7.83.2"}, B: Variant{Src: "."}, Build: BuildConfig{Base: "datadog/agent:7-full"}}, "datadog/agent:7-full"},
	}
	for _, c := range cases {
		spec := c.cfg.buildSpec("b", c.cfg.B, "/src", true, nil, nil)
		if spec.Base != c.want {
			t.Errorf("%s: base = %s, want %s", c.name, spec.Base, c.want)
		}
		if spec.Name != "b" || spec.Ref != "." || spec.Source != "/src" || !spec.PullBase {
			t.Errorf("%s: spec = %+v", c.name, spec)
		}
	}
	pinned := &ABConfig{B: Variant{Src: "."}, Build: BuildConfig{Builder: "datadog/agent-dev-env-linux:pinned"}}
	spec := pinned.buildSpec("b", pinned.B, "/src", false, nil, nil)
	if spec.Builder != "datadog/agent-dev-env-linux:pinned" || spec.PullBase {
		t.Errorf("builder/pull: %+v", spec)
	}
	if agentbuild.DefaultBuilder == "" {
		t.Error("no default builder")
	}
}

func TestLoadABConfigSrc(t *testing.T) {
	prof := writeTemp(t, "baseline.yaml", "duration: 2m\ngenerators:\n  plain: { streams: 4, rate: 100 }\n")
	cfgPath := writeTemp(t, "aoc.yaml", "profile: "+prof+"\na:\n  src: main\nb:\n  src: .\nbuild:\n  base: datadog/agent:7.83.2\n")
	c, err := loadABConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.A.Image != "" || c.A.Src != "main" || !c.A.built() {
		t.Errorf("a: %+v", c.A)
	}
	if !c.B.built() || c.Build.Base != "datadog/agent:7.83.2" {
		t.Errorf("b: %+v build: %+v", c.B, c.Build)
	}
	if err := c.validate(cfgPath); err != nil {
		t.Errorf("a src side needs no image: %v", err)
	}
	// Neither an image nor a source for b is still an error, and it names both flags.
	c.B = Variant{Name: "b"}
	if err := c.validate(cfgPath); err == nil || !strings.Contains(err.Error(), "--b-src") {
		t.Errorf("validate: %v", err)
	}
}
