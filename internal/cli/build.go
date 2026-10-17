package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/UTXOnly/agent_of_chaos/internal/agentbuild"
	"github.com/UTXOnly/agent_of_chaos/internal/findings"
)

func init() {
	register(command{name: "build", short: "build an agent image from a checkout, on top of the release (--src REF|.)", run: runBuild})
}

// BuildConfig is `build:` in aoc.yaml — what a side built from the
// checkout (a.src / b.src, --a-src / --b-src) is layered on and built in.
type BuildConfig struct {
	Base    string `yaml:"base,omitempty"`    // the release image; default a.image, or datadog/agent:7 when a is built too
	Builder string `yaml:"builder,omitempty"` // the dev-env image the build runs in
}

// buildSpec is the Spec for one side, from the config: the base is the
// control image unless the control is built too, and it is re-pulled
// when the control would be (a.pull: always), so the base is the
// control's exact image.
func (c *ABConfig) buildSpec(side string, v Variant, source string, pull bool, log io.Writer, progress func(string, ...any)) agentbuild.Spec {
	base := c.Build.Base
	if base == "" {
		base = c.A.Image
		if c.A.Src != "" || base == "" {
			base = defaultAImage
		}
	}
	return agentbuild.Spec{Source: source, Ref: v.Src, Base: base, Builder: c.Build.Builder, Name: side, PullBase: pull, Log: log, Progress: progress}
}

// buildSource is the checkout builds come from: `source:`, else
// DATADOG_AGENT_SRC, else ../datadog-agent — the same lookup the brief's
// code section uses.
func buildSource(configured string) (string, error) {
	src := findings.DetectSource(configured)
	if src == "" {
		return "", fmt.Errorf("no agent checkout: set `source:` in aoc.yaml or DATADOG_AGENT_SRC, or check the agent out next to this directory as ../datadog-agent")
	}
	return src, nil
}

func runBuild(args []string) int {
	fs := newFlagSet("build",
		"Build a Datadog Agent image from a checkout of the agent's repository,\n"+
			"the way the agent's own hacky-dev-image-build does but inside its Linux\n"+
			"dev-env container, so it works from a Mac: the Go binary and rtloader\n"+
			"compiled against the embedded Python of a release image, then layered\n"+
			"on that image. The Python integrations are the release's and keep\n"+
			"working. `aoc ab --b-src` does the same for a side of an A/B test.",
		"  aoc build                        # the checkout's working tree, uncommitted changes included, on datadog/agent:7\n"+
			"  aoc build --src main             # a ref: a branch, a tag, a commit\n"+
			"  aoc build --src . --base datadog/agent:7.83.2\n"+
			"  aoc ab --b $(aoc build 2>/dev/null) --workload baseline   # or just: aoc ab --b-src .")
	fs.section("What to build")
	src := fs.Str("src", ".", "a git ref in the checkout, or . for its working tree as it is")
	source := fs.Str("source", "", "the checkout (default: `source:` in aoc.yaml, DATADOG_AGENT_SRC, ../datadog-agent)")
	base := fs.Str("base", "", "the release image to layer the build on (default: `build.base` in aoc.yaml, else a.image)")
	builder := fs.Str("builder", "", "the image the build runs in (default: `build.builder` in aoc.yaml, else "+agentbuild.DefaultBuilder+")")
	pull := fs.Bool("pull", true, "re-pull the base image first (--pull=false keeps the local copy; the default follows a.pull)")
	fs.section("Plumbing")
	name := fs.Str("name", "build", "the scratch worktree's name under the cache; aoc ab uses a and b")
	platform := fs.Str("platform", "", "linux/<arch> (default: the docker daemon's)")
	config := fs.Str("config", "aoc.yaml", "the settings to read `source:` and `build:` from")
	logPath := fs.Str("log", "", "write the build's output here instead of stderr")
	asJSON := fs.Bool("json", false, "print the build record (image, commit, base, version) as JSON instead of the tag")
	if !fs.parse(args) {
		return 2
	}
	cfg := &ABConfig{A: Variant{Pull: "always"}} // the config's defaults, without a config
	if c, err := loadABConfig(*config); err == nil {
		cfg = c
	} else if !os.IsNotExist(err) {
		return fail("build: %v", err)
	}
	if *source == "" {
		*source = cfg.Source
	}
	checkout, err := buildSource(*source)
	if err != nil {
		return fail("build: %v", err)
	}
	var log io.Writer = os.Stderr
	if *logPath != "" {
		f, err := os.Create(*logPath)
		if err != nil {
			return fail("build: %v", err)
		}
		defer f.Close()
		log = f
	}
	spec := cfg.buildSpec(*name, Variant{Src: *src}, checkout, cfg.A.Pull == "always", log, func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "  "+format+"\n", args...)
	})
	if *base != "" {
		spec.Base = *base
	}
	if *builder != "" {
		spec.Builder = *builder
	}
	if flagGiven(fs, "pull") {
		spec.PullBase = *pull
	}
	spec.Platform = *platform
	ctx, cancel := signalContext()
	defer cancel()
	fmt.Fprintf(os.Stderr, "aoc build  %s  %s  on %s\n", checkout, describeRef(*src), spec.Base)
	b, err := agentbuild.Run(ctx, spec)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "\naoc: interrupted")
			return 130
		}
		return fail("build: %v", err)
	}
	if *asJSON {
		out, _ := json.MarshalIndent(b, "", "  ")
		fmt.Println(string(out))
		return 0
	}
	fmt.Println(b.Image)
	return 0
}

func describeRef(ref string) string {
	if ref == "" || ref == "." {
		return "working tree"
	}
	return ref
}
