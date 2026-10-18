package notebook

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestHandoffIndexesVisualsAfterRoundTrip(t *testing.T) {
	nb := ForRun(sample("evidence-test"), Options{})
	nb.Cells = append([]Cell{markdown("long generated narrative should stay out of context")}, nb.Cells...)
	nb.Cells = append(nb.Cells, markdown("### pkg.retry: CPU, +3%\n\nDetail kept in the evidence library."), iframe("https://example.com/profiling/comparison"))
	var stored Notebook
	if err := json.Unmarshal(nb.File(), &stored); err != nil {
		t.Fatal(err)
	}
	handoff := stored.Handoff("notebook.json")
	if strings.Contains(handoff, "long generated narrative") || strings.Contains(handoff, "aoc.intake.logs_per_sec{") {
		t.Fatal("handoff copied report text or query bodies instead of indexing evidence")
	}
	for i, cell := range stored.Cells {
		def := cell["attributes"].(map[string]any)["definition"].(map[string]any)
		row := fmt.Sprintf("| %d | %s |", i, def["type"])
		if strings.Contains(handoff, row) != (def["type"] != "markdown") {
			t.Errorf("wrong cell index/type in handoff: %s", row)
		}
	}
	if !strings.Contains(handoff, stored.Start.UTC().Format("2006-01-02T15:04:05Z07:00")) || !strings.Contains(handoff, stored.Name) {
		t.Fatal("handoff lost the question or absolute time window")
	}
	if !strings.Contains(handoff, "pkg.retry: CPU, +3%") || strings.Contains(handoff, "Detail kept") {
		t.Fatal("profile cell must be identifiable without copying its narrative")
	}
}
