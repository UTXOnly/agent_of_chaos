// Package cli implements the aoc subcommands.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/fmtutil"
	"github.com/UTXOnly/agent_of_chaos/internal/gen"
)

// EnvPrefix is prepended to upper-snake flag names to form env var names
// (--log-dir → AOC_LOG_DIR). Flags on the command line win.
const EnvPrefix = "AOC_"

type command struct {
	name  string
	short string
	run   func(args []string) int
}

var commands []command
var buildVersion = "dev"

func register(c command) { commands = append(commands, c) }

// Main dispatches a subcommand and returns the exit code.
func Main(args []string, version string) int {
	buildVersion = version
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(os.Stdout)
		return 0
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Println("aoc", version)
		return 0
	}
	for _, c := range commands {
		if c.name == args[0] {
			return c.run(args[1:])
		}
	}
	fmt.Fprintf(os.Stderr, "aoc: unknown command %q\n\n", args[0])
	usage(os.Stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `aoc — Datadog logs agent testing harness

Usage:
  aoc <command> [flags]

Commands:
`)
	order := []string{"generate", "intake", "run", "report", "compare", "ship", "scenarios", "profiles", "version"}
	byName := map[string]command{}
	for _, c := range commands {
		byName[c.name] = c
	}
	for _, n := range order {
		if c, ok := byName[n]; ok {
			fmt.Fprintf(w, "  %-11s %s\n", c.name, c.short)
		}
	}
	fmt.Fprintf(w, "  %-11s %s\n", "version", "print the build version")
	fmt.Fprintf(w, `
Every flag can also be set with an environment variable: AOC_<FLAG> with
dashes as underscores (--log-dir → AOC_LOG_DIR). Flags win over env.

Quick start (Docker, no real Datadog account needed):
  docker compose up -d                 # intake + agent + generators
  open http://localhost:8282           # live dashboard
  aoc report --intake http://localhost:8282 --out results/baseline
`)
}

// ── flag helpers ─────────────────────────────────────────────────────────────

// durationFlag accepts "90s", "5m", "1h30m" or bare seconds.
type durationFlag struct{ d *time.Duration }

func (f durationFlag) String() string {
	if f.d == nil {
		return "0"
	}
	return f.d.String()
}
func (f durationFlag) Set(s string) error {
	d, err := gen.ParseDuration(s)
	if err != nil {
		return err
	}
	*f.d = d
	return nil
}

// sizeFlag accepts "512KiB", "4MiB", "1MB" or bytes.
type sizeFlag struct{ n *int64 }

func (f sizeFlag) String() string {
	if f.n == nil {
		return "0"
	}
	return strconv.FormatInt(*f.n, 10)
}
func (f sizeFlag) Set(s string) error {
	n, err := fmtutil.ParseBytes(s)
	if err != nil {
		return err
	}
	*f.n = n
	return nil
}

// group is a labelled set of flags for the usage text.
type group struct {
	title string
	names []string
}

// flagSet wraps flag.FlagSet with grouped usage and env defaults.
type flagSet struct {
	*flag.FlagSet
	groups  []group
	current *group
	intro   string
	example string
}

func newFlagSet(name, intro, example string) *flagSet {
	fs := &flagSet{FlagSet: flag.NewFlagSet("aoc "+name, flag.ContinueOnError), intro: intro, example: example}
	fs.SetOutput(io.Discard)
	return fs
}

func (fs *flagSet) section(title string) {
	fs.groups = append(fs.groups, group{title: title})
	fs.current = &fs.groups[len(fs.groups)-1]
}

func (fs *flagSet) track(name string) {
	if fs.current == nil {
		fs.section("Flags")
	}
	fs.current.names = append(fs.current.names, name)
}

func (fs *flagSet) Str(name, def, usage string) *string {
	p := fs.String(name, def, usage)
	fs.track(name)
	return p
}
func (fs *flagSet) Int(name string, def int, usage string) *int {
	p := fs.FlagSet.Int(name, def, usage)
	fs.track(name)
	return p
}
func (fs *flagSet) Int64(name string, def int64, usage string) *int64 {
	p := fs.FlagSet.Int64(name, def, usage)
	fs.track(name)
	return p
}
func (fs *flagSet) Float(name string, def float64, usage string) *float64 {
	p := fs.Float64(name, def, usage)
	fs.track(name)
	return p
}
func (fs *flagSet) Bool(name string, def bool, usage string) *bool {
	p := fs.FlagSet.Bool(name, def, usage)
	fs.track(name)
	return p
}
func (fs *flagSet) Duration(name string, def time.Duration, usage string) *time.Duration {
	p := new(time.Duration)
	*p = def
	fs.Var(durationFlag{p}, name, usage)
	fs.track(name)
	return p
}
func (fs *flagSet) Size(name string, def int64, usage string) *int64 {
	p := new(int64)
	*p = def
	fs.Var(sizeFlag{p}, name, usage)
	fs.track(name)
	return p
}
func (fs *flagSet) Uint64(name string, def uint64, usage string) *uint64 {
	p := fs.FlagSet.Uint64(name, def, usage)
	fs.track(name)
	return p
}

func envName(flag string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(flag, "-", "_"))
}

// parse applies env defaults, then the command line. It prints usage and
// returns false on -h or error.
func (fs *flagSet) parse(args []string) (ok bool) {
	var envErr error
	fs.VisitAll(func(f *flag.Flag) {
		if v, set := os.LookupEnv(envName(f.Name)); set && v != "" && envErr == nil {
			if err := fs.Set(f.Name, v); err != nil {
				envErr = fmt.Errorf("%s=%q: %v", envName(f.Name), v, err)
			}
		}
	})
	if envErr != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", fs.Name(), envErr)
		return false
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			fs.usage(os.Stdout)
			return false
		}
		fmt.Fprintf(os.Stderr, "%s: %v\n(run with -h for help)\n", fs.Name(), err)
		return false
	}
	return true
}

func (fs *flagSet) usage(w io.Writer) {
	fmt.Fprintf(w, "%s\n\nUsage:\n  %s [flags]\n", fs.intro, fs.Name())
	if fs.example != "" {
		fmt.Fprintf(w, "\nExamples:\n%s\n", fs.example)
	}
	seen := map[string]bool{}
	for _, g := range fs.groups {
		fmt.Fprintf(w, "\n%s:\n", g.title)
		for _, n := range g.names {
			seen[n] = true
			fs.printFlag(w, fs.Lookup(n))
		}
	}
	var rest []string
	fs.VisitAll(func(f *flag.Flag) {
		if !seen[f.Name] {
			rest = append(rest, f.Name)
		}
	})
	if len(rest) > 0 {
		sort.Strings(rest)
		fmt.Fprintf(w, "\nOther:\n")
		for _, n := range rest {
			fs.printFlag(w, fs.Lookup(n))
		}
	}
	fmt.Fprintf(w, "\nEnv: every flag is also %s<FLAG> (dashes → underscores); flags win.\n", EnvPrefix)
}

func (fs *flagSet) printFlag(w io.Writer, f *flag.Flag) {
	def := f.DefValue
	switch {
	case def == "" || def == "0" || def == "false" || def == "0s":
		def = ""
	default:
		def = " (default " + def + ")"
	}
	name := "--" + f.Name
	if _, isBool := f.Value.(interface{ IsBoolFlag() bool }); !isBool {
		name += " " + placeholder(f)
	}
	fmt.Fprintf(w, "  %-30s %s%s\n", name, f.Usage, def)
}

func placeholder(f *flag.Flag) string {
	switch f.Value.(type) {
	case durationFlag:
		return "DUR"
	case sizeFlag:
		return "SIZE"
	}
	switch {
	case strings.Contains(f.Name, "dir"), strings.Contains(f.Name, "file"), strings.Contains(f.Name, "socket"):
		return "PATH"
	case strings.Contains(f.Name, "url"), strings.Contains(f.Name, "intake"), strings.Contains(f.Name, "telemetry"):
		return "URL"
	}
	if _, err := strconv.ParseFloat(f.DefValue, 64); err == nil {
		return "N"
	}
	return "VALUE"
}

// signalContext is cancelled on SIGINT/SIGTERM; a second signal exits hard.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
		<-ch
		fmt.Fprintln(os.Stderr, "aoc: forced exit")
		os.Exit(130)
	}()
	return ctx, cancel
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "aoc: "+format+"\n", args...)
	return 1
}
