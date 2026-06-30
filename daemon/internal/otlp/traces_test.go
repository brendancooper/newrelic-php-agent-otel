package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
)

// testExp is a shared Exporter used by the helper tests below. The
// endpoint is never hit (these tests only exercise the in-memory builders).
var testExp, _ = New(Config{Endpoint: "http://127.0.0.1:4318", ServiceName: "php-test"})

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// spanEventForTest builds a single NR span-event payload in the
// [intrinsics, user, agent] array shape.
func spanEventForTest(t *testing.T, traceID, guid, parentID string) []byte {
	intrinsics := map[string]any{
		"type":             "Span",
		"traceId":          traceID,
		"guid":             guid,
		"parentId":         parentID,
		"name":             "Handler/handle",
		"transaction.name": "WebTransaction/Function/index",
		"timestamp":        float64(1_700_000_000_000),
		"duration":         0.250,
		"category":         "generic",
		"span.kind":        "client",
		"sampled":          true,
		"priority":         1.5,
	}
	user := map[string]any{"user.tag": "alpha"}
	agent := map[string]any{"db.system": "mysql", "error.message": "boom", "error.class": "RuntimeError"}
	return mustJSON(t, [3]any{intrinsics, user, agent})
}

func TestBuildTraces_MapsSpanFields(t *testing.T) {
	exp, _ := New(Config{Endpoint: "http://127.0.0.1:4318", ServiceName: "php-test"})
	raw := spanEventForTest(t, "0a1b2c3d4e5f60718293a4b5c6d7e8fb", "aabbccddeeff0011", "1122334455667788")
	td, err := exp.BuildTraces([][]byte{raw}, time.Now())
	if err != nil {
		t.Fatalf("BuildTraces: %v", err)
	}
	if td.SpanCount() != 1 {
		t.Fatalf("SpanCount = %d, want 1", td.SpanCount())
	}
	s := td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)

	wantTrace, _ := hex.DecodeString("0a1b2c3d4e5f60718293a4b5c6d7e8fb")
	gotTrace := [16]byte(s.TraceID())
	if !bytes.Equal(gotTrace[:], wantTrace) {
		t.Errorf("trace id = %x, want %x", gotTrace, wantTrace)
	}
	wantSpan, _ := hex.DecodeString("aabbccddeeff0011")
	gotSpan := [8]byte(s.SpanID())
	if !bytes.Equal(gotSpan[:], wantSpan) {
		t.Errorf("span id = %x, want %x", gotSpan, wantSpan)
	}
	wantParent, _ := hex.DecodeString("1122334455667788")
	gotParent := [8]byte(s.ParentSpanID())
	if !bytes.Equal(gotParent[:], wantParent) {
		t.Errorf("parent id = %x, want %x", gotParent, wantParent)
	}
	if s.Name() != "Handler/handle" {
		t.Errorf("name = %q", s.Name())
	}
	if s.Kind() != ptrace.SpanKindClient {
		t.Errorf("kind = %v want client", s.Kind())
	}
	if s.StartTimestamp().AsTime().UnixMilli() != 1_700_000_000_000 {
		t.Errorf("start ts = %v", s.StartTimestamp().AsTime())
	}
	if got := s.EndTimestamp().AsTime().UnixMilli(); got != 1_700_000_000_250 {
		t.Errorf("end ts ms = %d, want 1700000000250", got)
	}
	if s.Status().Code() != ptrace.StatusCodeError {
		t.Errorf("status code = %v want error", s.Status().Code())
	}
	if s.Status().Message() != "boom" {
		t.Errorf("status message = %q", s.Status().Message())
	}
	// service.name on the resource
	svc, _ := td.ResourceSpans().At(0).Resource().Attributes().Get("service.name")
	if svc.Str() != "php-test" {
		t.Errorf("service.name = %q", svc.Str())
	}
	// error.message / error.class landed on attributes
	if v, _ := s.Attributes().Get("error.message"); v.Str() != "boom" {
		t.Errorf("error.message attr = %q", v.AsString())
	}
	if v, _ := s.Attributes().Get("error.class"); v.Str() != "RuntimeError" {
		t.Errorf("error.class attr = %q", v.AsString())
	}
	// user attribute preserved
	if v, _ := s.Attributes().Get("user.tag"); v.Str() != "alpha" {
		t.Errorf("user.tag = %q", v.AsString())
	}
	// reserved intrinsics NOT duplicated as attributes
	if _, ok := s.Attributes().Get("traceId"); ok {
		t.Error("traceId should not be a span attribute")
	}
	if _, ok := s.Attributes().Get("guid"); ok {
		t.Error("guid should not be a span attribute")
	}
}

func TestBuildTraces_LegacyShortTraceIDLeftPadded(t *testing.T) {
	exp, _ := New(Config{Endpoint: "http://127.0.0.1:4318", ServiceName: "x"})
	raw := spanEventForTest(t, "0a1b2c3d4e5f6071", "aabbccddeeff0011", "")
	td, _ := exp.BuildTraces([][]byte{raw}, time.Now())
	s := td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
	got := s.TraceID().String()
	want := "00000000000000000a1b2c3d4e5f6071"
	if got != want {
		t.Errorf("padded trace id = %q, want %q", got, want)
	}
	// ParentSpanID must be all-zero (valid OTel empty parent) when missing.
	for _, b := range s.ParentSpanID() {
		if b != 0 {
			t.Errorf("parent id should be empty, got %x", s.ParentSpanID().String())
			break
		}
	}
}

// startMockOTLP starts an HTTP server capturing one OTLP/protobuf request to
// path (transparently gzip-decompressed). Returns the listener, server, and a
// channel receiving the raw (decompressed) protobuf bytes.
func startMockOTLP(t *testing.T, path string) (net.Listener, *http.Server, chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	got := make(chan []byte, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			gr, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			body, _ = io.ReadAll(gr)
		}
		got <- body
		w.WriteHeader(200)
	})}
	go srv.Serve(ln)
	return ln, srv, got
}

func TestExportTraces_LiveRoundtrip(t *testing.T) {
	if testing.Short() {
		t.Skip("live mock test in short mode")
	}
	ln, srv, got := startMockOTLP(t, "/v1/traces")
	defer func() { srv.Close() }()
	defer ln.Close()

	exp, _ := New(Config{Endpoint: "http://" + ln.Addr().String(), ServiceName: "php-test"})
	raw := spanEventForTest(t, "0a1b2c3d4e5f60718293a4b5c6d7e8fb", "aabbccddeeff0011", "1122334455667788")
	td, _ := exp.BuildTraces([][]byte{raw}, time.Now())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exp.ExportTraces(ctx, td, time.Now()); err != nil {
		t.Fatalf("ExportTraces: %v", err)
	}

	select {
	case body := <-got:
		er := ptraceotlp.NewExportRequest()
		if err := er.UnmarshalProto(body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// ptraceotlp.ExportRequest traces-only; just confirm SpanCount.
		if gotsc := er.Traces().SpanCount(); gotsc != 1 {
			t.Fatalf("roundtrip SpanCount = %d, want 1", gotsc)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for /v1/traces POST")
	}
}

func TestExportTraces_EmptyNoOp(t *testing.T) {
	exp, _ := New(Config{Endpoint: "http://127.0.0.1:4318", ServiceName: "x"})
	if err := exp.ExportTraces(t.Context(), ptrace.NewTraces(), time.Now()); err != nil {
		t.Fatalf("ExportTraces(empty) = %v, want nil", err)
	}
}

// TestInferSpanKind_RootWebTxnIsServer verifies that a NR span event
// without an explicit `span.kind` intrinsic is still classified as SERVER when
// it is a root inbound web transaction. This is what drives Splunk APM's
// auto-generated `service.request` metric from our /v1/traces stream.
func TestInferSpanKind_RootWebTxnIsServer(t *testing.T) {
	cases := []struct {
		name        string
		intrinsics  map[string]any
		wantKind    ptrace.SpanKind
		wantErrAttr bool
	}{
		{
			name: "root_web_no_kind",
			intrinsics: map[string]any{
				"type":             "Span",
				"traceId":          "0a1b2c3d4e5f60718293a4b5c6d7e8fb",
				"guid":             "1122334455667788",
				"name":             "Controller/Controller::index",
				"transaction.name": "WebTransaction/Function/index",
				"timestamp":        float64(1_700_000_000_000),
				"duration":         0.1,
				"category":         "generic",
			},
			wantKind: ptrace.SpanKindServer,
		},
		{
			name: "root_web_nr_entry_point",
			intrinsics: map[string]any{
				"type":          "Span",
				"traceId":       "0a1b2c3d4e5f60718293a4b5c6d7e8fb",
				"guid":          "1122334455667788",
				"parentId":      "aabbccddeeff0011", // has a parent → fallback fails, but nr.entryPoint should override
				"name":          "WebTransaction/",  // any
				"timestamp":     float64(1_700_000_000_000),
				"duration":      0.1,
				"nr.entryPoint": true,
			},
			wantKind: ptrace.SpanKindServer,
		},
		{
			name: "child_datastore",
			intrinsics: map[string]any{
				"type":      "Span",
				"traceId":   "0a1b2c3d4e5f60718293a4b5c6d7e8fb",
				"guid":      "1122334455667788",
				"parentId":  "aabbccddeeff0011",
				"name":      "Datastore/statement/MySQL/users/select",
				"timestamp": float64(1_700_000_000_000),
				"duration":  0.05,
				"category":  "datastore",
			},
			wantKind: ptrace.SpanKindClient,
		},
		{
			name: "child_http_external",
			intrinsics: map[string]any{
				"type":      "Span",
				"traceId":   "0a1b2c3d4e5f60718293a4b5c6d7e8fb",
				"guid":      "1122334455667788",
				"parentId":  "aabbccddeeff0011",
				"name":      "External/api.example.com/all",
				"timestamp": float64(1_700_000_000_000),
				"duration":  0.05,
				"category":  "http",
			},
			wantKind: ptrace.SpanKindClient,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := mustJSON(t, [3]any{tc.intrinsics, map[string]any{}, map[string]any{}})
			td, err := testExp.BuildTraces([][]byte{raw}, time.Now())
			if err != nil {
				t.Fatalf("BuildTraces: %v", err)
			}
			s := td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
			if s.Kind() != tc.wantKind {
				t.Fatalf("kind = %v, want %v", s.Kind(), tc.wantKind)
			}
		})
	}
}

var _ = fmt.Sprintf
