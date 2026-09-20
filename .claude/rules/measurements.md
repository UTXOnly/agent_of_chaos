---
paths:
  - "internal/report/**"
  - "internal/findings/**"
  - "internal/prof/**"
  - "internal/intake/**"
  - "internal/notebook/**"
  - "docs/measurements.md"
  - "docs/datadog.md"
---

# Changing what is measured

- A metric exists in exactly one place: `compareMetrics` in
  `internal/report/compare.go` (name, getter, renderer, sense). Add it
  there; `headlineMetrics` decides whether it is a verdict row; `topics` in
  `internal/findings/findings.go` decides which evidence explains it.
  Pick the sense honestly (`neutral` for load controls and geometry).
- New raw data flows intake → `report.json`: collect in
  `internal/intake/observe.go` / `procs.go` / `profiles.go`, roll up in
  `build.go`, emit as an `aoc.*` gauge in `emit.go` (tag names stay
  low-cardinality: `proc:`, `gen:`, `status:`, `name:`), add the field to
  `internal/report/report.go`.
- Profile views are `prof.Kinds` (file, sample type, rate vs level, unit).
  Rates are per wall-clock second; levels are averaged over captures.
- Evidence tables are small: ≤ 15 rows, values rendered with the metric's
  own formatter, a one-line "how to read it" where the table needs one.
  The brief is read by people and agents with a budget — add a table only
  when it names a cause the existing ones cannot.
- Every new number gets a row in `docs/measurements.md` (definition and
  where it misleads) and, if it is emitted, in `docs/datadog.md`.
- Tests: `report_test.go` (rendering/medians), `findings_test.go`
  (classification and the brief), `prof_test.go` (synthetic pprof),
  `intake_test.go` (handlers, parsing). `make test && make vet` before
  committing.
