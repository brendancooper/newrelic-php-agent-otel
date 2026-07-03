package pprof

import (
	"strconv"
	"time"

	"github.com/google/pprof/profile"
)

// ProfileType selects which pprof SampleType/PeriodType to emit and which
// Splunk AlwaysOn Profiling keys to write.
type ProfileType string

const (
	// TypeCPU is a CPU-time profile: sample value = segment exclusive duration
	// in nanoseconds; the backend expects a source.event.period sample label.
	TypeCPU ProfileType = "cpu"
	// TypeWall is a wall-clock profile: sample value = segment inclusive
	// duration in nanoseconds; no source.event.period is emitted.
	TypeWall ProfileType = "wall"
)

// Splunk AlwaysOn Profiling sample-label keys (backend contract, contrib PR
// #48598 pkg/translator/splunk/profiles_to_splunk.go).
const (
	labelSourceEventName    = "source.event.name"
	numLabelSourceEventTime = "source.event.time"   // int64, unix MILLISECONDS
	labelSourceEventPeriod  = "source.event.period" // string; CPU only
)

// BuildProfile converts one decoded transaction Trace into a pprof Profile
// whose Samples each carry the Splunk AlwaysOn Profiling labels. One Sample is
// emitted per root->leaf path. profileType sets PeriodType and (for cpu) the
// source.event.period sample label. period is the sampling period in
// nanoseconds (used only for cpu profiles).
//
// The returned profile has Function/Location IDs starting at 1 (ID 0 is
// reserved and makes the profile malformed to go tool pprof).
func BuildProfile(t *Trace, profileType ProfileType, period time.Duration) *profile.Profile {
	p := &profile.Profile{
		TimeNanos:     t.StartTime.UnixNano(),
		DurationNanos: rootDurationNanos(t.Root),
		PeriodType:    &profile.ValueType{Type: string(profileType), Unit: "nanoseconds"},
		Period:        int64(period),
		SampleType:    []*profile.ValueType{{Type: string(profileType), Unit: "nanoseconds"}},
	}

	// Build the function/location tables keyed by segment name+file+line so
	// identical (name, filepath, lineno) triples share IDs. New entries get
	// incrementing IDs >= 1 (ID 0 is reserved and makes the profile malformed
	// to go tool pprof).
	var funcs []*profile.Function
	var locales []*profile.Location
	locID := make(map[string]uint64)

	// ensureEntry adds a Function + Location for a segment if missing and
	// returns the shared Location pointer. Filename/line come from the
	// segment's Code Level Metrics attributes when present (see
	// Segment.Filepath/Lineno); segments without them (CLM disabled, or
	// internal/builtin PHP functions) fall back to the generic "php"
	// filename with no line, preserving prior behavior.
	ensureEntry := func(seg *Segment) *profile.Location {
		filename := seg.Filepath
		if filename == "" {
			filename = "php"
		}
		key := seg.Name + "\x00" + filename + "\x00" + strconv.FormatInt(seg.Lineno, 10)
		if id, ok := locID[key]; ok {
			return locales[id-1]
		}
		fid := uint64(len(funcs) + 1)
		fn := &profile.Function{ID: fid, Name: seg.Name, SystemName: seg.Name, Filename: filename}
		funcs = append(funcs, fn)

		lid := uint64(len(locales) + 1)
		loc := &profile.Location{ID: lid, Line: []profile.Line{{Function: fn, Line: seg.Lineno}}}
		locales = append(locales, loc)
		locID[key] = lid
		return loc
	}

	periodStr := strconv.FormatInt(int64(period), 10)
	txnName := t.Name

	// addSample builds one pprof Sample from a leaf-first segment path.
	addSample := func(path []*Segment) {
		locs := make([]*profile.Location, 0, len(path))
		for i := len(path) - 1; i >= 0; i-- { // path is leaf-first already
			locs = append(locs, ensureEntry(path[i]))
		}

		// Sample value = inclusive duration of the LEAF segment, in ns.
		leaf := path[0]
		val := int64(leaf.DurationMs * float64(time.Millisecond))

		s := &profile.Sample{
			Location: locs,
			Value:    []int64{val},
			Label:    map[string][]string{},
			NumLabel: map[string][]int64{},
		}
		s.Label[labelSourceEventName] = []string{txnName}

		// source.event.time: unix time in MILLISECONDS when the leaf sample
		// was taken. Use the leaf segment's wall-clock stop time (the moment
		// the sampled observation completes).
		leafAbs := t.StartTime.Add(time.Duration(leaf.StopMs) * time.Millisecond)
		s.NumLabel[numLabelSourceEventTime] = []int64{leafAbs.UnixMilli()}

		if profileType == TypeCPU {
			s.Label[labelSourceEventPeriod] = []string{periodStr}
		}

		p.Sample = append(p.Sample, s)
	}

	// Build a single Mapping so pprof tools treat locations as user-space.
	p.Mapping = []*profile.Mapping{{ID: 1, File: "php-agent"}}

	walkPaths(t.Root, []*Segment{}, addSample)

	p.Function = funcs
	p.Location = locales
	return p
}

// walkPaths runs cb on every root->leaf path. The path passed to cb is ordered
// leaf-first (path[0] is the deepest segment, path[len-1] is the root), as
// pprof expects (callee before caller).
func walkPaths(root *Segment, prefix []*Segment, cb func(path []*Segment)) {
	if root == nil {
		return
	}
	// prefix is root-first; append the current node to keep growing the path
	// from the root downward.
	path := append([]*Segment{}, prefix...)
	path = append(path, root)

	if len(root.Children) == 0 {
		// leaf: reverse to leaf-first for the callback
		rev := make([]*Segment, len(path))
		for i := range path {
			rev[i] = path[len(path)-1-i]
		}
		cb(rev)
		return
	}
	for _, c := range root.Children {
		walkPaths(c, path, cb)
	}
}

// rootDurationNanos returns the total wall duration of the root segment in ns.
func rootDurationNanos(root *Segment) int64 {
	if root == nil {
		return 0
	}
	return int64(root.DurationMs * float64(time.Millisecond))
}
