// Package pprof converts New Relic PHP agent transaction-trace segment trees
// into pprof Profiles suitable for Splunk AlwaysOn Profiling via OTLP Logs.
//
// A transaction trace's segment tree arrives in the daemon as a JSON string
// (TxnTrace.Data) with this envelope shape (see axiom/nr_segment_traces.c and
// axiom/tests/test_segment_traces.c):
//
//	[
//	  [0, {}, {}, <rootNode>, <attrHash>],      // header: [version, agentAttrs, userAttrs, root, attrs]
//	  ["WebTransaction/*", "A", "B", ...]        // string table; backtick indices `\`N` ref this
//	]
//
// The attr-hash is emitted as the 5th element of the header array (not as a
// separate envelope element), so the real envelope has exactly two top-level
// elements and the string table sits at index 1. Older/hand-built traces may
// instead carry the attr-hash as a separate envelope element, giving a
// three-element envelope with the string table at index 2. The string table is
// therefore located by scanning the envelope for the element that parses as a
// JSON array of strings, which tolerates either layout.
//
// Each node is [startMillis, stopMillis, name, params, children], where name is
// either a literal string or a backtick-prefixed index ("`0") into the string
// table.
package pprof

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Segment is the decoded in-memory segment-tree node.
type Segment struct {
	Name       string  // resolved segment name (backtick index expanded)
	StartMs    int64   // offset from trace start, in milliseconds
	StopMs     int64   // offset from trace start, in milliseconds
	DurationMs float64 // stopMs - startMs
	Children   []*Segment
}

// Trace is a decoded transaction trace.
type Trace struct {
	Name      string    // transaction metric name (e.g. "WebTransaction/...")
	StartTime time.Time // wall-clock start of the trace (from TxnTrace.UnixTimestampMillis)
	Root      *Segment
}

// DecodeTrace parses a TxnTrace.Data JSON string into a Trace. traceName and
// traceStartUnixMillis come from the enclosing TxnTrace (MetricName and
// UnixTimestampMillis).
func DecodeTrace(data []byte, traceName string, traceStartUnixMillis float64) (*Trace, error) {
	// The envelope is a JSON array.
	var envelope []json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("pprof: parse trace envelope: %w", err)
	}
	if len(envelope) < 1 {
		return nil, fmt.Errorf("pprof: trace envelope has %d elements, need >=1", len(envelope))
	}

	// Locate the string table by scanning the envelope for the element that
	// parses as a JSON array of strings. The real agent emits a two-element
	// envelope whose string table is at index 1 (the attr-hash is folded into
	// the header array as its 5th element); some traces instead carry the
	// attr-hash as a separate envelope element, putting the string table at
	// index 2. Scanning tolerates both layouts and is robust to future
	// reordering. Not finding a string table is not fatal here: resolve() will
	// keep the raw backtick token, and findUnresolvedNames() below lets the
	// caller surface the problem.
	var stringTable []string
	for _, el := range envelope {
		var cand []string
		if err := json.Unmarshal(el, &cand); err == nil && cand != nil {
			stringTable = cand
			break
		}
	}

	// first element: the header [version, agentAttrs, userAttrs, rootNode],
	// optionally followed by a 5th element (the attribute hash) in the real
	// agent layout. Parse as a rawMessage array and extract index 3 (the root
	// node); any 5th element is ignored here.
	var header []json.RawMessage
	if err := json.Unmarshal(envelope[0], &header); err != nil {
		return nil, fmt.Errorf("pprof: parse trace header: %w", err)
	}
	if len(header) < 4 {
		return nil, fmt.Errorf("pprof: trace header has %d elements, need >=4", len(header))
	}
	rootNode := header[3]

	resolve := func(raw string) string {
		if len(raw) >= 2 && raw[0] == '`' {
			// Backtick-prefixed index into the string table.
			n, err := strconv.Atoi(raw[1:])
			if err == nil && n >= 0 && n < len(stringTable) {
				return stringTable[n]
			}
		}
		return raw
	}

	root, err := decodeNode(rootNode, resolve)
	if err != nil {
		return nil, fmt.Errorf("pprof: parse root node: %w", err)
	}

	start := time.Unix(0, int64(traceStartUnixMillis*float64(time.Millisecond)))
	return &Trace{
		Name:      traceName,
		StartTime: start,
		Root:      root,
	}, nil
}

// HasUnresolvedNames reports whether any segment still carries a raw
// backtick-index name (e.g. "`0"), which means the string table could not
// resolve it. This typically indicates the trace envelope carried no usable
// string table. Callers can use this to surface a diagnostic warning after
// DecodeTrace instead of silently emitting a pprof profile whose Function
// names are useless backtick tokens.
func (t *Trace) HasUnresolvedNames() bool {
	var walk func(*Segment) bool
	walk = func(s *Segment) bool {
		if s == nil {
			return false
		}
		if strings.HasPrefix(s.Name, "`") {
			return true
		}
		for _, c := range s.Children {
			if walk(c) {
				return true
			}
		}
		return false
	}
	return walk(t.Root)
}

// resolveName maps a backtick-indexed segment name to its resolved string; on
// failure it returns the raw token unchanged.
type resolveName func(raw string) string

// decodeNode parses a single segment node
// [startMs, stopMs, name, params, children].
func decodeNode(raw json.RawMessage, resolve resolveName) (*Segment, error) {
	var node []json.RawMessage
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil, fmt.Errorf("parse node: %w", err)
	}
	if len(node) < 5 {
		return nil, fmt.Errorf("node has %d elements, need >=5", len(node))
	}
	var startMs, stopMs float64
	if err := json.Unmarshal(node[0], &startMs); err != nil {
		return nil, fmt.Errorf("parse startMs: %w", err)
	}
	if err := json.Unmarshal(node[1], &stopMs); err != nil {
		return nil, fmt.Errorf("parse stopMs: %w", err)
	}
	var nameRaw string
	if err := json.Unmarshal(node[2], &nameRaw); err != nil {
		// name may legally be a non-string; fall back to a placeholder.
		nameRaw = ""
	}

	s := &Segment{
		Name:       resolve(nameRaw),
		StartMs:    int64(startMs),
		StopMs:     int64(stopMs),
		DurationMs: stopMs - startMs,
	}

	// children is index 4. It may be "null" or an empty array for a leaf.
	var children []json.RawMessage
	if err := json.Unmarshal(node[4], &children); err != nil {
		// Some leaves emit "" or null; treat as no children.
		children = nil
	}
	for _, c := range children {
		cs, err := decodeNode(c, resolve)
		if err != nil {
			return nil, err
		}
		s.Children = append(s.Children, cs)
	}
	// If node name is empty but resolved to "", keep an explicit placeholder
	// so pprof Function.Name is never blank (helper for readability).
	if strings.TrimSpace(s.Name) == "" {
		s.Name = "<unnamed>"
	}
	return s, nil
}
