package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
)

func TestBuildMetrics_MapsNRVector(t *testing.T) {
	exp, _ := New(Config{Endpoint: "http://127.0.0.1:4318", ServiceName: "php-test", Environment: "smoke"})
	ts := time.UnixMilli(1_700_000_000_000)
	records := []MetricRecord{
		{
			Name:       "WebTransaction/Function/index",
			Scope:      "",
			Count:      5,
			Total:      1.25,
			Exclusive:  1.0,
			Min:        0.10,
			Max:        0.50,
			SumSquares: 0.4,
			Timestamp:  ts,
		},
		{
			Name:       "Datastore/operation/MySQL/select",
			Scope:      "WebTransaction/Function/index",
			Count:      10,
			Total:      2.5,
			Exclusive:  2.5,
			Min:        0.05,
			Max:        0.50,
			SumSquares: 1.0,
			Timestamp:  ts,
		},
	}
	md := exp.BuildMetrics(records)
	if md.DataPointCount() != 2 {
		t.Fatalf("DataPointCount = %d, want 2", md.DataPointCount())
	}
	rm := md.ResourceMetrics()
	if rm.Len() != 1 {
		t.Fatalf("ResourceMetrics.Len = %d, want 1", rm.Len())
	}
	svc, _ := rm.At(0).Resource().Attributes().Get("service.name")
	if svc.Str() != "php-test" {
		t.Errorf("service.name = %q", svc.Str())
	}
	sm := rm.At(0).ScopeMetrics()
	if sm.Len() != 1 || sm.At(0).Scope().Name() != "otel.php" {
		t.Fatalf("scope = %v", sm)
	}
	metrics := sm.At(0).Metrics()
	if metrics.Len() != 2 {
		t.Fatalf("Metrics.Len = %d, want 2", metrics.Len())
	}

	// First record: WebTransaction → http.server.request.duration Histogram.
	m0 := metrics.At(0)
	if m0.Name() != "http.server.request.duration" {
		t.Errorf("name = %q, want http.server.request.duration", m0.Name())
	}
	if m0.Type() != pmetric.MetricTypeHistogram {
		t.Fatalf("type = %v want histogram", m0.Type())
	}
	dp0 := m0.Histogram().DataPoints().At(0)
	if got := dp0.Timestamp().AsTime().UnixMilli(); got != ts.UnixMilli() {
		t.Errorf("start ts = %v", dp0.Timestamp().AsTime())
	}
	if dp0.Count() != 5 {
		t.Errorf("count = %d, want 5", dp0.Count())
	}
	if dp0.Sum() != 1.25 {
		t.Errorf("sum = %v, want 1.25", dp0.Sum())
	}
	if v, _ := dp0.Attributes().Get("php.transaction.name"); v.Str() != "WebTransaction/Function/index" {
		t.Errorf("php.transaction.name = %q", v.AsString())
	}
	// min/max preserved as attributes for downstream traceability
	if v, _ := dp0.Attributes().Get("min"); v.Double() != 0.10 {
		t.Errorf("min = %v, want 0.10", v.Double())
	}
	if v, _ := dp0.Attributes().Get("max"); v.Double() != 0.50 {
		t.Errorf("max = %v, want 0.50", v.Double())
	}
	// Scope-less web-txn metric should NOT have a php.transaction.scope label.
	if _, ok := dp0.Attributes().Get("php.transaction.scope"); ok {
		t.Error("scope-less metric should not set php.transaction.scope")
	}
	// Bounds/counts must be set so Splunk chart builder can interpolate.
	if dp0.ExplicitBounds().Len() != len(otelLatencyBoundaries) {
		t.Fatalf("ImplicitBounds Len = %d, want %d", dp0.ExplicitBounds().Len(), len(otelLatencyBoundaries))
	}
	if dp0.BucketCounts().Len() != dp0.ExplicitBounds().Len()+1 {
		t.Fatalf("BucketCounts Len = %d, want %d (+Inf bucket)", dp0.BucketCounts().Len(), dp0.ExplicitBounds().Len()+1)
	}
	// Sum over per-bucket counts == total count.
	var total uint64
	for i := 0; i < dp0.BucketCounts().Len(); i++ {
		total += dp0.BucketCounts().At(i)
	}
	if total != 5 {
		t.Errorf("bucket counts sum = %d, want 5", total)
	}

	// Second record: Datastore/operation → db.client.operation.duration.
	m1 := metrics.At(1)
	if m1.Name() != "db.client.operation.duration" {
		t.Errorf("name = %q, want db.client.operation.duration", m1.Name())
	}
	if m1.Type() != pmetric.MetricTypeHistogram {
		t.Fatalf("type = %v want histogram", m1.Type())
	}
	dp1 := m1.Histogram().DataPoints().At(0)
	if dp1.Count() != 10 {
		t.Errorf("count = %d, want 10", dp1.Count())
	}
	if dp1.Sum() != 2.5 {
		t.Errorf("sum = %v, want 2.5", dp1.Sum())
	}
	if v, _ := dp1.Attributes().Get("db.system"); v.Str() != "MySQL" {
		t.Errorf("db.system = %q, want MySQL", v.AsString())
	}
	if v, _ := dp1.Attributes().Get("db.operation"); v.Str() != "select" {
		t.Errorf("db.operation = %q, want select", v.AsString())
	}
	// Scope of a child scope should be populated.
	if v, _ := dp1.Attributes().Get("php.transaction.scope"); v.Str() != "WebTransaction/Function/index" {
		t.Errorf("php.transaction.scope = %q", v.AsString())
	}
}

// TestBuildMetrics_SemanticNames covers the metrics-path name-classification
// and OTel type selection for the major NR metric categories: External HTTP,
// Apdex, CPU, MemoryPhysical, Instance/Reporting, Custom/*, Supportability/*,
// and the `newrelic.` fallback pass-through.
func TestBuildMetrics_SemanticNames(t *testing.T) {
	exp, _ := New(Config{Endpoint: "http://127.0.0.1:4318", ServiceName: "php-test"})
	ts := time.UnixMilli(1_700_000_000_000)
	records := []MetricRecord{
		{Name: "External/api.example.com/curl/all", Scope: "WebTransaction/Function/index", Count: 4, Total: 0.4, Min: 0.05, Max: 0.15, Timestamp: ts},
		{Name: "Apdex", Count: 5, Total: 2.0, Exclusive: 1.0, Min: 0.5, Max: 0.5, Timestamp: ts},
		{Name: "CPU/User/Utilization", Count: 1, Total: 0.42, Exclusive: 0, Min: 0, Max: 0, Timestamp: ts},
		{Name: "MemoryPhysical/system/Used", Count: 100.0, Total: 0, Timestamp: ts},
		{Name: "Instance/Reporting", Count: 1, Timestamp: ts},
		{Name: "Custom/MyApp/Batch/Duration", Count: 2, Total: 0.2, Min: 0.05, Max: 0.15, Timestamp: ts},
		{Name: "Supportability/Agent/Version", Count: 1, Timestamp: ts},
		{Name: "UnknownMetric/Foo/Bar", Count: 7, Total: 0, Timestamp: ts},
	}
	md := exp.BuildMetrics(records)
	sm := md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
	if sm.Len() != len(records) {
		t.Fatalf("Metrics.Len = %d, want %d", sm.Len(), len(records))
	}
	type want struct {
		name   string
		mtype  pmetric.MetricType
		isHist bool
		hasSum bool // for non-histogram with a non-count value
	}
	wants := []want{
		{"http.client.request.duration", pmetric.MetricTypeHistogram, true, false},
		{"apdex", pmetric.MetricTypeGauge, false, false},
		{"process.cpu.time", pmetric.MetricTypeSum, false, true},
		{"process.memory.usage", pmetric.MetricTypeGauge, false, false},
		{"process.uptime", pmetric.MetricTypeGauge, false, false},
		{"custom.MyApp/Batch/Duration", pmetric.MetricTypeGauge, false, false},
		{"agent.supportability.Agent/Version", pmetric.MetricTypeGauge, false, false},
		{"newrelic.UnknownMetric/Foo/Bar", pmetric.MetricTypeGauge, false, false},
	}
	for i, w := range wants {
		m := sm.At(i)
		if m.Name() != w.name {
			t.Errorf("[%d] name = %q, want %q", i, m.Name(), w.name)
		}
		if m.Type() != w.mtype {
			t.Errorf("[%d] %s type = %v, want %v", i, w.name, m.Type(), w.mtype)
		}
		if w.isHist {
			if m.Histogram().DataPoints().At(0).ExplicitBounds().Len() != len(otelLatencyBoundaries) {
				t.Errorf("[%d] %s ExplicitBounds.Len = %d, want %d",
					i, w.name, m.Histogram().DataPoints().At(0).ExplicitBounds().Len(), len(otelLatencyBoundaries))
			}
		}
	}
	// Apdex carries the computed score on the data point; (5 + 2/2)/(5+2+1)=0.75
	ap := sm.At(1).Gauge().DataPoints().At(0)
	if got := ap.DoubleValue(); got != 0.75 {
		t.Errorf("apdex score = %v, want 0.75", got)
	}
	if v, _ := ap.Attributes().Get("apdex.satisfied"); v.Double() != 5 {
		t.Errorf("apdex.satisfied = %v", v.Double())
	}
	if v, _ := ap.Attributes().Get("apdex.tolerated"); v.Double() != 2 {
		t.Errorf("apdex.tolerated = %v", v.Double())
	}
	if v, _ := ap.Attributes().Get("apdex.failed"); v.Double() != 1 {
		t.Errorf("apdex.failed = %v", v.Double())
	}
	// External carry net.peer.name derived from metric name.
	ext := sm.At(0).Histogram().DataPoints().At(0)
	if v, _ := ext.Attributes().Get("net.peer.name"); v.Str() != "api.example.com" {
		t.Errorf("net.peer.name = %q", v.AsString())
	}
	if v, _ := ext.Attributes().Get("http.flavor"); v.Str() != "curl" {
		t.Errorf("http.flavor = %q", v.AsString())
	}
	// CPU total = 0.42 preserved on the Sum.
	cpu := sm.At(2).Sum().DataPoints().At(0)
	if got := cpu.DoubleValue(); got != 0.42 {
		t.Errorf("process.cpu.time value = %v, want 0.42", got)
	}
	if v, _ := cpu.Attributes().Get("state"); v.Str() != "User" {
		t.Errorf("state = %q", v.AsString())
	}
}

func TestExportMetrics_LiveRoundtrip(t *testing.T) {
	if testing.Short() {
		t.Skip("live mock test in short mode")
	}
	ln, srv, got := startMockOTLP(t, "/v1/metrics")
	defer ln.Close()
	defer srv.Close()

	exp, _ := New(Config{Endpoint: "http://" + ln.Addr().String(), ServiceName: "php-test"})
	records := []MetricRecord{
		{Name: "WebTransaction/foo", Count: 3, Timestamp: time.UnixMilli(1_700_000_000_000)},
		{Name: "HttpDispatcher", Scope: "Tx", Count: 7, Total: 1.0, Timestamp: time.UnixMilli(1_700_000_000_000)},
	}
	md := exp.BuildMetrics(records)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exp.ExportMetrics(ctx, md, time.Now()); err != nil {
		t.Fatalf("ExportMetrics: %v", err)
	}

	select {
	case body := <-got:
		er := pmetricotlp.NewExportRequest()
		if err := er.UnmarshalProto(body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got := er.Metrics().DataPointCount(); got != 2 {
			t.Fatalf("roundtrip DataPointCount = %d, want 2", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for /v1/metrics POST")
	}
}

func TestExportMetrics_EmptyNoOp(t *testing.T) {
	exp, _ := New(Config{Endpoint: "http://127.0.0.1:4318", ServiceName: "x"})
	if err := exp.ExportMetrics(t.Context(), pmetric.NewMetrics(), time.Now()); err != nil {
		t.Fatalf("ExportMetrics(empty) = %v", err)
	}
}

var _ = bytes.NewReader
var _ = gzip.DefaultCompression
var _ = io.EOF
var _ = http.StatusOK
