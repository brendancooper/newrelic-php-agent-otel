// Command smoke is an end-to-end harness for the OTLP/PHP profiling and
// traces/metrics egress.
//
// It starts an in-process mock OTLP/HTTP collector that captures /v1/logs,
// /v1/traces, and /v1/metrics requests, then exercises all three OTLP signals
// via the daemon's internal/otlp.Exporter:
//
//   - profiling: builds a pprof Profile from a known PHP segment-tree JSON
//     (from internal/pprof), POSTs as OTLP Logs, base64+gzip decodes the
//     LogRecord body, re-parses with github.com/google/pprof, and asserts
//     the Splunk AlwaysOn Profiling LogRecord contract.
//   - traces: builds a synthetic NR span-event JSON payload, POSTs as OTLP
//     Traces, and asserts the converted span carries trace/span/parent IDs,
//     name, timestamps, status, and the merged attributes.
//   - metrics: builds synthetic NR MetricRecords, POSTs as OTLP Metrics, and
//     asserts the data points carry the count value and supporting attributes.
//
// Exit code 0 on success, non-zero on failure. Run with:
//
//	go run ./cmd/smoke
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"encoding/json"

	"github.com/google/pprof/profile"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"

	"github.com/newrelic/newrelic-php-agent/daemon/internal/otlp"
	"github.com/newrelic/newrelic-php-agent/daemon/internal/pprof"
)

const sampleTraceData = `[
  [0, {}, {}, [0, 2000, "` + "`" + `0", {}, [
    [0, 9, "` + "`" + `1", {}, [
      [1, 2, "` + "`" + `2", {}, []],
      [3, 4, "` + "`" + `3", {}, []]
    ]],
    [10, 60, "` + "`" + `4", {}, [
      [12, 30, "` + "`" + `5", {}, []],
      [40, 55, "PDO->query", {}, []]
    ]]
  ]]],
  {"agentAttributes": ["agent_attributes"], "userAttributes": ["user_attributes"], "intrinsics": ["intrinsics"]},
  ["WebTransaction/Function/index", "doWork", "dbQuery", "httpCall", "outerWrap", "innerSleep", "WebTransaction/*"]
]`

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "address for the mock collector")
	flag.Parse()

	if err := run(*addr); err != nil {
		fmt.Fprintf(os.Stderr, "smoke: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("smoke: PASS")
}

type captured struct {
	logs    chan *plog.Logs
	traces  chan ptrace.Traces
	metrics chan pmetric.Metrics
}

func run(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	cap := &captured{
		logs:    make(chan *plog.Logs, 1),
		traces:  make(chan ptrace.Traces, 1),
		metrics: make(chan pmetric.Metrics, 1),
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if r.Header.Get("Content-Encoding") == "gzip" {
			gr, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			body, err = io.ReadAll(gr)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
		}
		switch r.URL.Path {
		case "/v1/logs":
			er := plogotlp.NewExportRequest()
			if err := er.UnmarshalProto(body); err != nil {
				http.Error(w, "unmarshal: "+err.Error(), 400)
				return
			}
			ld := plog.NewLogs()
			er.Logs().CopyTo(ld)
			cap.logs <- &ld
		case "/v1/traces":
			er := ptraceotlp.NewExportRequest()
			if err := er.UnmarshalProto(body); err != nil {
				http.Error(w, "unmarshal: "+err.Error(), 400)
				return
			}
			cap.traces <- er.Traces()
		case "/v1/metrics":
			er := pmetricotlp.NewExportRequest()
			if err := er.UnmarshalProto(body); err != nil {
				http.Error(w, "unmarshal: "+err.Error(), 400)
				return
			}
			cap.metrics <- er.Metrics()
		default:
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(200)
	})}

	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Serve(ln) }()
	defer func() {
		_ = srv.Shutdown(context.Background())
		<-srvErr
	}()

	endpoint := "http://" + ln.Addr().String()
	log.Printf("mock collector listening on %s", endpoint)

	exp, err := otlp.New(otlp.Config{
		Endpoint:    endpoint,
		ServiceName: "php-smoke",
		Environment: "smoke",
		// Compression: default gzip.
	})
	if err != nil {
		return fmt.Errorf("otlp.New: %w", err)
	}

	if err := runProfiling(exp, cap); err != nil {
		return err
	}
	if err := runTraces(exp, cap); err != nil {
		return err
	}
	if err := runMetrics(exp, cap); err != nil {
		return err
	}
	return nil
}

// runProfiling exercises the OTLP Logs profiling path.
func runProfiling(exp *otlp.Exporter, cap *captured) error {
	tr, err := pprof.DecodeTrace([]byte(sampleTraceData), "WebTransaction/Function/index", float64(time.Now().Add(-time.Second).UnixMilli()))
	if err != nil {
		return fmt.Errorf("DecodeTrace: %w", err)
	}
	prof := pprof.BuildProfile(tr, pprof.TypeCPU, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exp.ExportProfiles(ctx, []otlp.ProfileRecord{
		{Profile: prof, ProfileType: "cpu"},
	}, time.Now()); err != nil {
		return fmt.Errorf("ExportProfiles: %w", err)
	}

	select {
	case ld := <-cap.logs:
		return assertLogs(ld)
	case <-time.After(5 * time.Second):
		return errors.New("timeout: mock collector received no /v1/logs request")
	}
}

// runTraces exercises the OTLP Traces path with a synthetic NR span event.
func runTraces(exp *otlp.Exporter, cap *captured) error {
	span := buildSmokeSpanEvent()
	td, err := exp.BuildTraces([][]byte{span}, time.Now())
	if err != nil {
		return fmt.Errorf("BuildTraces: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exp.ExportTraces(ctx, td, time.Now()); err != nil {
		return fmt.Errorf("ExportTraces: %w", err)
	}
	select {
	case td2 := <-cap.traces:
		return assertTraces(td2)
	case <-time.After(5 * time.Second):
		return errors.New("timeout: mock collector received no /v1/traces request")
	}
}

// runMetrics exercises the OTLP Metrics path with synthetic NR metric records
// covering the latency (Web/Datastore) and Count pass-through paths.
func runMetrics(exp *otlp.Exporter, cap *captured) error {
	records := []otlp.MetricRecord{
		{Name: "WebTransaction/Function/index", Count: 3, Total: 0.9, Min: 0.05, Max: 0.5, Timestamp: time.UnixMilli(1_700_000_000_000)},
		{Name: "Datastore/operation/MySQL/select", Scope: "WebTransaction/Function/index", Count: 12, Total: 2.0, Min: 0.01, Max: 0.5, Timestamp: time.UnixMilli(1_700_000_000_000)},
	}
	md := exp.BuildMetrics(records)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exp.ExportMetrics(ctx, md, time.Now()); err != nil {
		return fmt.Errorf("ExportMetrics: %w", err)
	}
	select {
	case md2 := <-cap.metrics:
		return assertMetrics(md2)
	case <-time.After(5 * time.Second):
		return errors.New("timeout: mock collector received no /v1/metrics request")
	}
}

// buildSmokeSpanEvent returns a NR span-event JSON payload in the
// [intrinsics, user, agent] array shape with a W3C 128-bit trace id. The
// span carries an explicit `span.kind=server` so Splunk APM server-side
// span-to-metrics derivation produces the canonical `service.request` chart
// metric.
func buildSmokeSpanEvent() []byte {
	intrinsics := map[string]any{
		"type":             "Span",
		"traceId":          "0a1b2c3d4e5f60718293a4b5c6d7e8fb",
		"guid":             "aabbccddeeff0011",
		"parentId":         "1122334455667788",
		"name":             "Handler/handle",
		"transaction.name": "WebTransaction/Function/index",
		"timestamp":        float64(1_700_000_000_000),
		"duration":         0.250,
		"category":         "http",
		"span.kind":        "server",
		"sampled":          true,
		"priority":         1.5,
	}
	user := map[string]any{"user.tag": "smoke"}
	agent := map[string]any{"db.system": "mysql", "error.message": "boom", "error.class": "RuntimeError"}
	b, _ := json.Marshal([3]any{intrinsics, user, agent})
	return b
}

func assertTraces(td ptrace.Traces) error {
	if td.SpanCount() != 1 {
		return fmt.Errorf("SpanCount = %d, want 1", td.SpanCount())
	}
	s := td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
	if s.TraceID().String() != "0a1b2c3d4e5f60718293a4b5c6d7e8fb" {
		return fmt.Errorf("trace_id = %s", s.TraceID().String())
	}
	if s.SpanID().String() != "aabbccddeeff0011" {
		return fmt.Errorf("span_id = %s", s.SpanID().String())
	}
	if s.ParentSpanID().String() != "1122334455667788" {
		return fmt.Errorf("parent_id = %s", s.ParentSpanID().String())
	}
	if s.Name() != "Handler/handle" {
		return fmt.Errorf("name = %q", s.Name())
	}
	// Server kind ensures Splunk APM auto-generates `service.request` from
	// the span server-side.
	if s.Kind() != ptrace.SpanKindServer {
		return fmt.Errorf("span kind = %v, want server", s.Kind())
	}
	if v, _ := s.Attributes().Get("db.system"); v.Str() != "mysql" {
		return fmt.Errorf("db.system = %q", v.AsString())
	}
	if s.Status().Code() != ptrace.StatusCodeError || s.Status().Message() != "boom" {
		return fmt.Errorf("status = %v %q", s.Status().Code(), s.Status().Message())
	}
	// `error` boolean attribute drives Splunk APM's `sf_error` dim.
	if v, _ := s.Attributes().Get("error"); !v.Bool() {
		return errors.New("error attribute not set on errored span")
	}
	log.Printf("traces ok: 1 server span, trace_id=%s", s.TraceID().String())
	return nil
}

func assertMetrics(md pmetric.Metrics) error {
	if md.DataPointCount() != 2 {
		return fmt.Errorf("DataPointCount = %d, want 2", md.DataPointCount())
	}
	rm := md.ResourceMetrics().At(0).Resource()
	if v, ok := rm.Attributes().Get("service.name"); !ok || v.Str() != "php-smoke" {
		return errors.New("wrong service.name on metrics resource")
	}
	sm := md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
	if sm.At(0).Name() != "http.server.request.duration" {
		return fmt.Errorf("metric name = %q, want http.server.request.duration", sm.At(0).Name())
	}
	if sm.At(0).Type() != pmetric.MetricTypeHistogram {
		return fmt.Errorf("metric type = %v, want histogram", sm.At(0).Type())
	}
	hdp := sm.At(0).Histogram().DataPoints().At(0)
	if hdp.Count() != 3 {
		return fmt.Errorf("count = %d, want 3", hdp.Count())
	}
	if got := hdp.Sum(); got != 0.9 {
		return fmt.Errorf("sum = %v, want 0.9", got)
	}
	log.Printf("metrics ok: 2 data points")
	return nil
}

func assertLogs(ld *plog.Logs) error {
	if rl := ld.ResourceLogs().Len(); rl != 1 {
		return fmt.Errorf("ResourceLogs.Len = %d, want 1", rl)
	}
	res := ld.ResourceLogs().At(0).Resource()
	if v, ok := res.Attributes().Get("service.name"); !ok || v.Str() != "php-smoke" {
		return errors.New("missing/wrong service.name")
	}
	if v, ok := res.Attributes().Get("deployment.environment"); !ok || v.Str() != "smoke" {
		return errors.New("missing/wrong deployment.environment")
	}
	if sl := ld.ResourceLogs().At(0).ScopeLogs().Len(); sl != 1 {
		return fmt.Errorf("ScopeLogs.Len = %d, want 1", sl)
	}
	sl := ld.ResourceLogs().At(0).ScopeLogs().At(0)
	if sl.Scope().Name() != "otel.profiling" {
		return fmt.Errorf("scope name = %q, want otel.profiling", sl.Scope().Name())
	}
	if sl.LogRecords().Len() < 1 {
		return errors.New("no LogRecords")
	}
	lr := sl.LogRecords().At(0)
	body := lr.Body().Str()
	if body == "" {
		return errors.New("empty LogRecord body")
	}
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return fmt.Errorf("base64 decode: %w", err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("gzip new: %w", err)
	}
	pprofBytes, err := io.ReadAll(gr)
	if err != nil {
		return fmt.Errorf("gzip read: %w", err)
	}
	pf, err := profile.Parse(bytes.NewReader(pprofBytes))
	if err != nil {
		return fmt.Errorf("pprof parse: %w", err)
	}
	if err := pf.CheckValid(); err != nil {
		return fmt.Errorf("pprof CheckValid: %w", err)
	}
	if len(pf.Sample) == 0 {
		return errors.New("pprof has no samples")
	}
	// Required attributes.
	for _, k := range []string{
		"com.splunk.sourcetype",
		"profiling.data.type",
		"profiling.data.format",
		"profiling.data.total.frame.count",
		"profiling.instrumentation.source",
	} {
		if _, ok := lr.Attributes().Get(k); !ok {
			return fmt.Errorf("missing LogRecord attribute %q", k)
		}
	}
	// Per-sample labels required by the backend.
	for i, s := range pf.Sample {
		if _, ok := labelValue(s, "source.event.name"); !ok {
			return fmt.Errorf("sample %d missing source.event.name", i)
		}
		_, has := labelNumValue(s, "source.event.time")
		if !has {
			return fmt.Errorf("sample %d missing source.event.time", i)
		}
	}
	log.Printf("decoded pprof: %d samples, %d functions, %d locations",
		len(pf.Sample), len(pf.Function), len(pf.Location))
	return nil
}

func labelValue(s *profile.Sample, key string) (string, bool) {
	if vals, ok := s.Label[key]; ok && len(vals) > 0 {
		return vals[0], true
	}
	return "", false
}

func labelNumValue(s *profile.Sample, key string) (int64, bool) {
	if vals, ok := s.NumLabel[key]; ok && len(vals) > 0 {
		return vals[0], true
	}
	return 0, false
}
