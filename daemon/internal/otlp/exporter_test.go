package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/pprof/profile"
	"go.opentelemetry.io/collector/pdata/pcommon"

	"github.com/newrelic/newrelic-php-agent/daemon/internal/pprof"
)

func base64Decode(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
func gzipReadAll(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}
func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// TestExporter_LiveCollector sends a profile through the OTLP exporter to a
// local Splunk OTel collector running on 127.0.0.1:4318 and asserts the
// collector accepts the request. It then scrapes the collector metrics
// endpoint to confirm splunk_hec/profiling forwarded the record.
//
// This is skipped if no collector is reachable on :4318.
func TestExporter_LiveCollector(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live collector test in short mode")
	}
	endpoint := "http://127.0.0.1:4318"
	if !reachabilityOK(endpoint + "/health") {
		// fall back to a generic OTLP probe
		if !reachabilityOK(endpoint + "/v1/logs") {
			t.Skipf("no collector reachable at %s", endpoint)
		}
	}
	if !reachabilityOK("http://127.0.0.1:8888/metrics") {
		t.Skip("collector metrics endpoint not reachable on :8888")
	}

	before := profilingSentCount(t)

	tr := &pprof.Trace{
		Name:      "WebTransaction/test/live",
		StartTime: time.Now().Add(-1 * time.Second),
		Root: &pprof.Segment{
			Name:       "WebTransaction/test/live",
			StartMs:    0,
			StopMs:     1000,
			DurationMs: 1000,
			Children: []*pprof.Segment{
				{Name: "dbQuery", StartMs: 10, StopMs: 400, DurationMs: 390},
				{Name: "httpCall", StartMs: 410, StopMs: 990, DurationMs: 580},
			},
		},
	}
	prof := pprof.BuildProfile(tr, pprof.TypeCPU, 10*time.Millisecond)

	exp, err := New(Config{
		Endpoint:    endpoint,
		ServiceName: "php-otel-test",
		Environment: "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ed := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exp.ExportProfiles(ctx, []ProfileRecord{
		{Profile: prof, ProfileType: string(pprof.TypeCPU)},
	}, ed); err != nil {
		t.Fatalf("ExportProfiles: %v", err)
	}

	// Allow the collector to forward the record and update its metrics.
	for i := 0; i < 30; i++ {
		time.Sleep(500 * time.Millisecond)
		if after := profilingSentCount(t); after > before {
			t.Logf("profiling sent counter %d -> %d", before, after)
			return
		}
	}
	t.Fatalf("profiling sent counter did not advance (before=%d)", before)
}

func reachabilityOK(url string) bool {
	resp, err := http.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return resp.StatusCode < 500
}

func profilingSentCount(t *testing.T) int {
	t.Helper()
	resp, err := http.Get("http://127.0.0.1:8888/metrics")
	if err != nil {
		t.Fatalf("scrape metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, line := range strings.Split(string(body), "\n") {
		// Label ordering in Prometheus is not guaranteed; match the metric
		// name + the exporter label by substring, then parse the trailing int.
		if !strings.HasPrefix(line, "otelcol_exporter_sent_log_records") {
			continue
		}
		if !strings.Contains(line, `exporter="splunk_hec/profiling"`) {
			continue
		}
		idx := strings.LastIndex(line, " ")
		if idx < 0 {
			continue
		}
		var n int
		fmt.Sscanf(line[idx+1:], "%d", &n)
		return n
	}
	// The exporter may use a different label set; fall back to a substring match.
	t.Fatalf("splunk_hec/profiling sent counter not found in metrics scrape")
	return 0
}

// TestProfilingLogRecord_BuildsContractShape asserts the LogRecord matches
// the locked Splunk AlwaysOn Profiling contract without needing a collector.
func TestProfilingLogRecord_BuildsContractShape(t *testing.T) {
	tr := &pprof.Trace{
		Name:      "WebTransaction/Function/index",
		StartTime: time.Unix(1_700_000_000, 0),
		Root: &pprof.Segment{
			Name:       "WebTransaction/Function/index",
			StartMs:    0,
			StopMs:     100,
			DurationMs: 100,
			Children: []*pprof.Segment{
				{Name: "dbQuery", StartMs: 10, StopMs: 90, DurationMs: 80},
			},
		},
	}
	prof := pprof.BuildProfile(tr, pprof.TypeCPU, 10*time.Millisecond)

	lr := ProfilingLogRecord(prof, "cpu", time.Unix(1_700_000_000, 0))

	if lr.Body().Type() != pcommon.ValueTypeStr {
		t.Fatalf("Body type = %v, want Str", lr.Body().Type())
	}
	bodyStr := lr.Body().Str()
	if bodyStr == "" {
		t.Fatal("Body is empty")
	}

	// Required attributes.
	for _, want := range []struct{ k, v string }{
		{attrSplunkSourcetype, scopeName},
		{attrProfDataFormat, profFormatPprofGzipB64},
		{attrProfInstrSource, profInstrSourceContinuous},
	} {
		got, ok := lr.Attributes().Get(want.k)
		if !ok {
			t.Errorf("missing attr %q", want.k)
			continue
		}
		if got.Str() != want.v {
			t.Errorf("attr %q = %q want %q", want.k, got.Str(), want.v)
		}
	}
	if dt, _ := lr.Attributes().Get(attrProfDataType); dt.Str() != "cpu" {
		t.Errorf("profiling.data.type = %q want cpu", dt.Str())
	}
	if fc, _ := lr.Attributes().Get(attrProfTotalFrameCount); fc.Int() <= 0 {
		t.Errorf("profiling.data.total.frame.count = %d, want > 0", fc.Int())
	}

	// Round-trip the body through base64 -> gzip -> pprof parse.
	raw, err := base64Decode(bodyStr)
	if err != nil {
		t.Fatalf("base64 decode body: %v", err)
	}
	gz, err := gzipReadAll(raw)
	if err != nil {
		t.Fatalf("gunzip body: %v", err)
	}
	rp, err := profile.Parse(bytesReader(gz))
	if err != nil {
		t.Fatalf("parse pprof: %v", err)
	}
	if len(rp.Sample) != 1 {
		t.Errorf("reparsed sample count = %d want 1", len(rp.Sample))
	}
}
