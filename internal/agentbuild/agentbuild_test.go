package agentbuild

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestTag(t *testing.T) {
	cases := []struct {
		id   Ident
		base string
		want string
	}{
		{Ident{Commit: "9bd6b55c80ba7a5e59d28a44a8b109774cb7aab1", Branch: "main"}, "datadog/agent:7", "aoc/agent-dev:main"},
		{Ident{Commit: "9bd6b55c80ba7a5e59d28a44a8b109774cb7aab1", Branch: "brian/sender-fix", Dirty: []string{"a.go"}}, "datadog/agent:7", "aoc/agent-dev:brian-sender-fix-dirty"},
		{Ident{Commit: "9bd6b55c80ba7a5e59d28a44a8b109774cb7aab1"}, "datadog/agent:7-full", "aoc/agent-dev:9bd6b55-full"},
		{Ident{Commit: "9bd6b55c80ba7a5e59d28a44a8b109774cb7aab1", Branch: "-weird..name/"}, "datadog/agent:7.83.2-jmx", "aoc/agent-dev:weird..name--jmx"},
	}
	for _, c := range cases {
		if got := c.id.Tag(c.base); got != c.want {
			t.Errorf("Tag(%+v, %s) = %s, want %s", c.id, c.base, got, c.want)
		}
	}
}

func TestDescribe(t *testing.T) {
	id := Ident{Commit: "9bd6b55c80ba7a5e59d28a44a8b109774cb7aab1", Branch: "main"}
	if got := id.Describe(); got != "main @ 9bd6b55" {
		t.Errorf("got %q", got)
	}
	id.Dirty = []string{"x", "y", "z"}
	if got := id.Describe(); got != "main @ 9bd6b55 +3 uncommitted" {
		t.Errorf("got %q", got)
	}
	id.Branch = ""
	if got := id.Describe(); got != "9bd6b55 +3 uncommitted" {
		t.Errorf("got %q", got)
	}
}

func TestLooksLikeSHA(t *testing.T) {
	for s, want := range map[string]bool{"main": false, "9bd6b55": true, "9bd6b55c80ba7a5e59d28a44a8b109774cb7aab1": true, "deadbeef-branch": false, "abc": false, "7.83.2": false} {
		if got := looksLikeSHA(s); got != want {
			t.Errorf("looksLikeSHA(%q) = %v", s, got)
		}
	}
}

func TestPackaging(t *testing.T) {
	for img, want := range map[string]string{"datadog/agent:7": "", "datadog/agent:7-full": "-full", "datadog/agent:7.83.2-jmx": "-jmx", "registry:5000/agent:7-full": "-full", "datadog/agent": ""} {
		if got := packaging(img); got != want {
			t.Errorf("packaging(%q) = %q, want %q", img, got, want)
		}
	}
}

func TestUntar(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(h *tar.Header, body string) {
		if body != "" {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			tw.Write([]byte(body))
		}
	}
	add(&tar.Header{Name: "opt/datadog-agent/embedded/bin/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	add(&tar.Header{Name: "opt/datadog-agent/embedded/bin/python3.13", Typeflag: tar.TypeReg, Mode: 0o755}, "#!python")
	add(&tar.Header{Name: "opt/datadog-agent/embedded/bin/python3", Typeflag: tar.TypeSymlink, Linkname: "python3.13"}, "")
	add(&tar.Header{Name: "opt/datadog-agent/embedded/lib/python3.13/os.py", Typeflag: tar.TypeReg, Mode: 0o644}, "import sys")
	add(&tar.Header{Name: "etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644}, "nope")
	add(&tar.Header{Name: "opt/datadog-agent/embedded/../../../evil", Typeflag: tar.TypeReg, Mode: 0o644}, "nope")
	tw.Close()

	dir := t.TempDir()
	if err := untar(&buf, dir, "opt/datadog-agent/embedded/"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "bin", "python3.13")); err != nil || string(b) != "#!python" {
		t.Errorf("python3.13: %v %q", err, b)
	}
	if st, err := os.Stat(filepath.Join(dir, "bin", "python3.13")); err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Errorf("python3.13 should be executable: %v", err)
	}
	if link, err := os.Readlink(filepath.Join(dir, "bin", "python3")); err != nil || link != "python3.13" {
		t.Errorf("python3 symlink: %v %q", err, link)
	}
	if _, err := os.Stat(filepath.Join(dir, "lib", "python3.13", "os.py")); err != nil {
		t.Errorf("os.py: %v", err)
	}
	for _, bad := range []string{"etc/passwd", "evil", "../evil", "../../evil"} {
		if _, err := os.Stat(filepath.Join(dir, bad)); err == nil {
			t.Errorf("%s should not have been extracted", bad)
		}
	}
}

func TestCopyTree(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "disk.d"), 0o755)
	os.WriteFile(filepath.Join(src, "disk.d", "conf.yaml.default"), []byte("init_config:\n"), 0o644)
	os.Symlink("conf.yaml.default", filepath.Join(src, "disk.d", "conf.yaml.example"))
	dst := filepath.Join(t.TempDir(), "conf.d")
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "disk.d", "conf.yaml.default")); err != nil || string(b) != "init_config:\n" {
		t.Errorf("copied file: %v %q", err, b)
	}
	if l, err := os.Readlink(filepath.Join(dst, "disk.d", "conf.yaml.example")); err != nil || l != "conf.yaml.default" {
		t.Errorf("symlink: %v %q", err, l)
	}
	// A missing source still yields an (empty) destination for COPY.
	empty := filepath.Join(t.TempDir(), "conf.d")
	if err := copyTree(filepath.Join(src, "missing"), empty); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(empty); err != nil || !st.IsDir() {
		t.Errorf("empty destination: %v", err)
	}
}
