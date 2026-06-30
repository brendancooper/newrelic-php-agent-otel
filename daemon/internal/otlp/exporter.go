// Package otlp sends profiling (pprof), traces, and metrics to a Splunk OTel
// collector over OTLP/HTTP. Profiling uses the Logs signal (one LogRecord per
// pprof profile under scope name "otel.profiling", per the Splunk AlwaysOn
// Profiling contract locked during Phase 0 — see PLAN.md). Traces and metrics
// use the standard /v1/traces and /v1/metrics OTLP/HTTP signals and are
// converted from New Relic SpanEvents and MetricTable records respectively.
package otlp

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/pprof/profile"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
)

// Splunk AlwaysOn Profiling LogRecord attribute keys and fixed values, from
// exporter/splunkhecexporter client.go (contrib PR #48598).
const (
	scopeName                 = "otel.profiling"
	scopeVersion              = "0.1.0"
	attrSplunkSourcetype      = "com.splunk.sourcetype"
	attrProfDataType          = "profiling.data.type"
	attrProfDataFormat        = "profiling.data.format"
	profFormatPprofGzipB64    = "pprof-gzip-base64"
	attrProfTotalFrameCount   = "profiling.data.total.frame.count"
	attrProfInstrSource       = "profiling.instrumentation.source"
	profInstrSourceContinuous = "continuous"
)

// Config controls exporter behavior. All fields map to OTel env vars where
// possible.
type Config struct {
	Endpoint           string            // e.g. "http://127.0.0.1:4318"; "/v1/logs" is appended
	ServiceName        string            // OTEL_SERVICE_NAME (else first app name)
	ServiceVersion     string            // OTEL_SERVICE_VERSION (optional)
	Environment        string            // OTEL_DEPLOYMENT_ENVIRONMENT (optional)
	SamplePeriod       time.Duration     // sampling period for cpu profiles (default 10ms)
	Headers            map[string]string // OTEL_EXPORTER_OTLP_HEADERS (optional)
	InsecureSkipVerify bool              // skip TLS cert verification (OTEL_EXPORTER_OTLP_INSECURE)
	Compression        string            // "gzip" (default) or "none"
}

// Exporter posts pprof profiles as OTLP Logs to a Splunk OTel collector.
type Exporter struct {
	cfg    Config
	client *http.Client
}

// New returns an Exporter using the given config and a default HTTP client.
func New(cfg Config) (*Exporter, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("otlp: empty endpoint")
	}
	if cfg.ServiceName == "" {
		return nil, errors.New("otlp: empty service.name")
	}
	if cfg.SamplePeriod == 0 {
		cfg.SamplePeriod = 10 * time.Millisecond
	}
	if cfg.Compression == "" {
		cfg.Compression = "gzip"
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify},
	}
	return &Exporter{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second, Transport: transport},
	}, nil
}

// ProfilingLogRecord builds a single LogRecord carrying one pprof profile,
// matching the splunk_hec profiling LogRecord contract. profileType is the
// value written to profiling.data.type; profileTime sets the LogRecord
// timestamp (also the per-sample source.event.time label origin, which callers
// set when building the profile).
func ProfilingLogRecord(prof *profile.Profile, profileType string, now time.Time) plog.LogRecord {
	var pprofBuf bytes.Buffer
	if err := prof.Write(&pprofBuf); err != nil {
		// prof.Write only fails on I/O; our bytes.Buffer never errors.
		// Surface as an empty body so the caller can detect via Size==0.
		prof = &profile.Profile{}
	}
	bodyStr := base64.StdEncoding.EncodeToString(pprofBuf.Bytes())

	lr := plog.NewLogRecord()
	lr.SetTimestamp(pcommon.NewTimestampFromTime(now))
	lr.SetObservedTimestamp(pcommon.NewTimestampFromTime(now))
	lr.Body().SetStr(bodyStr)
	lr.Attributes().PutStr(attrSplunkSourcetype, scopeName)
	lr.Attributes().PutStr(attrProfDataType, profileType)
	lr.Attributes().PutStr(attrProfDataFormat, profFormatPprofGzipB64)
	lr.Attributes().PutStr(attrProfInstrSource, profInstrSourceContinuous)
	var totalFrameCount int64
	for _, s := range prof.Sample {
		if len(s.Location) > 0 {
			totalFrameCount += int64(len(s.Location))
		}
	}
	lr.Attributes().PutInt(attrProfTotalFrameCount, totalFrameCount)
	return lr
}

// ExportProfiles sends a batch of (profile, profileType) pairs as a single
// OTLP Logs request. All profiles share this exporter's resource attributes.
// timestamp is used as the LogRecord timestamp for every record.
func (e *Exporter) ExportProfiles(ctx context.Context, records []ProfileRecord, timestamp time.Time) error {
	if len(records) == 0 {
		return nil
	}

	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	res := rl.Resource()
	res.Attributes().PutStr("service.name", e.cfg.ServiceName)
	res.Attributes().PutStr("telemetry.sdk.name", "php")
	res.Attributes().PutStr("telemetry.sdk.language", "php")
	if e.cfg.ServiceVersion != "" {
		res.Attributes().PutStr("service.version", e.cfg.ServiceVersion)
	}
	if e.cfg.Environment != "" {
		res.Attributes().PutStr("deployment.environment", e.cfg.Environment)
	}

	sl := rl.ScopeLogs().AppendEmpty()
	sl.Scope().SetName(scopeName)
	sl.Scope().SetVersion(scopeVersion)

	for _, r := range records {
		lr := sl.LogRecords().AppendEmpty()
		ProfilingLogRecord(r.Profile, r.ProfileType, timestamp).MoveTo(lr)
	}

	er := plogotlp.NewExportRequestFromLogs(ld)
	body, err := er.MarshalProto()
	if err != nil {
		return fmt.Errorf("otlp: marshal logs: %w", err)
	}
	return e.postProtobuf(ctx, "/v1/logs", body)
}

// ProfileRecord pairs a built pprof Profile with its profiling.data.type.
type ProfileRecord struct {
	Profile     *profile.Profile
	ProfileType string
}
