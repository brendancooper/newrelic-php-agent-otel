//
// Copyright 2020 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//

// otlp_signals.go wires SpanEvents / MetricTable into the OTLP traces and
// metrics egress (Phase 1 traces/metrics follow-on). Both run alongside the
// profiling path, gated by OTLPConfig.EmitTraces / EmitMetrics.
package newrelic

import (
	"context"
	"time"

	"github.com/newrelic/newrelic-php-agent/daemon/internal/newrelic/log"
	"github.com/newrelic/newrelic-php-agent/daemon/internal/otlp"
)

// harvestTraces converts SpanEvents to OTLP traces and POSTs them. It's the
// OTLP counterpart of considerHarvestPayload for the SpanEvents bucket. Safe
// to call with a nil exporter (no-op). Mutates the SpanEvents bucket by
// merging failed-event data into a supplied harvests' failure sink at the end
// (the caller's harvest goroutine already moved the snapshot out, so
// failures on the in-flight snapshot are dropped; the daemon already saw
// numSeen/numSaved metrics).
func harvestTraces(events *SpanEvents, exp *otlp.Exporter, harvestStart time.Time, cfg OTLPConfig) {
	if exp == nil || events == nil || events.Empty() {
		return
	}
	raw := spanEventDataBytes(events)
	if len(raw) == 0 {
		return
	}
	td, err := exp.BuildTraces(raw, harvestStart)
	if err != nil && td.SpanCount() == 0 {
		log.Warnf("otlp: no traces built: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := exp.ExportTraces(ctx, td, harvestStart); err != nil {
		log.Warnf("otlp: export traces failed: %v", err)
	}
}

// harvestMetricsOTLP converts a MetricTable snapshot to OTLP metrics and
// POSTs them. It's the OTLP counterpart of considerHarvestPayload for the
// metrics bucket. Called as a goroutine by harvestMetrics when OTLP metrics
// are enabled.
func harvestMetricsOTLP(metrics *MetricTable, exp *otlp.Exporter, harvestStart time.Time, cfg OTLPConfig) {
	if exp == nil || metrics == nil || metrics.Empty() {
		return
	}
	records := metricTableRecords(metrics, harvestStart)
	if len(records) == 0 {
		return
	}
	md := exp.BuildMetrics(records)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := exp.ExportMetrics(ctx, md, harvestStart); err != nil {
		log.Warnf("otlp: export metrics failed: %v", err)
	}
}

// spanEventDataBytes returns the raw JSON bytes for each SpanEvent in the
// in-memory reservoir. The slice is len-ordinary; callers must not assume
// the reservoir's contents remain stable past this call.
func spanEventDataBytes(events *SpanEvents) [][]byte {
	if events == nil || events.analyticsEvents == nil || events.analyticsEvents.events == nil {
		return nil
	}
	out := make([][]byte, 0, len(*events.analyticsEvents.events))
	for _, e := range *events.analyticsEvents.events {
		out = append(out, []byte(e.data))
	}
	return out
}

// metricTableRecords copies NR metric instances out of an in-memory
// MetricTable into the flat records the OTLP builder consumes. Iterates the
// private map inner→outer→*metric to avoid the JSON round-trip through
// CollectorJSON.
func metricTableRecords(mt *MetricTable, harvestStart time.Time) []otlp.MetricRecord {
	if mt == nil || mt.metrics == nil {
		return nil
	}
	// mt.metrics is map[string]map[string]*metric (outer name, inner scope).
	out := make([]otlp.MetricRecord, 0, mt.count)
	for name, scopes := range mt.metrics {
		for scope, m := range scopes {
			data := m.data
			out = append(out, otlp.MetricRecord{
				Name:       name,
				Scope:      scope,
				Count:      data.countSatisfied,
				Total:      data.totalTolerated,
				Exclusive:  data.exclusiveFailed,
				Min:        data.min,
				Max:        data.max,
				SumSquares: data.sumSquares,
				Timestamp:  harvestStart,
			})
		}
	}
	return out
}
