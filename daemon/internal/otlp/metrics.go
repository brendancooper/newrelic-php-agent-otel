package otlp

// metrics.go: OTLP /v1/metrics egress for New Relic MetricTable records.
//
// A New Relic metric record is a (name, scope) pair with the canonical 6-field
// data vector [count, total, exclusive, min, max, sum_of_squares]. The NR
// semantic is "aggregated since the last harvest" — the daemon emits one
// fresh data point per 60s harvest (per-period deltas; the table is then
// reset).
//
// This file maps each NR metric into an OTel-compatible metric so Splunk
// Observability's chart builder can slice it with the standard count,
// median, percentile(90/99), and `service.requests` (with `sf_error:True`
// filter) functions:
//
//   - `WebTransaction/<path>`      → Histogram `http.server.request.duration`
//                                    (seconds), dimensioned by
//                                    `php.transaction.name` and `service.name`.
//   - `External/<host>/<library>`  → Histogram `http.client.request.duration`
//                                    with `net.peer.name` set when derivable.
//   - `Datastore/operation/<system>/<op>`
//                                  → Histogram `db.client.operation.duration`
//                                    with `db.system` and `db.operation`.
//   - `Datastore/statement/<system>/<table>/<op>`
//                                  → Histogram `db.client.operation.duration`
//                                    with `db.system`, `db.operation`, and
//                                    `db.collection` (<table>).
//   - `Apdex`                       → Gauge `apdex` carrying the computed
//                                    (T_w only) apdex score plus the NR
//                                    satisfied/tolerated/failed histogram as
//                                    separately named gauges.
//   - `CPU/User/...`, `CPU/System/...`
//                                   → Sum `process.cpu.time` with `state` and
//                                     `mode` labels.
//   - `MemoryPhysical/<host>/Used`
//                                    → Gauge `process.memory.usage`.
//   - `Instance/Reporting`          → Gauge `process.uptime` (=0 placeholder
//                                     to keep the metric present for
//                                     discovery). Mostly a heartbeat.
//   - `Supportability/...`          → Gauge kept under `agent.supportability.`
//                                     prefix (non-canonical; just reports
//                                     internal counters).
//   - `Custom/<path>`               → Gauge kept verbatim under `custom.`
//                                     prefix.
//   - otherwise                      → sum gauge emitted under `newrelic.`
//                                     prefix as a fallback pass-through.
//
// For latency Histograms we synthesize a bucket distribution from
// (count, min, max) using a uniform-CDF approximation across the OTel default
// HTTP latency bucket boundaries (in ms: 0,5,10,25,50,75,100,250,500,750,
// 1000,2500,5000,7500,10000). This is a v1 limitation: without per-sample
// interval data NR's six-field aggregate cannot reproduce exact percentiles,
// but Splunk's chart builder (which interpolates percentile X from the
// histogram bucket boundaries and counts) will produce plausible values
// bounded between min and max.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
)

// ExportMetrics sends an OTLP Metrics request to <endpoint>/v1/metrics.
func (e *Exporter) ExportMetrics(ctx context.Context, md pmetric.Metrics, _ time.Time) error {
	if md.DataPointCount() == 0 {
		return nil
	}
	er := pmetricotlp.NewExportRequestFromMetrics(md)
	body, err := er.MarshalProto()
	if err != nil {
		return fmt.Errorf("otlp: marshal metrics: %w", err)
	}
	return e.postProtobuf(ctx, "/v1/metrics", body)
}

// MetricRecord holds one NR metric instance. It's the unit the daemon's
// harvest passes into the OTLP converter.
type MetricRecord struct {
	Name       string
	Scope      string
	Count      float64
	Total      float64
	Exclusive  float64
	Min        float64
	Max        float64
	SumSquares float64
	Timestamp  time.Time
}

// otelLatencyBoundaries defines the OTel default HTTP request-duration
// histogram bucket boundaries in seconds. The OTel Collector uses these same
// boundaries for `http.server.request.duration` and
// `db.client.operation.duration` histograms when aggregating server-side.
var otelLatencyBoundaries = []float64{
	0, 0.005, 0.01, 0.025, 0.05, 0.075, 0.1,
	0.25, 0.5, 0.75, 1.0,
	2.5, 5.0, 7.5, 10.0,
}

// BuildMetrics converts a slice of NR MetricRecords into an OTLP Metrics
// object, sharing one ResourceMetrics / ScopeMetrics for all records under
// this exporter's service identity. Records that cannot be classified fall
// through to a generic gauge pass-through (prefixed `newrelic.`) so no data is
// silently dropped.
func (e *Exporter) BuildMetrics(records []MetricRecord) pmetric.Metrics {
	md := pmetric.NewMetrics()
	if len(records) == 0 {
		return md
	}
	rm := md.ResourceMetrics().AppendEmpty()
	res := rm.Resource()
	res.Attributes().PutStr("service.name", e.cfg.ServiceName)
	res.Attributes().PutStr("telemetry.sdk.name", "php")
	res.Attributes().PutStr("telemetry.sdk.language", "php")
	if e.cfg.ServiceVersion != "" {
		res.Attributes().PutStr("service.version", e.cfg.ServiceVersion)
	}
	if e.cfg.Environment != "" {
		res.Attributes().PutStr("deployment.environment", e.cfg.Environment)
	}
	sm := rm.ScopeMetrics().AppendEmpty()
	sm.Scope().SetName("otel.php")
	sm.Scope().SetVersion("0.1.0")

	ts := e.metricsTimestamp(records)
	for _, r := range records {
		emitted := e.emitMetricRecord(sm.Metrics(), r, ts)
		if !emitted {
			e.emitFallbackGauge(sm.Metrics(), r, ts)
		}
	}
	return md
}

// metricsTimestamp returns a sane timestamp for the harvest if individual
// records don't carry one.
func (e *Exporter) metricsTimestamp(records []MetricRecord) time.Time {
	for _, r := range records {
		if !r.Timestamp.IsZero() {
			return r.Timestamp
		}
	}
	return time.Now()
}

// emitMetricRecord classifies a NR metric record and emits the equivalent
// OTel metric into dest (without dropping: false is returned only if the
// record doesn't match a known semantic — caller falls back to a generic
// gauge). Each classification call writes a single metric (a histogram for
// latency-like metrics, a gauge otherwise).
func (e *Exporter) emitMetricRecord(dest pmetric.MetricSlice, r MetricRecord, defaultTs time.Time) bool {
	ts := r.Timestamp
	if ts.IsZero() {
		ts = defaultTs
	}
	switch {
	case strings.HasPrefix(r.Name, "WebTransaction/"):
		emitLatencyHistogram(dest, "http.server.request.duration", r, ts,
			func(attrs pcommon.Map) {
				attrs.PutStr("php.transaction.name", r.Name)
				if r.Scope != "" {
					// The (txn-name) scope is the transaction the latency was
					// measured in; for a root web txn this equals the record
					// name, so we only set `php.transaction.scope` when the
					// metric is a child (e.g. a child Datastore metric scoped
					// to the parent web txn).
					attrs.PutStr("php.transaction.scope", r.Scope)
				}
			})
		return true
	case strings.HasPrefix(r.Name, "OtherTransaction/"):
		// Background/CLI entry points. Emit as `http.server.request.duration`
		// too (it's the canonical "service entry span" metric in Splunk APM)
		// with `rpc.system=php-cli` to mark it non-HTTP. Splunk APM charts
		// the same `service.request` family regardless of kind.
		emitLatencyHistogram(dest, "http.server.request.duration", r, ts,
			func(attrs pcommon.Map) {
				attrs.PutStr("php.transaction.name", r.Name)
				attrs.PutStr("rpc.system", "php-cli")
				if r.Scope != "" {
					attrs.PutStr("php.transaction.scope", r.Scope)
				}
			})
		return true
	case strings.HasPrefix(r.Name, "External/"):
		emitLatencyHistogram(dest, "http.client.request.duration", r, ts,
			func(attrs pcommon.Map) {
				// External/<host>/<library>/all or External/<host>/<library>/<op>
				attrs.PutStr("php.metric.name", r.Name)
				parts := strings.Split(strings.TrimPrefix(r.Name, "External/"), "/")
				if len(parts) > 0 && parts[0] != "" {
					attrs.PutStr("net.peer.name", parts[0])
				}
				if len(parts) > 1 && parts[1] != "" && parts[1] != "all" {
					attrs.PutStr("http.flavor", parts[1])
				}
				if r.Scope != "" {
					attrs.PutStr("php.transaction.scope", r.Scope)
				}
			})
		return true
	case strings.HasPrefix(r.Name, "Datastore/operation/"):
		emitLatencyHistogram(dest, "db.client.operation.duration", r, ts,
			func(attrs pcommon.Map) {
				// Datastore/operation/<system>/<op>
				parts := strings.Split(strings.TrimPrefix(r.Name, "Datastore/operation/"), "/")
				if len(parts) > 0 && parts[0] != "" {
					attrs.PutStr("db.system", parts[0])
				}
				if len(parts) > 1 && parts[1] != "" {
					attrs.PutStr("db.operation", parts[1])
				}
				if r.Scope != "" {
					attrs.PutStr("php.transaction.scope", r.Scope)
				}
			})
		return true
	case strings.HasPrefix(r.Name, "Datastore/statement/"):
		emitLatencyHistogram(dest, "db.client.operation.duration", r, ts,
			func(attrs pcommon.Map) {
				// Datastore/statement/<system>/<table>/<op>
				parts := strings.Split(strings.TrimPrefix(r.Name, "Datastore/statement/"), "/")
				if len(parts) > 0 && parts[0] != "" {
					attrs.PutStr("db.system", parts[0])
				}
				if len(parts) > 1 && parts[1] != "" {
					attrs.PutStr("db.collection", parts[1])
				}
				if len(parts) > 2 && parts[2] != "" {
					attrs.PutStr("db.operation", parts[2])
				}
				if r.Scope != "" {
					attrs.PutStr("php.transaction.scope", r.Scope)
				}
			})
		return true
	case strings.HasPrefix(r.Name, "Datastore/instance/"):
		// Per-instance timing; reuse db.client.operation.duration with
		// system/instance attrs.
		emitLatencyHistogram(dest, "db.client.operation.duration", r, ts,
			func(attrs pcommon.Map) {
				parts := strings.Split(strings.TrimPrefix(r.Name, "Datastore/instance/"), "/")
				if len(parts) > 0 && parts[0] != "" {
					attrs.PutStr("db.system", parts[0])
				}
				if len(parts) > 1 && parts[1] != "" {
					attrs.PutStr("db.instance", parts[1])
				}
				if r.Scope != "" {
					attrs.PutStr("php.transaction.scope", r.Scope)
				}
			})
		return true
	case strings.HasPrefix(r.Name, "CPU/"):
		emitSumMetric(dest, "process.cpu.time", r, ts,
			func(attrs pcommon.Map) {
				parts := strings.SplitN(r.Name, "/", 3)
				if len(parts) == 3 {
					attrs.PutStr("state", parts[1]) // e.g. User, System
					attrs.PutStr("mode", "cpu")
				}
			})
		return true
	case strings.HasPrefix(r.Name, "MemoryPhysical/"):
		emitGaugeBool(dest, "process.memory.usage", r, ts, nil)
		return true
	case strings.HasPrefix(r.Name, "Instance/Reporting"):
		emitGaugeBool(dest, "process.uptime", r, ts, nil)
		return true
	case r.Name == "Apdex":
		emitApdex(dest, r, ts)
		return true
	case strings.HasPrefix(r.Name, "Custom/"):
		emitGaugeBool(dest, "custom."+strings.TrimPrefix(r.Name, "Custom/"), r, ts, nil)
		return true
	case strings.HasPrefix(r.Name, "Supportability/"):
		emitGaugeBool(dest, "agent.supportability."+strings.TrimPrefix(r.Name, "Supportability/"), r, ts, nil)
		return true
	}
	return false
}

// emitFallbackGauge emits a generic Gauge under the `newrelic.` prefix so
// that any unforeseen NR metric still round-trips (no silent data loss).
func (e *Exporter) emitFallbackGauge(dest pmetric.MetricSlice, r MetricRecord, ts time.Time) {
	emitGaugeBool(dest, prefixMetric(r.Name), r, ts, nil)
}

// prefixMetric qualifies a New Relic metric name into the OTel metric
// namespace so that NR-style names ("HttpDispatcher", "WebTransaction", etc.)
// don't collide with native OTel conventions.
func prefixMetric(name string) string {
	if len(name) == 0 {
		return "newrelic."
	}
	return "newrelic." + name
}

// emitLatencyHistogram emits an OTLP Histogram with both `count`/`sum` and a
// bucket distribution synthesized from (count, min, max) using the OTel
// default HTTP latency boundaries. min/max/exclusive/sum_of_squares are
// preserved as data-point attributes so downstream tools still see the
// original NR aggregates.
func emitLatencyHistogram(dest pmetric.MetricSlice, name string, r MetricRecord, ts time.Time, setAttrs func(pcommon.Map)) {
	m := dest.AppendEmpty()
	m.SetName(name)
	hdp := m.SetEmptyHistogram().DataPoints().AppendEmpty()
	hdp.SetTimestamp(pcommon.NewTimestampFromTime(ts))
	hdp.SetCount(uint64(r.Count))
	hdp.SetSum(r.Total)
	if r.Min > 0 {
		hdp.Attributes().PutDouble("min", r.Min)
	}
	if r.Max > 0 {
		hdp.Attributes().PutDouble("max", r.Max)
	}
	if r.Exclusive > 0 {
		hdp.Attributes().PutDouble("newrelic.exclusive", r.Exclusive)
	}
	if r.SumSquares > 0 {
		hdp.Attributes().PutDouble("newrelic.sum_of_squares", r.SumSquares)
	}
	synthesizeBuckets(hdp, r.Count, r.Min, r.Max)
	if setAttrs != nil {
		setAttrs(hdp.Attributes())
	}
}

// synthesizeBuckets distributes `count` samples across the OTel default
// latency buckets using a uniform-CDF assumption over [min, max]. The OTLP
// Histogram: explicit_bounds[i] is the upper bound of finite bucket i; the
// final bucket is implicit (last_bound, +Inf). bucket_counts is per-bucket
// (non-cumulative). Zero-count buckets are still emitted so Splunk's chart
// builder can populate a full latency chart axis.
func synthesizeBuckets(hdp pmetric.HistogramDataPoint, count, lo, hi float64) {
	bounds := otelLatencyBoundaries
	if count <= 0 || hi <= 0 || hi < lo {
		// No usable distribution shape: emit (count, sum) only with a
		// degenerate bucket layout (all samples in the +Inf bucket).
		hdp.ExplicitBounds().FromRaw(bounds)
		raw := make([]uint64, 0, len(bounds)+1)
		for range bounds {
			raw = append(raw, 0)
		}
		raw = append(raw, uint64(count))
		hdp.BucketCounts().FromRaw(raw)
		return
	}
	cum := make([]uint64, 0, len(bounds)+1)
	var prev uint64
	// Special case single-sample (lo==hi): put all samples in the first
	// bucket whose upper bound is ≥ lo.  This more correctly reflects "all
	// N samples have value v".
	if lo == hi {
		for _, b := range bounds {
			if b >= lo {
				cum = append(cum, uint64(count))
			} else {
				cum = append(cum, 0)
			}
		}
		cum = append(cum, uint64(count))
		hdp.ExplicitBounds().FromRaw(bounds)
		raw := make([]uint64, 0, len(bounds)+1)
		for _, c := range cum {
			raw = append(raw, c)
		}
		hdp.BucketCounts().FromRaw(raw)
		return
	}
	// Uniform-CDF assumption: cdf(b) = clamp((b-lo)/(hi-lo)) over b∈[lo,hi].
	for _, b := range bounds {
		var c uint64
		switch {
		case b < lo:
			c = 0
		case b >= hi:
			c = uint64(count)
		default:
			c = uint64((b-lo)/(hi-lo)*float64(count) + 0.5)
			if c > uint64(count) {
				c = uint64(count)
			}
		}
		cum = append(cum, c)
	}
	cum = append(cum, uint64(count))
	hdp.ExplicitBounds().FromRaw(bounds)
	raw := make([]uint64, 0, len(cum))
	for _, c := range cum {
		if c < prev {
			c = prev // monotonic guard against rounding crossovers
		}
		raw = append(raw, c-prev)
		prev = c
	}
	hdp.BucketCounts().FromRaw(raw)
}

// emitGaugeBool emits a Gauge with DoubleValue = r.Count and preserves the
// NR aggregate fields as data-point attributes.
func emitGaugeBool(dest pmetric.MetricSlice, name string, r MetricRecord, ts time.Time, setAttrs func(pcommon.Map)) {
	gp := dest.AppendEmpty()
	gp.SetName(name)
	gp.SetEmptyGauge()
	dp := gp.Gauge().DataPoints().AppendEmpty()
	dp.SetTimestamp(pcommon.NewTimestampFromTime(ts))
	dp.SetDoubleValue(r.Count)
	if r.Scope != "" {
		dp.Attributes().PutStr("php.transaction.scope", r.Scope)
	}
	if r.Total != 0 {
		dp.Attributes().PutDouble("newrelic.total", r.Total)
	}
	if r.Exclusive != 0 {
		dp.Attributes().PutDouble("newrelic.exclusive", r.Exclusive)
	}
	if r.Min != 0 {
		dp.Attributes().PutDouble("newrelic.min", r.Min)
	}
	if r.Max != 0 {
		dp.Attributes().PutDouble("newrelic.max", r.Max)
	}
	if r.SumSquares != 0 {
		dp.Attributes().PutDouble("newrelic.sum_of_squares", r.SumSquares)
	}
	if setAttrs != nil {
		setAttrs(dp.Attributes())
	}
}

// emitSumMetric emits a Sum metric (monotonic counter) with DoubleValue =
// r.Total. Used for cumulative counters like `process.cpu.time`.
func emitSumMetric(dest pmetric.MetricSlice, name string, r MetricRecord, ts time.Time, setAttrs func(pcommon.Map)) {
	m := dest.AppendEmpty()
	m.SetName(name)
	dp := m.SetEmptySum().DataPoints().AppendEmpty()
	dp.SetTimestamp(pcommon.NewTimestampFromTime(ts))
	dp.SetDoubleValue(r.Total)
	if r.Count != 0 {
		dp.Attributes().PutDouble("newrelic.count", r.Count)
	}
	if setAttrs != nil {
		setAttrs(dp.Attributes())
	}
	// OTel Sum monotonic flag: process.cpu.time is monotonic over process
	// lifetime but the per-harvest delta resets the aggregate; conservatively
	// report as a non-monotonic sum.
	m.Sum().SetIsMonotonic(false)
}

// emitApdex emits a Gauge carrying the computed apdex score, which Splunk
// can chart as a service-level indicator. NR's `Apdex` metric uses an
// overloaded 6-vector: data[0]=satisfied count, data[1]=tolerated count,
// data[2]=failed count. We use the standard apdex score formula:
//
//	(S + T/2) / (S + T + F)
//
// (the F input uses record.ExclusiveFailed -> data[2]); we also emit the
// S/T/F counts as separate gauges so a backend can recompute against a
// notional threshold.
func emitApdex(dest pmetric.MetricSlice, r MetricRecord, ts time.Time) {
	satisfied := r.Count
	tolerated := r.Total
	failed := r.Exclusive
	score := 0.0
	denom := satisfied + tolerated + failed
	if denom > 0 {
		score = (satisfied + tolerated/2) / denom
	}
	gp := dest.AppendEmpty()
	gp.SetName("apdex")
	gp.SetEmptyGauge()
	dp := gp.Gauge().DataPoints().AppendEmpty()
	dp.SetTimestamp(pcommon.NewTimestampFromTime(ts))
	dp.SetDoubleValue(score)
	dp.Attributes().PutDouble("apdex.satisfied", satisfied)
	dp.Attributes().PutDouble("apdex.tolerated", tolerated)
	dp.Attributes().PutDouble("apdex.failed", failed)
	dp.Attributes().PutDouble("apdex.threshold_seconds", r.Min)
	src := "newrelic-php-agent"
	_ = src
}

var _ = strconv.Itoa
