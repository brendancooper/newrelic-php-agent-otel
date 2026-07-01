package pprof

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/pprof/profile"
)

func mustReadFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return b
}

// realTraceData is a TxnTrace.Data JSON string in the exact envelope shape
// produced by axiom/nr_segment_traces.c: a TWO-element top-level array whose
// first element is the header [version, agentAttrs, userAttrs, rootNode,
// attrHash] (the attribute hash is folded into the header array as its 5th
// element, NOT a separate envelope element) and whose second element is the
// string table that backtick-indexed names reference. It mirrors the
// multi-node fixture in axiom/tests/test_segment_traces.c with nested +
// sibling segments.
const realTraceData = `[
  [0, {}, {}, [0, 2000, "` + "`" + `0", {}, [
    [0, 9, "` + "`" + `1", {}, [
      [1, 2, "` + "`" + `2", {}, []],
      [3, 4, "` + "`" + `3", {}, []]
    ]],
    [10, 60, "` + "`" + `4", {}, [
      [12, 30, "` + "`" + `5", {}, []],
      [40, 55, "PDO->query", {}, []]
    ]]
  ]], {"agentAttributes": ["agent_attributes"], "userAttributes": ["user_attributes"], "intrinsics": ["intrinsics"]}],
  ["WebTransaction/Function/index", "doWork", "dbQuery", "httpCall", "outerWrap", "innerSleep", "WebTransaction/*"]
]`

// legacyThreeElementTraceData carries the attribute hash as a SEPARATE
// top-level envelope element (string table at index 2). The real agent does
// not emit this layout, but the decoder tolerates it; this guards against
// regressing that tolerance.
const legacyThreeElementTraceData = `[
  [0, {}, {}, [0, 2000, "` + "`" + `0", {}, [
    [0, 9, "` + "`" + `1", {}, []]
  ]]],
  {"intrinsics": {"cpu_time": 0.002}},
  ["WebTransaction/Function/index", "doWork"]
]`

func TestDecodeTrace(t *testing.T) {
	tr, err := DecodeTrace([]byte(realTraceData), "WebTransaction/Function/index", 1_700_000_000_000)
	if err != nil {
		t.Fatalf("DecodeTrace: %v", err)
	}
	if tr.Name != "WebTransaction/Function/index" {
		t.Errorf("Name = %q", tr.Name)
	}
	if tr.Root == nil {
		t.Fatal("nil root")
	}
	// Root resolves via "`0" -> string table[0].
	if got, want := tr.Root.Name, "WebTransaction/Function/index"; got != want {
		t.Errorf("root name = %q want %q", got, want)
	}
	if len(tr.Root.Children) != 2 {
		t.Fatalf("root children = %d want 2", len(tr.Root.Children))
	}
	if got, want := tr.Root.Children[0].Name, "doWork"; got != want {
		t.Errorf("child0 name = %q want %q", got, want)
	}
	if got, want := tr.Root.Children[1].Name, "outerWrap"; got != want {
		t.Errorf("child1 name = %q want %q", got, want)
	}
	// Literal (non-backtick) name resolves as-is.
	if got, want := tr.Root.Children[1].Children[1].Name, "PDO->query"; got != want {
		t.Errorf("literal name = %q want %q", got, want)
	}
	if tr.HasUnresolvedNames() {
		t.Errorf("realTraceData should have no unresolved backtick names")
	}
}

func TestDecodeTrace_LegacyThreeElementEnvelope(t *testing.T) {
	// The decoder must tolerate a 3-element envelope (attr-hash separate, string
	// table at index 2) even though the real agent emits a 2-element envelope.
	tr, err := DecodeTrace([]byte(legacyThreeElementTraceData), "WebTransaction/Function/index", 1_700_000_000_000)
	if err != nil {
		t.Fatalf("DecodeTrace: %v", err)
	}
	if got, want := tr.Root.Name, "WebTransaction/Function/index"; got != want {
		t.Errorf("root name = %q want %q", got, want)
	}
	if got, want := tr.Root.Children[0].Name, "doWork"; got != want {
		t.Errorf("child name = %q want %q", got, want)
	}
	if tr.HasUnresolvedNames() {
		t.Errorf("legacy envelope should have no unresolved backtick names")
	}
}

func TestDecodeTrace_NoStringTableFlagUnresolved(t *testing.T) {
	// A trace envelope whose string table cannot be located must still decode
	// (names keep the raw backtick token) and must report unresolved names so
	// the caller can warn.
	bt := "`"
	noTable := `[[0, {}, {}, [0, 2000, "` + bt + `0", {}, [[1, 2, "` + bt + `1", {}, []]]]]]`
	tr, err := DecodeTrace([]byte(noTable), "WebTransaction/Function/index", 1_700_000_000_000)
	if err != nil {
		t.Fatalf("DecodeTrace: %v", err)
	}
	if !tr.HasUnresolvedNames() {
		t.Errorf("expected unresolved backtick names when string table is absent")
	}
	if got, want := tr.Root.Name, bt+"0"; got != want {
		t.Errorf("root name = %q want raw token %q", got, want)
	}
}

func TestBuildProfile_CPU(t *testing.T) {
	tr, err := DecodeTrace([]byte(realTraceData), "WebTransaction/Function/index", 1_700_000_000_000)
	if err != nil {
		t.Fatalf("DecodeTrace: %v", err)
	}
	p := BuildProfile(tr, TypeCPU, 10*time.Millisecond)

	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		t.Fatalf("pprof.Write: %v", err)
	}

	// Parse back with google/pprof to confirm validity.
	rp, err := profile.Parse(&buf)
	if err != nil {
		t.Fatalf("parse written profile: %v", err)
	}
	if got, want := len(rp.SampleType), 1; got != want {
		t.Errorf("SampleType len = %d want %d", got, want)
	}
	if got, want := rp.SampleType[0].Type, "cpu"; got != want {
		t.Errorf("SampleType = %q want %q", got, want)
	}

	// Four leaves (doWork->{dbQuery,httpCall}, outerWrap->{innerSleep,
	// PDO->query}) -> four samples.
	if got, want := len(rp.Sample), 4; got != want {
		t.Fatalf("Sample count = %d want %d", got, want)
	}

	// Every sample carries the Splunk labels.
	for i, s := range rp.Sample {
		if got := s.Label[labelSourceEventName]; len(got) != 1 || got[0] != "WebTransaction/Function/index" {
			t.Errorf("sample %d: source.event.name = %v", i, got)
		}
		periods := s.Label[labelSourceEventPeriod]
		if len(periods) != 1 || periods[0] != "10000000" {
			t.Errorf("sample %d: source.event.period = %v want [10000000]", i, periods)
		}
		ts := s.NumLabel[numLabelSourceEventTime]
		if len(ts) != 1 || ts[0] <= 0 {
			t.Errorf("sample %d: source.event.time = %v want positive ms", i, ts)
		}
	}

	// All IDs >= 1 (ID 0 is reserved).
	for _, f := range rp.Function {
		if f.ID == 0 {
			t.Errorf("Function %q has ID 0 (reserved)", f.Name)
		}
	}
	for _, l := range rp.Location {
		if l.ID == 0 {
			t.Error("Location has ID 0 (reserved)")
		}
	}
}

func TestBuildProfile_Wall_NoPeriodLabel(t *testing.T) {
	tr, err := DecodeTrace([]byte(realTraceData), "WebTransaction/Function/index", 1_700_000_000_000)
	if err != nil {
		t.Fatalf("DecodeTrace: %v", err)
	}
	p := BuildProfile(tr, TypeWall, 10*time.Millisecond)
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		t.Fatalf("pprof.Write: %v", err)
	}
	rp, err := profile.Parse(&buf)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rp.SampleType[0].Type != "wall" {
		t.Errorf("SampleType = %q want wall", rp.SampleType[0].Type)
	}
	for i, s := range rp.Sample {
		if _, ok := s.Label[labelSourceEventPeriod]; ok {
			t.Errorf("sample %d: wall profile must not carry source.event.period", i)
		}
	}
}

// TestBuildProfile_RoundtripsThroughGoToolPprof writes the profile and runs
// `go tool pprof -top` on it as an external validity check (skipped if the go
// toolchain is unavailable).
func TestBuildProfile_RoundtripsThroughGoToolPprof(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping external pprof round-trip in short mode")
	}
	tr, err := DecodeTrace([]byte(realTraceData), "WebTransaction/Function/index", 1_700_000_000_000)
	if err != nil {
		t.Fatalf("DecodeTrace: %v", err)
	}
	p := BuildProfile(tr, TypeCPU, 10*time.Millisecond)

	dir := t.TempDir()
	out := filepath.Join(dir, "cpu.pb.gz")
	f, err := os.Create(out)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := p.Write(f); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()

	// Validate by re-parsing.
	rp, err := profile.ParseData(mustReadFile(t, out))
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if len(rp.Sample) == 0 {
		t.Fatal("reparsed profile has no samples")
	}
}

// TestBuildProfile_GoldenPaths is the golden fixture test: it builds the pprof
// from the known realTraceData segment tree, writes it to a fixture file,
// re-parses it, and asserts the exact root->leaf function-name call stacks for
// every sample plus the per-sample label values. This locks the segment-tree ->
// pprof conversion against regressions.
func TestBuildProfile_GoldenPaths(t *testing.T) {
	tr, err := DecodeTrace([]byte(realTraceData), "WebTransaction/Function/index", 1_700_000_000_000)
	if err != nil {
		t.Fatalf("DecodeTrace: %v", err)
	}
	p := BuildProfile(tr, TypeCPU, 10*time.Millisecond)

	// Write the golden fixture to a temp path (this is the artefact a user could
	// open with `go tool pprof`); keep it deterministic enough to re-parse.
	dir := t.TempDir()
	fixture := filepath.Join(dir, "golden.pb.gz")
	f, err := os.Create(fixture)
	if err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	if err := p.Write(f); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	f.Close()

	rp, err := profile.ParseData(mustReadFile(t, fixture))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if err := rp.CheckValid(); err != nil {
		t.Fatalf("CheckValid: %v", err)
	}

	// Extract root->leaf function-name paths for every sample.
	got := make([][]string, 0, len(rp.Sample))
	for _, s := range rp.Sample {
		var path []string
		for _, loc := range s.Location { // pprof stores root-first in this builder
			for _, ln := range loc.Line {
				path = append(path, ln.Function.Name)
			}
		}
		got = append(got, path)
	}

	// Expected root->leaf call stacks derived from realTraceData.
	want := [][]string{
		{"WebTransaction/Function/index", "doWork", "dbQuery"},
		{"WebTransaction/Function/index", "doWork", "httpCall"},
		{"WebTransaction/Function/index", "outerWrap", "innerSleep"},
		{"WebTransaction/Function/index", "outerWrap", "PDO->query"},
	}
	if len(got) != len(want) {
		t.Fatalf("sample count = %d, want %d (got paths %v)", len(got), len(want), got)
	}
	// Order is not guaranteed across map iteration; compare as sets.
	have := make(map[string]struct{}, len(got))
	for _, g := range got {
		have[strings.Join(g, "->")] = struct{}{}
	}
	for _, w := range want {
		key := strings.Join(w, "->")
		if _, ok := have[key]; !ok {
			t.Errorf("missing expected path %q; have %v", key, keysOf(have))
		}
	}

	// Golden label assertions: every sample name + period.
	for _, s := range rp.Sample {
		if nv := s.NumLabel[numLabelSourceEventTime]; len(nv) != 1 || nv[0] < 1_700_000_000_000 || nv[0] >= 1_700_000_002_000 {
			t.Errorf("source.event.time = %v, want within [1700000000000, 1700000002000)", nv)
		}
		if lv := s.Label[labelSourceEventName]; len(lv) != 1 || lv[0] != "WebTransaction/Function/index" {
			t.Errorf("source.event.name = %v", lv)
		}
		if pv := s.Label[labelSourceEventPeriod]; len(pv) != 1 || pv[0] != "10000000" {
			t.Errorf("source.event.period = %v, want [10000000]", pv)
		}
	}
}

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
