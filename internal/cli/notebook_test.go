package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/notebook"
	"github.com/UTXOnly/agent_of_chaos/internal/report"
)

type rejectNotebookNetwork struct{ t *testing.T }

func (r rejectNotebookNetwork) RoundTrip(req *http.Request) (*http.Response, error) {
	r.t.Fatalf("notebook preparation made a network request: %s %s", req.Method, req.URL.Path)
	return nil, nil
}

func TestNotebookLocalEvenWithKeys(t *testing.T) {
	t.Setenv("DD_API_KEY", "test-api")
	t.Setenv("DD_APP_KEY", "test-app")
	old := http.DefaultTransport
	http.DefaultTransport = rejectNotebookNetwork{t}
	t.Cleanup(func() { http.DefaultTransport = old })
	dir := t.TempDir()
	r := report.Report{Name: "test", WindowStart: time.Unix(1700000000, 0), WindowEnd: time.Unix(1700000180, 0), Seconds: 180}
	b, _ := json.Marshal(r)
	if err := os.WriteFile(filepath.Join(dir, "report.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runNotebook([]string{"--results", dir}); code != 0 {
		t.Fatalf("notebook exit %d", code)
	}
	for _, name := range []string{"notebook.json", "notebook.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "notebook.url")); !os.IsNotExist(err) {
		t.Fatal("preparation must not invent a published notebook URL")
	}
	wantURL := "https://app.datadoghq.com/notebook/456"
	if err := linkNotebook(dir, wantURL); err != nil {
		t.Fatal(err)
	}
	if code := runNotebook([]string{"--results", dir}); code != 0 {
		t.Fatalf("single-run refresh exit %d", code)
	}
	if got := recordedNotebookURL(filepath.Join(dir, "notebook.json")); got != wantURL {
		t.Fatalf("single-run link lost: %s", got)
	}
}

func TestABCompareOnlyPreservesNotebookAndGate(t *testing.T) {
	t.Setenv("DD_API_KEY", "test-api")
	t.Setenv("DD_APP_KEY", "test-app")
	old := http.DefaultTransport
	http.DefaultTransport = rejectNotebookNetwork{t}
	t.Cleanup(func() { http.DefaultTransport = old })
	dir := t.TempDir()
	root := filepath.Join(dir, "example")
	for _, side := range []string{"a", "b"} {
		r := report.Report{Name: "example-" + side, WindowStart: time.Unix(1700000000, 0), WindowEnd: time.Unix(1700000180, 0), Seconds: 180}
		r.Agent.Image = "test/agent:7"
		r.Delivery = report.Delivery{GeneratedRecords: 100, Unique: 99, Missing: 1, Ratio: .99, AllFinal: true}
		b, _ := json.Marshal(r)
		runDir := filepath.Join(root, side)
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runDir, "report.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wantURL := "https://app.datadoghq.com/notebook/789"
	if err := os.WriteFile(filepath.Join(root, "notebook.url"), []byte(wantURL), 0o644); err != nil {
		t.Fatal(err)
	}
	config := writeTemp(t, "config.yaml", fmt.Sprintf(`name: example
results: %q
a: {image: "test/agent:7"}
b: {image: "test/agent:7"}
notebook: true
runs: 1
profile:
  duration: 3m
  generators:
    plain: {streams: 1, rate: 100}
`, dir))
	if code := runAB([]string{"--config", config, "--compare-only", "--notebook"}); code != 3 {
		t.Fatalf("shared loss must still fail delivery, got exit %d", code)
	}
	b, err := os.ReadFile(filepath.Join(root, "ab.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sum abSummary
	if err := json.Unmarshal(b, &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Notebook != wantURL {
		t.Fatalf("A/B re-render lost notebook: %s", sum.Notebook)
	}
	if _, err := os.Stat(sum.Files["notebook_brief"]); err != nil {
		t.Fatalf("A/B missing handoff: %v", err)
	}
}

func TestNotebookLinkAndRerenderAB(t *testing.T) {
	dir := t.TempDir()
	abFile := filepath.Join(dir, "ab.json")
	if err := os.WriteFile(abFile, []byte(`{"experiment":"test","future_field":{"keep":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	nb := &notebook.Notebook{Name: "test question", Start: time.Unix(1700000000, 0), End: time.Unix(1700000180, 0)}
	if _, err := saveNotebook(nb, filepath.Join(dir, "notebook.json")); err != nil {
		t.Fatal(err)
	}
	wantURL := "https://myorg.datadoghq.com/notebook/123/test-question"
	if code := runNotebook([]string{"--results", dir, "--link", wantURL}); code != 0 {
		t.Fatalf("link exit %d", code)
	}
	// A/B roots do not have report.json. Reopening must use their evidence.
	if code := runNotebook([]string{"--results", dir}); code != 0 {
		t.Fatalf("A/B notebook exit %d", code)
	}
	if got := recordedNotebookURL(filepath.Join(dir, "notebook.json")); got != wantURL {
		t.Fatalf("link lost during rerender: %s", got)
	}
	b, err := os.ReadFile(abFile)
	if err != nil {
		t.Fatal(err)
	}
	var sum map[string]json.RawMessage
	if err := json.Unmarshal(b, &sum); err != nil {
		t.Fatal(err)
	}
	if string(sum["future_field"]) == "" || !strings.Contains(string(sum["future_field"]), "true") {
		t.Fatal("link discarded existing metadata")
	}
	var gotURL string
	json.Unmarshal(sum["notebook"], &gotURL)
	if gotURL != wantURL {
		t.Fatal("ab.json link not updated")
	}
}

func TestNotebookLinkValidation(t *testing.T) {
	for _, raw := range []string{"", "http://example.com/notebook/1", "https://example.com/", "https://example.com/notebook/no", "https://example.com/notebook/0", "https://user:secret@example.com/notebook/1"} {
		t.Run(raw, func(t *testing.T) {
			dir := t.TempDir()
			if err := linkNotebook(dir, raw); err == nil {
				t.Fatal("accepted invalid notebook URL")
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatal("invalid link wrote files")
			}
		})
	}
	if err := linkNotebook(t.TempDir(), "https://app.datadoghq.com/notebook/123"); err == nil {
		t.Fatal("accepted directory without results")
	}
}

func TestConcludeOfflineKeepsEvidence(t *testing.T) {
	t.Setenv("DD_API_KEY", "")
	t.Setenv("DD_APP_KEY", "unused")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ab.json"), []byte(`{"experiment":"test"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runConclude([]string{"--results", dir, "--verdict", "inconclusive", "3 missing; extend drain"}); code != 0 {
		t.Fatalf("conclude exit %d", code)
	}
	b, err := os.ReadFile(filepath.Join(dir, "conclusion.md"))
	if err != nil || !strings.Contains(string(b), "3 missing; extend drain") {
		t.Fatalf("local conclusion missing: %v", err)
	}
	if code := runConclude([]string{"--results", dir, "--verdict", "typo", "replacement"}); code == 0 {
		t.Fatal("accepted invalid verdict")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "conclusion.md"))
	if string(b) != string(after) {
		t.Fatal("invalid verdict replaced the conclusion")
	}
}
