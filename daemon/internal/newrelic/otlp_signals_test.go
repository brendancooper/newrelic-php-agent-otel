package newrelic

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/newrelic/newrelic-php-agent/daemon/internal/otlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
)

// mockOTLPServer captures the first request body to path and signals receipt
// via the returned channel. The body is gzip-decompressed if needed.
func mockOTLPServer(t *testing.T, path string) (endpoint string, body chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	body = make(chan []byte, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			if gr, err := gzip.NewReader(bytes.NewReader(b)); err == nil {
				b, _ = io.ReadAll(gr)
			}
		}
		select {
		case body <- b:
		default:
		}
		w.WriteHeader(200)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	endpoint = "http://" + ln.Addr().String()
	return endpoint, body
}

// spanEventJSON returns a single synthetic NR span-event JSON payload matching
// the [intrinsics, user, agent] shape used by the C extension.
func spanEventJSON(traceID, guid, parentID string) []byte {
	intrinsics := map[string]any{
		"type":             "Span",
		"traceId":          traceID,
		"guid":             guid,
		"parentId":         parentID,
		"name":             "Handler/handle",
		"transaction.name": "WebTransaction/Function/smoke",
		"timestamp":        float64(1_700_000_000_000),
		"duration":         0.200,
		"category":         "generic",
		"span.kind":        "client",
		"sampled":          true,
		"priority":         1.5,
	}
	user := map[string]any{"user.tag": "smoke"}
	agent := map[string]any{"db.system": "mysql"}
	b, _ := json.Marshal([3]any{intrinsics, user, agent})
	return b
}

// TestHarvestTraces_MockCollectorRoundtrip populates a SpanEvents bucket with
// a synthetic span event, calls harvestTraces against a mock collector, and
// verifies an OTLP /v1/traces request carrying the expected span was sent.
func TestHarvestTraces_MockCollectorRoundtrip(t *testing.T) {
	endpoint, body := mockOTLPServer(t, "/v1/traces")
	exp, err := otlp.New(otlp.Config{Endpoint: endpoint, ServiceName: "php-traces-test"})
	if err != nil {
		t.Fatalf("otlp.New: %v", err)
	}

	se := NewSpanEvents(100)
	se.AddEventFromData(spanEventJSON("0a1b2c3d4e5f60718293a4b5c6d7e8fb", "aabbccddeeff0011", "1122334455667788"), SamplingPriority(1.0))

	harvestTraces(se, exp, time.Now(), OTLPConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ctx
	select {
	case b := <-body:
		er := ptraceotlp.NewExportRequest()
		if err := er.UnmarshalProto(b); err != nil {
			t.Fatalf("unmarshal traces: %v", err)
		}
		if n := er.Traces().SpanCount(); n != 1 {
			t.Fatalf("SpanCount = %d, want 1", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: no /v1/traces POST received")
	}
}

// TestHarvestMetricsOTLP_MockCollectorRoundtrip populates a MetricTable,
// calls harvestMetricsOTLP against a mock collector, and verifies the OTLP
// /v1/metrics request carries the metric data point.
func TestHarvestMetricsOTLP_MockCollectorRoundtrip(t *testing.T) {
	endpoint, body := mockOTLPServer(t, "/v1/metrics")
	exp, err := otlp.New(otlp.Config{Endpoint: endpoint, ServiceName: "php-metrics-test"})
	if err != nil {
		t.Fatalf("otlp.New: %v", err)
	}

	mt := NewMetricTable(1000, time.Now())
	mt.AddCount("WebTransaction/Function/smoke", "", 5, Forced)
	mt.AddValue("Custom/latency", "", 0.1, Forced)
	harvestMetricsOTLP(mt, exp, time.Now(), OTLPConfig{})

	select {
	case b := <-body:
		er := pmetricotlp.NewExportRequest()
		if err := er.UnmarshalProto(b); err != nil {
			t.Fatalf("unmarshal metrics: %v", err)
		}
		if n := er.Metrics().DataPointCount(); n != 2 {
			t.Fatalf("DataPointCount = %d, want 2", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: no /v1/metrics POST received")
	}
}
