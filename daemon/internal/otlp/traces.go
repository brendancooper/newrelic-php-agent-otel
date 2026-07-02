package otlp

// traces.go: OTLP /v1/traces egress for New Relic SpanEvents.
//
// The span event JSON payload produced by the PHP C extension (and carried
// untouched through the daemon over flatbuffers) is the array
//
//	[intrinsics, user_attributes, agent_attributes]
//
// where each element is a JSON object. This file maps that payload to an OTLP
// ResourceSpans / ScopeSpans / Span tree and POSTs it (gzip protobuf) to
// <endpoint>/v1/traces. The OTLP span IDs are 8 bytes; trace IDs are 16 bytes.
// NR trace IDs are 16 hex characters (8 bytes) today and will become 32 hex
// once the W3C 128-bit change in the C extension lands; this converter
// left-pads short IDs with zero bytes to always produce a valid 16-byte
// TraceID, so the same code handles both widths.

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
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
)

// MaxSpanEventJSONSize caps the size of a single decoded JSON element to keep
// a runaway event from blowing memory during decode.
const maxSpanEventJSONSize = 256 * 1024

// ExportTraces sends an OTLP Traces request to <endpoint>/v1/traces. It's the
// /v1/traces analogue of ExportProfiles.
func (e *Exporter) ExportTraces(ctx context.Context, td ptrace.Traces, _ time.Time) error {
	if td.SpanCount() == 0 {
		return nil
	}
	er := ptraceotlp.NewExportRequestFromTraces(td)
	body, err := er.MarshalProto()
	if err != nil {
		return fmt.Errorf("otlp: marshal traces: %w", err)
	}
	return e.postProtobuf(ctx, "/v1/traces", body)
}

// postProtobuf sends a (optionally gzip) OTLP protobuf body to path under the
// exporter's endpoint. Shared by ExportProfiles/ExportTraces/ExportMetrics.
func (e *Exporter) postProtobuf(ctx context.Context, path string, body []byte) error {
	url := e.cfg.Endpoint + path
	var bodyReader io.Reader = bytes.NewReader(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bodyReader)
	if err != nil {
		return fmt.Errorf("otlp: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	if strings.EqualFold(e.cfg.Compression, "gzip") {
		var payload bytes.Buffer
		gw := gzip.NewWriter(&payload)
		if _, err := gw.Write(body); err != nil {
			return fmt.Errorf("otlp: gzip: %w", err)
		}
		if err := gw.Close(); err != nil {
			return fmt.Errorf("otlp: gzip close: %w", err)
		}
		req.Body = io.NopCloser(&payload)
		req.ContentLength = int64(payload.Len())
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(payload.Bytes())), nil }
		req.Header.Set("Content-Encoding", "gzip")
	}
	for k, v := range e.cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("otlp: post %s: %w", path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("otlp: collector returned %d for %s: %s", resp.StatusCode, path, strconv.Quote(string(respBody)))
	}
	return nil
}

// spanEventPayload is the [intrinsics, user, agent] array shape.
type spanEventPayload struct {
	Intrinsics map[string]any `json:"intrinsics"`
	User       map[string]any `json:"user_attributes"`
	Agent      map[string]any `json:"agent_attributes"`
}

// decodeSpanEvent parses one NR span event JSON blob into the payload shape.
func decodeSpanEvent(data []byte) (*spanEventPayload, error) {
	if len(data) > maxSpanEventJSONSize {
		return nil, fmt.Errorf("span event json too large (%d bytes)", len(data))
	}
	// The payload is a 3-element JSON array. Decode into a typed struct first
	// using a wrapper, then unmarshal via strip-array: build [intrinsics,
	// user, agent] into an object with named fields using a 3-element slice
	// then map into the struct.
	var arr [3]json.RawMessage
	if err := json.Unmarshal(data, &arr); err != nil {
		return nil, fmt.Errorf("decode span event array: %w", err)
	}
	p := &spanEventPayload{}
	if len(arr[0]) > 0 {
		if err := json.Unmarshal(arr[0], &p.Intrinsics); err != nil {
			return nil, fmt.Errorf("decode intrinsics: %w", err)
		}
	}
	if len(arr[1]) > 0 {
		if err := json.Unmarshal(arr[1], &p.User); err != nil {
			return nil, fmt.Errorf("decode user_attributes: %w", err)
		}
	}
	if len(arr[2]) > 0 {
		if err := json.Unmarshal(arr[2], &p.Agent); err != nil {
			return nil, fmt.Errorf("decode agent_attributes: %w", err)
		}
	}
	return p, nil
}

// BuildTraces converts a slice of NR span event JSON blobs into an OTLP
// Traces object, sharing one ResourceSpans / ScopeSpans for all spans (the
// Resource carries this exporter's service identity). Caller is responsible
// for ensuring all events belong to the same service.
func (e *Exporter) BuildTraces(events [][]byte, now time.Time) (ptrace.Traces, error) {
	td := ptrace.NewTraces()
	if len(events) == 0 {
		return td, nil
	}
	rs := td.ResourceSpans().AppendEmpty()
	res := rs.Resource()
	res.Attributes().PutStr("service.name", e.cfg.ServiceName)
	res.Attributes().PutStr("telemetry.sdk.name", "php")
	res.Attributes().PutStr("telemetry.sdk.language", "php")
	if e.cfg.ServiceVersion != "" {
		res.Attributes().PutStr("service.version", e.cfg.ServiceVersion)
	}
	if e.cfg.Environment != "" {
		res.Attributes().PutStr("deployment.environment", e.cfg.Environment)
	}
	ss := rs.ScopeSpans().AppendEmpty()
	ss.Scope().SetName("otel.php")
	ss.Scope().SetVersion("0.1.0")

	var firstErr error
	for _, raw := range events {
		p, err := decodeSpanEvent(raw)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		span := ss.Spans().AppendEmpty()
		fillSpan(span, p, now)
	}
	return td, firstErr
}

// reservedSpanAttrKeys are intrinsics that map to dedicated OTLP Span fields
// rather than onto span attributes, so they should not be duplicated as
// attributes.
var reservedSpanAttrKeys = map[string]struct{}{
	"type":             {},
	"category":         {},
	"name":             {},
	"traceId":          {},
	"guid":             {},
	"parentId":         {},
	"transactionId":    {},
	"transaction.name": {},
	"timestamp":        {},
	"duration":         {},
	"priority":         {},
	"sampled":          {},
	"span.kind":        {},
	"nr.entryPoint":    {},
	"trustedParentId":  {},
	"tracingVendors":   {},
	"priority.state":   {},
}

// fillSpan populates an OTLP Span from a decoded NR span event.
func fillSpan(span ptrace.Span, p *spanEventPayload, now time.Time) {
	get := func(k string) (any, bool) {
		if p.Intrinsics != nil {
			if v, ok := p.Intrinsics[k]; ok {
				return v, true
			}
		}
		return nil, false
	}
	str := func(k string) string {
		if v, ok := get(k); ok {
			switch t := v.(type) {
			case string:
				return t
			case float64:
				return strconv.FormatFloat(t, 'f', -1, 64)
			}
		}
		return ""
	}

	if tid := str("traceId"); tid != "" {
		span.SetTraceID(hexToTraceID(tid))
	} else {
		// Without a trace ID the span is uncorrelatable; OTel collectors drop
		// spans with an empty TraceID entirely, so synthesize one from the
		// span guid so the span still surfaces.
		if g := str("guid"); g != "" {
			span.SetTraceID(hexToTraceID(g))
		}
	}
	span.SetSpanID(hexToSpanID(str("guid")))
	if pid := str("parentId"); pid != "" {
		span.SetParentSpanID(hexToSpanID(pid))
	}
	if name := str("name"); name != "" {
		span.SetName(name)
	} else if tn := str("transaction.name"); tn != "" {
		span.SetName(tn)
	}

	// Timestamp / duration. NR intrinsics carry timestamp in ms and
	// duration in seconds.
	startMs := int64(0)
	if v, ok := get("timestamp"); ok {
		switch t := v.(type) {
		case float64:
			startMs = int64(t)
		case json.Number:
			n, _ := t.Int64()
			startMs = n
		}
	}
	if startMs > 0 {
		span.SetStartTimestamp(pcommon.NewTimestampFromTime(time.UnixMilli(startMs)))
		var durS float64
		if v, ok := get("duration"); ok {
			if f, ok := v.(float64); ok {
				durS = f
			}
		}
		if durS > 0 {
			span.SetEndTimestamp(pcommon.NewTimestampFromTime(time.UnixMilli(startMs).Add(time.Duration(durS * float64(time.Second)))))
		} else {
			span.SetEndTimestamp(pcommon.NewTimestampFromTime(time.UnixMilli(startMs)))
		}
	} else {
		// Fall back to harvest time if the event lacks a timestamp.
		span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
		span.SetEndTimestamp(pcommon.NewTimestampFromTime(now))
	}

	// Span kind. NR span events carry an explicit `span.kind` when the C extension
	// set one; otherwise infer from the entry-point / category signals. Root
	// inbound web transactions must be SERVER so Splunk APM server-side
	// span-to-metrics derivation produces the canonical `service.request` and
	// `service.request.error` chart metrics.
	if kind := str("span.kind"); kind != "" {
		span.SetKind(mapSpanKind(kind))
	} else {
		span.SetKind(inferSpanKind(p, str, get))
	}

	// Status: NR doesn't carry explicit span status; flag error if the
	// agent attributes carry an error message.
	if p.Agent != nil {
		if v, ok := p.Agent["error.message"]; ok {
			if s, ok := v.(string); ok && s != "" {
				span.Status().SetCode(ptrace.StatusCodeError)
				span.Status().SetMessage(s)
			}
		}
		// Make sure error.message/error.class survive as attributes too
		// (OTel convention).
		putAny(span.Attributes(), "error.message", p.Agent["error.message"])
		putAny(span.Attributes(), "error.class", p.Agent["error.class"])
	}

	// OTel / Splunk APM convention: set the `error` boolean attribute when
	// the span errored so downstream (`sf_error` dim, dashboards) treats the
	// span as an error span independently of `status.code`.
	if span.Status().Code() == ptrace.StatusCodeError {
		span.Attributes().PutBool("error", true)
	}

	// Carry remaining intrinsics + user + agent attributes as OTel span
	// attributes (skip the reserved ones already mapped to span fields).
	mergeAttrs(span.Attributes(), p.Intrinsics, reservedSpanAttrKeys, "intrinsics.")
	mergeAttrs(span.Attributes(), p.User, nil, "")
	// agent.* except the error.* we already added.
	agentReserved := map[string]struct{}{"error.message": {}, "error.class": {}}
	mergeAttrs(span.Attributes(), p.Agent, agentReserved, "")

	// Datastore calls: add the OTel semantic-convention attributes Splunk
	// Observability Cloud's APM backend-inference logic needs to render the
	// call as an inferred "<db.system>:<db.name>" database node rather than
	// a same-service internal span. Without these the span only carries
	// NR's own legacy attribute names (`component`, `db.instance`,
	// `peer.hostname`/`peer.address`), which Splunk's OTLP ingest doesn't
	// recognize.
	if str("category") == "datastore" {
		addDatastoreSemanticAttrs(span.Attributes(), p)
	}
}

// datastoreDBSystem maps the New Relic datastore "component" product name
// (lowercased; see axiom/nr_datastore_private.h) onto the canonical OTel
// `db.system` semantic-convention value. The C agent only ever populates the
// `db.system` agent attribute directly for the AWS DynamoDB integration
// (agent/lib_aws_sdk_php.c); every other datastore driver carries only the
// human-readable product name in the `component` intrinsic, and that name
// doesn't always match the OTel value (e.g. "Postgres" vs "postgresql").
var datastoreDBSystem = map[string]string{
	"mongodb":   "mongodb",
	"memcached": "memcached",
	"mysql":     "mysql",
	"redis":     "redis",
	"mssql":     "mssql",
	"oracle":    "oracle",
	"postgres":  "postgresql",
	"sqlite":    "sqlite",
	"firebird":  "firebird",
	"odbc":      "other_sql",
	"sybase":    "sybase",
	"informix":  "informix",
	"pdo":       "other_sql",
	"dynamodb":  "dynamodb",
}

// addDatastoreSemanticAttrs enriches a datastore client span with `db.system`,
// `db.name`, and peer host/port attributes under both the OTel semantic
// conventions Splunk's backend-inference logic reads (`net.peer.name`/
// `server.address`, `net.peer.port`/`server.port`) and preserves NR's own
// legacy names (`component`, `db.instance`, `peer.hostname`, `peer.address`)
// already merged onto the span for backward compatibility.
func addDatastoreSemanticAttrs(attrs pcommon.Map, p *spanEventPayload) {
	agentStr := func(k string) string {
		if p.Agent == nil {
			return ""
		}
		if v, ok := p.Agent[k].(string); ok {
			return v
		}
		return ""
	}
	intrinsicStr := func(k string) string {
		if p.Intrinsics == nil {
			return ""
		}
		if v, ok := p.Intrinsics[k].(string); ok {
			return v
		}
		return ""
	}

	if _, ok := attrs.Get("db.system"); !ok {
		dbSystem := agentStr("db.system")
		if dbSystem == "" {
			if component := intrinsicStr("component"); component != "" {
				if mapped, ok := datastoreDBSystem[strings.ToLower(component)]; ok {
					dbSystem = mapped
				} else {
					dbSystem = strings.ToLower(component)
				}
			}
		}
		if dbSystem != "" {
			attrs.PutStr("db.system", dbSystem)
		}
	}

	if dbName := agentStr("db.instance"); dbName != "" {
		attrs.PutStr("db.name", dbName)
	}

	if host := agentStr("peer.hostname"); host != "" && !strings.EqualFold(host, "unknown") {
		attrs.PutStr("net.peer.name", host)
		attrs.PutStr("server.address", host)
	}
	if addr := agentStr("peer.address"); addr != "" {
		if _, portStr, err := net.SplitHostPort(addr); err == nil && portStr != "" && !strings.EqualFold(portStr, "unknown") {
			if port, err := strconv.Atoi(portStr); err == nil {
				attrs.PutInt("net.peer.port", int64(port))
				attrs.PutInt("server.port", int64(port))
			}
		}
	}
}

func mergeAttrs(dst pcommon.Map, src map[string]any, skip map[string]struct{}, prefix string) {
	if src == nil {
		return
	}
	// Sort keys for deterministic output (helps golden tests).
	keys := make([]string, 0, len(src))
	for k := range src {
		if _, omit := skip[k]; omit {
			continue
		}
		keys = append(keys, k)
	}
	stringsSort(keys)
	for _, k := range keys {
		if prefix != "" && strings.HasPrefix(k, "nr.") {
			// NR-specific intrinsics → keep verbatim names.
			prefix = ""
		}
		name := prefix + k
		if prefix == "" {
			name = k
		}
		putAny(dst, name, src[k])
	}
}

func stringsSort(s []string) {
	// Small slice; insertion sort avoids importing sort just for this.
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j-1] > s[j] {
			s[j-1], s[j] = s[j], s[j-1]
			j--
		}
	}
}

func putAny(dst pcommon.Map, key string, v any) {
	if v == nil {
		return
	}
	switch t := v.(type) {
	case string:
		if t == "" {
			return
		}
		dst.PutStr(key, t)
	case bool:
		dst.PutBool(key, t)
	case float64:
		// JSON numbers are float64. Avoid storing NaN/Inf; OTel attribute
		// values can be any float but downstream JSON may choke.
		if t != t || t > 1e308 || t < -1e308 {
			return
		}
		dst.PutDouble(key, t)
	}
}

// inferSpanKind maps a NR span to an OTLP SpanKind when the payload did not
// carry an explicit `span.kind` attribute. It looks at the entry-point flag,
// the presence of a parent (root vs nested), and the `category` intrinsic.
//
//   - nr.entryPoint=true (root span carrying the entry-point marker)
//     ⇒ SpanKindServer  (Splunk APM service.entry → service.request)
//   - no parentId AND transaction.name starts with WebTransaction/
//     ⇒ SpanKindServer  (root inbound web tx)
//   - category=datastore ⇒ SpanKindClient  (DB call as a child)
//   - category=http     ⇒ SpanKindClient  (outbound HTTP call as a child)
//   - otherwise          ⇒ SpanKindInternal
func inferSpanKind(p *spanEventPayload, str func(string) string, get func(string) (any, bool)) ptrace.SpanKind {
	if v, ok := get("nr.entryPoint"); ok {
		if b, ok := v.(bool); ok && b {
			return ptrace.SpanKindServer
		}
	}
	if str("parentId") == "" {
		name := str("transaction.name")
		if name == "" {
			name = str("name")
		}
		if strings.HasPrefix(name, "WebTransaction/") {
			return ptrace.SpanKindServer
		}
	}
	if cat, ok := get("category"); ok {
		switch fmt.Sprintf("%v", cat) {
		case "datastore":
			return ptrace.SpanKindClient
		case "http":
			return ptrace.SpanKindClient
		}
	}
	return ptrace.SpanKindInternal
}

func mapSpanKind(k string) ptrace.SpanKind {
	switch strings.ToLower(k) {
	case "client":
		return ptrace.SpanKindClient
	case "server":
		return ptrace.SpanKindServer
	case "producer":
		return ptrace.SpanKindProducer
	case "consumer":
		return ptrace.SpanKindConsumer
	case "internal":
		return ptrace.SpanKindInternal
	default:
		return ptrace.SpanKindInternal
	}
}

// hexToTraceID parses a hex trace id into a 16-byte TraceID. Handles both
// 32-hex (W3C 128-bit) and shorter (legacy NR 16-hex) by left-padding with
// zeros, mirroring the C extension's pad_trace_id behaviour.
func hexToTraceID(h string) pcommon.TraceID {
	var out [16]byte
	if h == "" {
		return pcommon.TraceID(out)
	}
	h = strings.ToLower(h)
	b, err := hex.DecodeString(h)
	if err != nil || len(b) == 0 {
		return pcommon.TraceID(out)
	}
	if len(b) > 16 {
		b = b[len(b)-16:]
	}
	copy(out[16-len(b):], b)
	return pcommon.TraceID(out)
}

// hexToSpanID parses a hex span id into an 8-byte SpanID. Shorter ids pad
// with leading zeros; an empty id yields an empty SpanID (uncorrelated).
func hexToSpanID(h string) pcommon.SpanID {
	var out [8]byte
	if h == "" {
		return pcommon.SpanID(out)
	}
	h = strings.ToLower(h)
	b, err := hex.DecodeString(h)
	if err != nil || len(b) == 0 {
		return pcommon.SpanID(out)
	}
	if len(b) > 8 {
		b = b[len(b)-8:]
	}
	copy(out[8-len(b):], b)
	return pcommon.SpanID(out)
}
