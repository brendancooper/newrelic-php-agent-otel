# otel-php-agent

A fork of the [New Relic PHP Agent](https://github.com/newrelic/newrelic-php-agent)
that sends profiling, trace, and metrics data to **Splunk Observability
Cloud** via a local Splunk distribution of the OpenTelemetry Collector, using
OTLP/HTTP.

## Copyright and License

This project is a derivative work of the New Relic PHP Agent,
Copyright © New Relic Corporation. All rights not explicitly granted remain
with New Relic and its third-party licensors.

The agent is licensed under the [Apache License 2.0](./LICENSE). It bundles
source from several third-party libraries, each carrying its own copyright
and license — see [THIRD_PARTY_NOTICES.md](./THIRD_PARTY_NOTICES.md) for the
full list and attributions. This fork retains the original license and
copyright notices unchanged; only the modifications described below have
been added on top.

> **v1 scope:** profiling + traces + metrics only. Errors, slow SQLs,
> transaction events, custom/error/log events, and the New Relic collector /
> Infinite Tracing gRPC roundtrips are dropped; the daemon runs without
> phoning home to New Relic (`OTEL_PHP_NO_PHONE_HOME`).

---

## Architecture

```
PHP request ──(C extension, segment tree)──► Go daemon
                                                   │
                                    pprof · OTLP traces · OTLP metrics
                                                   ▼
                                     Splunk OTel Collector (OTLP/HTTP)
                                                   ▼
                              Splunk Observability Cloud (APM + Profiling)
```

The PHP C extension's existing instrumentation is reused unchanged — each
instrumented transaction still produces a segment tree, span data, and
metrics. The Go daemon converts these into standard OpenTelemetry shapes
(pprof profiles, OTLP spans, OTLP metrics) and ships them to a local OTLP/HTTP
collector instead of the New Relic backend. 

---

## Install

Releases are distributed as a self-contained tarball — no Go or C toolchain
needed on the target host.

```sh
tar xzf otel-php-agent_<version>_<os>_<arch>.tar.gz
cd otel-php-agent_<version>_<os>_<arch>
sudo ./newrelic_install install
```

`newrelic_install` finds every PHP installation on the host and, for each,
installs the extension, writes a `newrelic.ini`, and installs/starts the Go
daemon. After install, edit the generated `newrelic.ini` to set
`newrelic.otel_endpoint` to your collector's address (e.g.
`http://127.0.0.1:4318`), then restart PHP.

To uninstall:

```sh
sudo ./newrelic_install uninstall
```

---

## Changes from upstream New Relic PHP Agent

- **New Go daemon exporters** (`daemon/internal/otlp`, `daemon/internal/pprof`):
  convert New Relic's internal segment trees, span events, and metric tables
  into pprof profiles and OTLP traces/metrics, and send them to a collector
  over OTLP/HTTP instead of the New Relic backend.
- **No phone-home**: the New Relic preconnect/connect handshake is replaced
  by a local synthetic connect (`localConnect`) so the agent runs fully
  offline from New Relic. Infinite Tracing (gRPC) is disabled.
- **W3C trace context**: a new `newrelic.otel_w3c_trace_id` setting generates
  a spec-compliant 128-bit trace ID at the root transaction, used for the
  OTLP traces/metrics egress.
- **New `newrelic.otel_*` INI settings** (forwarded to the daemon as standard
  `OTEL_*` environment variables): endpoint, service name/version,
  environment, exporter headers/insecure flag, profile type, and sample
  period. Setting `newrelic.otel_endpoint` is the master switch that enables
  OTLP egress and no-phone-home.
- **Code Level Metrics auto-enable**: `newrelic.code_level_metrics.enabled`
  is automatically turned on whenever OTLP egress is enabled (so profiles
  carry file/function info), unless explicitly set otherwise.