// Package agentbuild builds a Datadog Agent image from a checkout of the
// agent's repository, the way the agent's own `dda inv
// agent.hacky-dev-image-build` does — the Go binary and rtloader compiled
// against the embedded Python of a release image, then layered on that
// image — but inside the agent's Linux dev-env container, so it works from
// a Mac and needs nothing but Docker and git on the host.
//
// Everything the build does not replace is the release's: the embedded
// Python, the integrations, the base OS. The Python checks therefore keep
// working, and an A/B of the built image against its base differs only in
// the Go code. Building both sides from the checkout (main and a branch)
// leaves nothing but the branch between them.
package agentbuild

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/report"
)

const (
	// DefaultBuilder is the agent's Linux developer environment: Go, dda,
	// cmake, bazel and the rest at the versions main expects.
	DefaultBuilder = "datadog/agent-dev-env-linux"
	// Repository is where built images are tagged: aoc/agent-dev:<name>.
	Repository = "aoc/agent-dev"
	// cacheVolume holds the builder's /var/cache/dd — the Go build and
	// module caches, bazel, uv — so the second build is incremental.
	cacheVolume = "aoc-agent-build-cache"
	// embeddedPath is where the agent expects its Python; the release's is
	// mounted there in the builder so the rtloader links against it and
	// records the path it will have at run time.
	embeddedPath = "/opt/datadog-agent/embedded"
)

// Spec is one build: a checkout, what in it, and the release it goes on.
type Spec struct {
	Source  string // the checkout
	Ref     string // a git ref, or "." for the working tree as it is, uncommitted changes included
	Base    string // the release image the build is layered on
	Builder string // the image the build runs in (DefaultBuilder)
	Cache   string // scratch: worktrees, the base's runtime, build contexts (default: ~/.cache/aoc/agent-build)
	Name    string // the scratch worktree's name; one per side (a, b) so both can be built in a row
	// Platform is the image platform, linux/<arch>; empty means the
	// daemon's own.
	Platform string
	// PullBase re-pulls Base first, so a moving tag is the latest.
	PullBase bool
	// Log receives everything the build prints; Progress gets one line
	// per step.
	Log      io.Writer
	Progress func(format string, args ...any)
}

// Ident is what a Spec resolves to before anything runs: the commit, the
// branch, and the working tree's changes on top for a "." build.
type Ident struct {
	Commit    string
	Branch    string   // Ref when it names a branch; the current branch for "."
	Dirty     []string // paths changed or added on top of Commit ("." only)
	patch     []byte   // git diff --binary HEAD
	untracked []string
}

// Resolve reads the checkout: what commit Ref is, and for the working tree
// what sits on top of it. It runs no build.
func Resolve(ctx context.Context, spec Spec) (*Ident, error) {
	src, err := filepath.Abs(spec.Source)
	if err != nil {
		return nil, err
	}
	if _, err := git(ctx, src, "rev-parse", "--git-dir"); err != nil {
		return nil, fmt.Errorf("%s is not a git checkout", src)
	}
	if isWorkingTree(spec.Ref) {
		id := &Ident{}
		if id.Commit, err = git(ctx, src, "rev-parse", "HEAD"); err != nil {
			return nil, fmt.Errorf("%s: HEAD: %v", src, err)
		}
		id.Branch, _ = git(ctx, src, "branch", "--show-current")
		out, err := gitBytes(ctx, src, "diff", "--binary", "--no-color", "--no-ext-diff", "HEAD")
		if err != nil {
			return nil, fmt.Errorf("%s: diff: %v", src, err)
		}
		id.patch = out
		changed, _ := git(ctx, src, "diff", "--name-only", "HEAD")
		untracked, _ := git(ctx, src, "ls-files", "--others", "--exclude-standard")
		id.untracked = lines(untracked)
		id.Dirty = append(lines(changed), id.untracked...)
		return id, nil
	}
	commit, err := git(ctx, src, "rev-parse", "--verify", "--quiet", spec.Ref+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("%s: %q is not a commit (git fetch?)", src, spec.Ref)
	}
	id := &Ident{Commit: commit}
	if !looksLikeSHA(spec.Ref) {
		id.Branch = spec.Ref
	}
	return id, nil
}

func isWorkingTree(ref string) bool { return ref == "" || ref == "." }

// Tag names the image a Spec produces: aoc/agent-dev:<branch>[-dirty],
// with the base's -full/-jmx suffix kept so the two sides' packaging can
// still be told apart from the tags. A build from a bare commit is named
// by its short SHA.
func (id *Ident) Tag(base string) string {
	name := id.Branch
	if name == "" {
		name = short(id.Commit)
	}
	name = tagSafe(name)
	if len(id.Dirty) > 0 {
		name += "-dirty"
	}
	name += packaging(base)
	return Repository + ":" + name
}

// Describe is the one-line identity for plans and logs: "main @ 9bd6b55"
// or "brian/fix @ 1a2b3c4 +3 uncommitted".
func (id *Ident) Describe() string {
	s := short(id.Commit)
	if id.Branch != "" {
		s = id.Branch + " @ " + s
	}
	if n := len(id.Dirty); n > 0 {
		s += fmt.Sprintf(" +%d uncommitted", n)
	}
	return s
}

// Run builds the image: a scratch worktree of the commit (plus the
// working tree's changes), the base image's Python extracted beside it,
// the agent compiled in the builder against that Python, and the result
// layered on the base.
func Run(ctx context.Context, spec Spec) (*report.Build, error) {
	started := time.Now()
	if spec.Log == nil {
		spec.Log = io.Discard
	}
	if spec.Progress == nil {
		spec.Progress = func(string, ...any) {}
	}
	if spec.Builder == "" {
		spec.Builder = DefaultBuilder
	}
	if spec.Name == "" {
		spec.Name = "build"
	}
	src, err := filepath.Abs(spec.Source)
	if err != nil {
		return nil, err
	}
	spec.Source = src
	if spec.Base == "" {
		return nil, errors.New("no base image")
	}
	if spec.Platform == "" {
		if spec.Platform, err = daemonPlatform(ctx); err != nil {
			return nil, err
		}
	}
	cache, err := cacheDir(spec)
	if err != nil {
		return nil, err
	}
	id, err := Resolve(ctx, spec)
	if err != nil {
		return nil, err
	}
	tag := id.Tag(spec.Base)
	b := &report.Build{Image: tag, Source: src, Ref: spec.Ref, Branch: id.Branch, Commit: id.Commit, Dirty: len(id.Dirty), Base: spec.Base}
	if isWorkingTree(spec.Ref) {
		b.Ref = "."
	}
	fmt.Fprintf(spec.Log, "aoc build: %s (%s) on %s → %s\n", src, id.Describe(), spec.Base, tag)

	wt := filepath.Join(cache, spec.Name)
	spec.Progress("worktree %s ← %s", wt, id.Describe())
	if err := checkout(ctx, spec, id, wt); err != nil {
		return nil, fmt.Errorf("worktree: %w", err)
	}

	baseID, err := baseImage(ctx, spec)
	if err != nil {
		return nil, err
	}
	b.BaseID = baseID
	emb := filepath.Join(cache, spec.Name+".embedded")
	spec.Progress("embedded python of %s (%s) → %s", spec.Base, short(strings.TrimPrefix(baseID, "sha256:")), emb)
	if err := extractEmbedded(ctx, spec, baseID, emb); err != nil {
		return nil, fmt.Errorf("embedded python: %w", err)
	}

	spec.Progress("compiling in %s (log: %s)", spec.Builder, logName(spec.Log))
	if err := compile(ctx, spec, wt, emb); err != nil {
		return nil, err
	}

	spec.Progress("layering on %s → %s", spec.Base, tag)
	imageID, err := assemble(ctx, spec, id, wt, emb, filepath.Join(cache, spec.Name+".image"), baseID, tag)
	if err != nil {
		return nil, err
	}
	b.ImageID = imageID
	if v, err := dockerOut(ctx, "run", "--rm", "--platform", spec.Platform, "--entrypoint", "agent", tag, "version"); err == nil {
		b.Version = strings.TrimSpace(v)
	} else {
		return nil, fmt.Errorf("the built image cannot run `agent version`: %v", err)
	}
	b.Seconds = time.Since(started).Round(time.Second).Seconds()
	spec.Progress("built %s: %s (%s)", tag, b.Version, time.Duration(b.Seconds*float64(time.Second)))
	return b, nil
}

// cacheDir is the scratch root for this checkout: one directory per
// checkout path, so two checkouts never share worktrees.
func cacheDir(spec Spec) (string, error) {
	root := spec.Cache
	if root == "" {
		root = os.Getenv("AOC_BUILD_CACHE")
	}
	if root == "" {
		base := os.Getenv("XDG_CACHE_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(home, ".cache")
		}
		root = filepath.Join(base, "aoc", "agent-build")
	}
	sum := sha1.Sum([]byte(spec.Source))
	dir := filepath.Join(root, filepath.Base(spec.Source)+"-"+hex.EncodeToString(sum[:])[:8])
	return dir, os.MkdirAll(dir, 0o755)
}

// checkout puts the commit in the scratch worktree — a linked worktree of
// the checkout, so its objects and tags are shared and nothing in the
// checkout itself is touched — then, for a working-tree build, lays the
// checkout's uncommitted changes and untracked files on top. Build outputs
// (bin/, dev/, rtloader/build) are ignored files and survive from one
// build to the next.
func checkout(ctx context.Context, spec Spec, id *Ident, wt string) error {
	src := spec.Source
	if _, err := git(ctx, wt, "rev-parse", "--git-dir"); err != nil {
		// Missing, or left broken: start it over. Prune first, so a stale
		// registration of the same path does not block the add.
		os.RemoveAll(wt)
		git(ctx, src, "worktree", "prune")
		if _, err := git(ctx, src, "worktree", "add", "--detach", wt, id.Commit); err != nil {
			return err
		}
	}
	if _, err := git(ctx, wt, "reset", "--hard", "--quiet", id.Commit); err != nil {
		return err
	}
	if _, err := git(ctx, wt, "clean", "-fdq"); err != nil {
		return err
	}
	if len(id.patch) > 0 {
		cmd := exec.CommandContext(ctx, "git", "-C", wt, "apply", "--binary", "--whitespace=nowarn", "-")
		cmd.Stdin = bytes.NewReader(id.patch)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("applying the working tree's changes: %v\n%s", err, out)
		}
	}
	for _, rel := range id.untracked {
		if err := copyFile(filepath.Join(src, rel), filepath.Join(wt, rel)); err != nil {
			return fmt.Errorf("copying untracked %s: %v", rel, err)
		}
	}
	return nil
}

// baseImage pulls the base when asked (or when it is missing) and returns
// its image ID: the build is pinned to that, not to the tag.
func baseImage(ctx context.Context, spec Spec) (string, error) {
	pull := func() error {
		spec.Progress("pulling %s", spec.Base)
		return dockerRun(ctx, spec.Log, "pull", "--platform", spec.Platform, spec.Base)
	}
	if spec.PullBase {
		if err := pull(); err != nil {
			return "", err
		}
	}
	id, err := dockerOut(ctx, "image", "inspect", "--format", "{{.Id}}", spec.Base)
	if err != nil && !spec.PullBase {
		if err := pull(); err != nil {
			return "", err
		}
		id, err = dockerOut(ctx, "image", "inspect", "--format", "{{.Id}}", spec.Base)
	}
	if err != nil {
		return "", fmt.Errorf("base image %s: %v", spec.Base, err)
	}
	return strings.TrimSpace(id), nil
}

// extractEmbedded copies the base image's Python — interpreter, library,
// headers, stdlib and site-packages — into dir, keyed by the image ID so
// it is done once per base. The rtloader is built against it and the
// agent linked against that, exactly as the release was.
func extractEmbedded(ctx context.Context, spec Spec, baseID, dir string) error {
	marker := filepath.Join(dir, ".aoc-base")
	if cur, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(cur)) == baseID {
		return nil
	}
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--platform", spec.Platform, "--entrypoint", "bash", baseID,
		"-c", "tar --create "+embeddedPath+"/{bin,lib,include}/*python*")
	cmd.Stderr = spec.Log
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	uerr := untar(out, dir, strings.TrimPrefix(embeddedPath, "/")+"/")
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("docker run %s: %v", spec.Base, err)
	}
	if uerr != nil {
		return uerr
	}
	return os.WriteFile(marker, []byte(baseID+"\n"), 0o644)
}

// buildCommand is what runs in the builder: the agent's own build task,
// with the rtloader built by cmake against the mounted Python and
// installed where the agent looks for it at run time, and the binary's
// paths set as the release's are. --no-development keeps the compiler's
// optimisations on; hacky-dev-image-build turns them off for delve.
const buildCommand = `set -euo pipefail
dda inv agent.build \
  --legacy-rtloader-cmake \
  --embedded-path=` + embeddedPath + ` \
  --python-home-3=` + embeddedPath + ` \
  --cmake-options='-DPython3_ROOT_DIR=` + embeddedPath + ` -DPython3_FIND_STRATEGY=LOCATION' \
  --no-development \
  --install-path=/opt/datadog-agent
`

// compile runs the build in the builder image. The worktree and the
// checkout's git directory are mounted at their host paths, so the
// worktree's .git pointer and git itself just work; the git directory is
// read-only, so nothing in the container can touch the checkout. The
// caches persist in a named volume.
func compile(ctx context.Context, spec Spec, wt, emb string) error {
	gitDir, err := git(ctx, spec.Source, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("git dir of %s: %v", spec.Source, err)
	}
	args := []string{"run", "--rm", "--name", "aoc-build-" + tagSafe(spec.Name), "--platform", spec.Platform,
		// The image realigns its user to these so files written to the
		// worktree keep the host's ownership.
		"-e", "HOST_UID=" + strconv.Itoa(os.Getuid()), "-e", "HOST_GID=" + strconv.Itoa(os.Getgid()),
		// The version and commit come from ldflags; VCS stamping would
		// need to write the (read-only) index.
		"-e", "GOFLAGS=-buildvcs=false",
		"--mount", "type=bind,src=" + wt + ",dst=" + wt,
		"--mount", "type=bind,src=" + gitDir + ",dst=" + gitDir + ",ro",
		"--mount", "type=bind,src=" + emb + ",dst=" + embeddedPath,
		"--mount", "type=volume,src=" + cacheVolume + ",dst=/var/cache/dd",
		"-w", wt, spec.Builder, "gosu", "dd", "bash", "-c", buildCommand}
	fmt.Fprintf(spec.Log, "$ docker %s\n", strings.Join(args, " "))
	if err := dockerRun(ctx, spec.Log, args...); err != nil {
		return fmt.Errorf("build failed in %s: %v (see the log)", spec.Builder, err)
	}
	return nil
}

// dockerfile lays the build on the base. Only what the build produced is
// replaced: the agent binary, the rtloader libraries, and the default
// check configs the checkout ships (added to the release's, as
// hacky-dev-image-build does).
const dockerfile = `# generated by aoc build — do not edit
ARG BASE_IMAGE=datadog/agent:7
FROM ${BASE_IMAGE}
COPY agent /opt/datadog-agent/bin/agent/agent
COPY lib/ /opt/datadog-agent/embedded/lib/
COPY conf.d/ /etc/datadog-agent/conf.d/
`

// assemble collects the build's outputs into a small build context and
// runs docker build with the base pinned by ID.
func assemble(ctx context.Context, spec Spec, id *Ident, wt, emb, ctxDir, baseID, tag string) (string, error) {
	os.RemoveAll(ctxDir)
	if err := os.MkdirAll(filepath.Join(ctxDir, "lib"), 0o755); err != nil {
		return "", err
	}
	agent := filepath.Join(wt, "bin", "agent", "agent")
	if err := copyFile(agent, filepath.Join(ctxDir, "agent")); err != nil {
		return "", fmt.Errorf("the build left no agent binary: %v", err)
	}
	libs, _ := filepath.Glob(filepath.Join(emb, "lib", "libdatadog-agent-*"))
	n := 0
	for _, lib := range libs {
		st, err := os.Lstat(lib)
		if err != nil || !st.Mode().IsRegular() {
			continue // the base keeps its own .so → .so.0 → .so.0.1.0 links
		}
		if err := copyFile(lib, filepath.Join(ctxDir, "lib", filepath.Base(lib))); err != nil {
			return "", err
		}
		n++
	}
	if n == 0 {
		return "", fmt.Errorf("the build left no rtloader libraries in %s/lib", emb)
	}
	confd := filepath.Join(wt, "bin", "agent", "dist", "conf.d")
	if err := copyTree(confd, filepath.Join(ctxDir, "conf.d")); err != nil {
		return "", fmt.Errorf("conf.d: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return "", err
	}
	// BuildKit resolves FROM by name, so the ID gets a tag for the build.
	pin := Repository + "-base:" + short(strings.TrimPrefix(baseID, "sha256:"))
	if err := dockerRun(ctx, spec.Log, "tag", baseID, pin); err != nil {
		return "", err
	}
	defer dockerRun(ctx, io.Discard, "rmi", "--no-prune", pin)
	dirty := "false"
	if len(id.Dirty) > 0 {
		dirty = "true"
	}
	args := []string{"build", "--platform", spec.Platform, "--progress=plain", "--pull=false", "-t", tag,
		"--build-arg", "BASE_IMAGE=" + pin,
		"--label", "org.opencontainers.image.revision=" + id.Commit,
		"--label", "org.opencontainers.image.base.name=" + spec.Base,
		"--label", "aoc.build.source=" + spec.Source,
		"--label", "aoc.build.ref=" + orDot(spec.Ref),
		"--label", "aoc.build.dirty=" + dirty,
		"--label", "aoc.build.base=" + baseID,
		ctxDir}
	if err := dockerRun(ctx, spec.Log, args...); err != nil {
		return "", fmt.Errorf("docker build: %v", err)
	}
	imageID, err := dockerOut(ctx, "image", "inspect", "--format", "{{.Id}}", tag)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(imageID), nil
}

func orDot(ref string) string {
	if isWorkingTree(ref) {
		return "."
	}
	return ref
}

// ── helpers ─────────────────────────────────────────────────────────────────

func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitBytes(ctx, dir, args...)
	return strings.TrimSpace(string(out)), err
}

func gitBytes(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, errors.New(msg)
	}
	return out, nil
}

func dockerRun(ctx context.Context, log io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = log, log
	return cmd.Run()
}

func dockerOut(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", errors.New(msg)
		}
		return "", err
	}
	return string(out), nil
}

// daemonPlatform is linux/<the daemon's architecture>: what the base image
// and the builder are pulled and run as.
func daemonPlatform(ctx context.Context) (string, error) {
	out, err := dockerOut(ctx, "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}")
	if err != nil {
		return "", fmt.Errorf("docker: %v", err)
	}
	return strings.TrimSpace(out), nil
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func looksLikeSHA(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// tagSafe makes a docker tag out of a branch name: [A-Za-z0-9_.-], no
// leading separator, at most 100 characters.
func tagSafe(s string) string {
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-' {
			return r
		}
		return '-'
	}, s)
	s = strings.TrimLeft(s, ".-")
	if len(s) > 100 {
		s = s[:100]
	}
	if s == "" {
		return "build"
	}
	return s
}

// packaging is the -full or -jmx suffix of an image's tag, if any.
func packaging(image string) string {
	tag := image
	if i := strings.LastIndex(tag, ":"); i >= 0 && !strings.Contains(tag[i+1:], "/") {
		tag = tag[i+1:]
	}
	for _, s := range []string{"-full", "-jmx"} {
		if strings.HasSuffix(tag, s) {
			return s
		}
	}
	return ""
}

func logName(w io.Writer) string {
	if f, ok := w.(*os.File); ok {
		return f.Name()
	}
	return "stderr"
}
