//
// Copyright 2020 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//

// This file wires the OTLP profiling exporter into the harvest path and
// provides a "no phone home" local connect so an app reaches AppStateConnected
// without contacting the New Relic collector. Both are part of the OTLP/PHP
// profiling conversion (Phase 1 harvest integration + a minimal slice of the
// Phase 2 "no phone home" work pulled forward so a real PHP request actually
// produces a profile).
package newrelic

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/newrelic/newrelic-php-agent/daemon/internal/newrelic/collector"
	"github.com/newrelic/newrelic-php-agent/daemon/internal/newrelic/limits"
	"github.com/newrelic/newrelic-php-agent/daemon/internal/newrelic/log"
	"github.com/newrelic/newrelic-php-agent/daemon/internal/otlp"
	"github.com/newrelic/newrelic-php-agent/daemon/internal/pprof"
)

// OTLPConfig configures the local OTLP profiling egress. When Endpoint is
// non-empty, an App's transaction traces are converted to pprof and POSTed to
// the Splunk OTel collector's OTLP Logs receiver. When NoPhoneHome is true,
// ConnectApplication is replaced by a local synthetic connect (no NR
// preconnect/connect roundtrip).
type OTLPConfig struct {
	Endpoint           string            // e.g. "http://127.0.0.1:4318"; "/v1/logs" is appended by the exporter
	ServiceName        string            // OTEL_SERVICE_NAME; defaults to first app name when empty
	ServiceVersion     string            // OTEL_SERVICE_VERSION (optional)
	Environment        string            // OTEL_DEPLOYMENT_ENVIRONMENT (optional)
	SamplePeriod       time.Duration     // cpu sampling period (default 10ms)
	ProfileType        string            // "cpu" or "wall"; default "cpu"
	NoPhoneHome        bool              // skip NR connect/preconnect; set App state locally
	Headers            map[string]string // OTEL_EXPORTER_OTLP_HEADERS (optional)
	InsecureSkipVerify bool              // skip TLS cert verification
	Compression        string            // "gzip" (default) or "none"

	// EmitTraces enables NR SpanEvents -> OTLP /v1/traces conversion.
	EmitTraces bool
	// EmitMetrics enables NR MetricTable -> OTLP /v1/metrics conversion.
	EmitMetrics bool
}

// profileType returns the configured pprof profile type (validated), defaulting
// to CPU.
func (c OTLPConfig) profileType() pprof.ProfileType {
	if c.ProfileType == string(pprof.TypeWall) {
		return pprof.TypeWall
	}
	return pprof.TypeCPU
}

// localRunID generates a fresh, locally-unique agent run id without contacting
// any collector.
func localRunID() AgentRunID {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read should never fail on Linux; fall back to a timestamp.
		return AgentRunID(fmt.Sprintf("local-%d", time.Now().UnixNano()))
	}
	return AgentRunID("local-" + hex.EncodeToString(b[:]))
}

// syntheticConnectReply builds a ConnectReply JSON body with sane defaults so
// that parseConnectReply populates EventHarvestConfig / sampling fields without
// requiring a real collector roundtrip.
func syntheticConnectReply(id AgentRunID) []byte {
	type harvestLimits struct {
		ErrorEventData    *int `json:"error_event_data"`
		AnalyticEventData *int `json:"analytic_event_data"`
		CustomEventData   *int `json:"custom_event_data"`
		SpanEventData     *int `json:"span_event_data"`
		LogEventData      *int `json:"log_event_data"`
	}
	// all event harvest limits set to 0 => v1 drops every non-profiling event
	// signal (custom/error/analytic/span/log events); only TxnTraces survive and
	// are emitted as pprof by the OTLP profiling path. Span events get the
	// full default capacity so the OTLP traces egress has span data to send.
	zero := 0
	spanDefault := limits.MaxSpanMaxEvents
	type eventHarvest struct {
		ReportPeriodMS uint64        `json:"report_period_ms"`
		HarvestLimits  harvestLimits `json:"harvest_limits"`
	}
	type spanHarvest struct {
		ReportPeriodMS uint64 `json:"report_period_ms"`
		SpanEventData  *int   `json:"harvest_limit"`
	}
	type reply struct {
		AgentRunID          AgentRunID   `json:"agent_run_id"`
		EventHarvestConfig  eventHarvest `json:"event_harvest_config"`
		SpanEventHarvest    spanHarvest  `json:"span_event_harvest_config"`
		SamplingFrequency   int          `json:"sampling_target_period_in_seconds"`
		SamplingTarget      int          `json:"sampling_target"`
		CollectTraces       bool         `json:"collect_traces"`
		MaxPayloadSizeBytes int          `json:"max_payload_size_in_bytes"`
	}
	reportPeriodMs := uint64(limits.DefaultReportPeriod / time.Millisecond)
	r := reply{
		AgentRunID: id,
		EventHarvestConfig: eventHarvest{
			ReportPeriodMS: reportPeriodMs,
			HarvestLimits: harvestLimits{
				ErrorEventData:    &zero,
				AnalyticEventData: &zero,
				CustomEventData:   &zero,
				SpanEventData:     &spanDefault,
				LogEventData:      &zero,
			},
		},
		SpanEventHarvest:    spanHarvest{ReportPeriodMS: reportPeriodMs, SpanEventData: &spanDefault},
		SamplingFrequency:   60,
		SamplingTarget:      10,
		CollectTraces:       true,
		MaxPayloadSizeBytes: limits.DefaultMaxPayloadSizeInBytes,
	}
	b, _ := json.Marshal(r)
	return b
}

// localConnect synthesizes a ConnectAttempt for an app without contacting the
// NR collector. The returned attempt has no error and carries a defaulted
// ConnectReply, so processConnectAttempt transitions the app to
// AppStateConnected.
func localConnect(args *ConnectArgs, serviceName string) ConnectAttempt {
	id := localRunID()
	body := syntheticConnectReply(id)
	reply, err := parseConnectReply(body)
	if err != nil {
		return ConnectAttempt{Key: args.AppKey, Err: fmt.Errorf("local connect: %w", err)}
	}
	log.Infof("otlp: local (no-phone-home) connect for app %q with run id %q", args.AppKey, id)
	return ConnectAttempt{
		Key:                 args.AppKey,
		Collector:           "local",
		Reply:               reply,
		RawReply:            collector.RPMResponse{StatusCode: 200, Body: body},
		RawSecurityPolicies: []byte("{}"),
	}
}

// harvestProfiles converts the collected TxnTraces into pprof profiles and
// POSTs them to the OTLP collector. It is the OTLP equivalent of
// considerHarvestPayload for the TxnTraces bucket. Called as a goroutine by
// the harvest path; safe to call with a nil exporter (no-op).
func harvestProfiles(traces *TxnTraces, exp *otlp.Exporter, profileType pprof.ProfileType, harvestStart time.Time, cfg OTLPConfig) {
	if exp == nil || traces == nil || traces.Empty() {
		return
	}

	records := collectProfileRecords(traces, profileType, cfg)
	if len(records) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := exp.ExportProfiles(ctx, records, harvestStart); err != nil {
		log.Errorf("otlp: failed to export %d profile(s) to %s: %v", len(records), cfg.Endpoint, err)
	}
}

// collectProfileRecords decodes every collected TxnTrace into a pprof Profile
// and returns the batch. Malformed traces are skipped with a warning so a
// single bad trace can't poison the whole harvest.
func collectProfileRecords(traces *TxnTraces, profileType pprof.ProfileType, cfg OTLPConfig) []otlp.ProfileRecord {
	period := cfg.SamplePeriod
	if period == 0 {
		period = 10 * time.Millisecond
	}

	var records []otlp.ProfileRecord
	add := func(heap *TxnTraceHeap) {
		for _, tt := range *heap {
			tr, err := pprof.DecodeTrace(tt.Data, tt.MetricName, tt.UnixTimestampMillis)
			if err != nil {
				log.Warnf("otlp: skipping unparseable trace %q: %v", tt.MetricName, err)
				continue
			}
			if tr.HasUnresolvedNames() {
				log.Warnf("otlp: trace %q has unresolved backtick segment names (its TxnTrace.Data envelope has no usable string table); pprof functions will be named by raw index", tt.MetricName)
			}
			prof := pprof.BuildProfile(tr, profileType, period)
			records = append(records, otlp.ProfileRecord{
				Profile:     prof,
				ProfileType: string(profileType),
			})
		}
	}
	add(traces.regular)
	add(traces.forcePersisted)
	add(traces.synthetics)
	return records
}
