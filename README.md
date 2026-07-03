# otel-php-agent

A fork of the [New Relic PHP Agent](https://github.com/newrelic/newrelic-php-agent)
modified to send **profiling call-graph data** (pprof) to **Splunk AlwaysOn
Profiling** via a local Splunk distribution of the OpenTelemetry Collector,
using the **OTLP/HTTP Logs** signal.

The PHP C extension's existing segment-tree instrumentation is reused
unchanged: every instrumented transaction produces a segment tree that the
**Go daemon** decodes and converts into a [pprof](https://github.com/google/pprof)
`Profile`, then ships it to the local collector as OTLP Logs (one
`LogRecord` per profile, `Body = base64(gzip(pprof))`, scope `otel.profiling`).
See [PLAN.md](./PLAN.md) for the full design and the locked backend contract.

> **v1 scope:** profiling + traces + metrics. Errors, slow SQLs, transaction
> events, custom/error/log events, and the New Relic collector / Infinite
> Tracing gRPC roundtrips are **not** sent; the daemon runs without phoning
> home (`OTEL_PHP_NO_PHONE_HOME`).

---

## Architecture

```
PHP request ──(C extension, segment tree)──► flatbuffers ──► Go daemon
                                                              │
                                          TxnTrace.Data / SpanEvents / MetricTable
                                                              ▼
                              pprof (profile.go)   otlp.traces   otlp.metrics
                                                              ▼
                              otlp.Exporter.Export{Profiles,Traces,Metrics}
                                                              ▼
            OTLP/HTTP POST <endpoint>/v1/logs  /v1/traces  /v1/metrics  (gzip protobuf)
                                                              ▼
                                        Splunk OTel Collector
                                                              ▼
            Splunk Observability Cloud: Code Profiling + APM (`service.request*`)
```

The three OTLP signals are emitted over separate endpoints so a local
 Splunk OTel Collector can route each independently. The daemon maps New
 Relic data into OpenTelemetry conventions so the data lands in Splunk
 Observability already shaped like native OTel/`service.request` data:

- **/v1/logs** (profiling): one `LogRecord` per harvest with Body =
  base64(gzip(pprof)). Scope name `otel.profiling` triggers the collector's
  `splunk_hec/profiling` exporter route.
- **/v1/traces** (APM spans): each NR SpanEvent → OTLP Span, with `SpanKind`
  set to SERVER on root `WebTransaction/*` so Splunk APM AUTOMATICALLY
  derives the canonical `service.request`, `service.request.error`, and
  `service.root` service metrics server-side (charts: Service requests,
  Service latency, Service errors, SLI/SLO).
- **/v1/metrics** (per-harvest aggregates): each NR MetricTable record is
  converted to the equivalent OTel-semantic metric — e.g.
  `http.server.request.duration`, `http.client.request.duration`,
  `db.client.operation.duration` as **Histograms** with OTel default
  latency bucket boundaries (the figure Splunk's chart builder uses for
  percentile(90)/(99)/median), and `process.cpu.time`,
  `process.memory.usage`, `apdex`, `custom.*`, `agent.supportability.*`
  for non-latency signals. NR's six-field aggregate vector
  (count, total, exclusive, min, max, sum_of_squares) is preserved on the
data
  point; bucket counts are synthesized via a uniform-CDF approximation in
  [min, max] (v1 limitation — exact percentiles require per-sample data).

Key packages (under `daemon/`):

| Package | Responsibility |
| --- | --- |
| `internal/pprof` | `DecodeTrace` parses the `TxnTrace.Data` JSON envelope into an in-memory `Trace`/`Segment` tree; `BuildProfile` walks root→leaf paths and emits a pprof `Profile` with the Splunk AlwaysOn Profiling sample labels (`source.event.name`, `source.event.time`, `source.event.period`). |
| `internal/otlp` | `Exporter.Export{Profiles,Traces,Metrics}` build OTLP Logs / Traces / Metrics requests: profiling (scope `otel.profiling`, Body = `base64(gzip(pprof))` string + five `profiling.*` attributes) → `/v1/logs`; traces (NR SpanEvent → OTLP Span, root web txns → `SpanKind.SERVER`) → `/v1/traces`; metrics (NR MetricTable → OTel-semantic Histogram / Gauge / Sum) → `/v1/metrics`. |
| `internal/newrelic` | Harvest integration wires the exporter into `harvestAll`/`harvestByType`/`harvestMetrics`: profiling via `otlp_harvest.go` and traces/metrics via `otlp_signals.go`. `localConnect` provides the no-phone-home synthetic connect so apps reach `AppStateConnected` without contacting New Relic. |

---

## Quick start

Prebuilt binaries are published as **GitHub Releases** tarballs — you do **not**
need Go or a C toolchain to install. Grab the release tarball for your host
from the GitHub Releases page (on any machine with access) and copy it onto
the target host; the installer then runs **fully offline** against that local
archive. (If the target host has internet access there is an opt-in download
mode — see step 2.)

The installer is [`agent/install.sh`](./agent/install.sh) (also served raw
from the repo, like the upstream
[`newrelic-install.sh`](https://raw.githubusercontent.com/newrelic/newrelic-php-agent/refs/heads/main/agent/newrelic-install.sh)).
It extracts the tarball and runs the bundled `newrelic-install` script (the same
script the upstream New Relic agent ships), which installs the PHP extension,
the Go daemon, and an init script for every PHP it finds on the host.

This walks through: **collector → install → configure PHP → restart → verify**.
Assumes a host with PHP 7.2–8.5 (non-ZTS; amd64 or arm64) and a collector on
`127.0.0.1:4318`. Run installer/editor steps as `root`.

Release assets are named
`otel-php-agent_<version>_<os>_<arch>[_musl].tar.gz`, e.g.

- `otel-php-agent_1.0.0_linux_amd64.tar.gz` — glibc x86_64
- `otel-php-agent_1.0.0_linux_arm64_musl.tar.gz` — Alpine aarch64

Pick the one matching the *target* host (not the machine you download on).

### 1. Start a local OTLP/HTTP collector

Install the Splunk distribution of the OpenTelemetry Collector (or any OTel
collector with an OTLP/HTTP logs receiver and a `splunk_hec` profiling
exporter). A working local config has:

- an `otlphttp` receiver on `0.0.0.0:4318`,
- a logs pipeline routing `Scope().Name() == "otel.profiling"` to a
  `splunk_hec/profiling` exporter (`profiling_data_enabled`, pointing at the
  Splunk ingest HEC `/v1/log` with an access token).

(See `spike/main.go` and [PLAN.md](./PLAN.md) for the routing contract and
attribute keys.)

### 2. Install (offline by default)

Copy the release tarball **and** `install.sh` onto the target host (scp, USB,
internal artifact mirror, etc.). Then run, as `root`:

```sh
sudo sh install.sh ./otel-php-agent_1.0.0_linux_amd64.tar.gz install
```

`install.sh` extracts the archive to a temp dir and runs the bundled
`newrelic-install`. You can also point it at an **already-extracted** bundle
directory, or let it auto-find a matching tarball in the current directory:

```sh
# already-extracted bundle dir
sudo sh install.sh ./otel-php-agent-linux-amd64 install

# auto-find a *_<os>_<arch>*.tar.gz in the current directory
sudo sh install.sh install
```

The bundled `newrelic-install` finds every PHP installation on the box and,
for each:

- links the ABI-matching `newrelic-<PHP_API_VERSION>.so` into PHP's extension
  dir as `newrelic.so`,
- writes a `newrelic.ini` (from `newrelic.ini.template`, with
  `REPLACE_WITH_REAL_KEY` filled in) into each PHP's INI scan dir (or
  `mods-available` + per-SAPI `20-newrelic.ini` symlinks on Debian),
- installs the daemon at `/usr/bin/newrelic-daemon`, plus a platform init
  script (`init.<ostype>`), and (re)starts it.

Non-interactive / overrides (forwarded to `newrelic-install`):

```sh
sudo NR_INSTALL_SILENT=1 sh install.sh ./otel-php-agent_1.0.0_linux_amd64.tar.gz install
sudo NR_INSTALL_DAEMONPATH=/usr/bin/newrelic-daemon \
     sh install.sh ./otel-php-agent_1.0.0_linux_amd64.tar.gz install
sudo NR_INSTALL_PATH=/path/to/php-config-dir \
     sh install.sh ./otel-php-agent_1.0.0_linux_amd64.tar.gz install   # pin a PHP
```

#### 2a. If you skipped `install.sh` (run `newrelic-install` directly)

The tarball is self-contained — it **is** the release bundle the installer
extracts. You can skip `install.sh` entirely:

```sh
tar xzf otel-php-agent_1.0.0_linux_amd64.tar.gz
sudo ./newrelic-install install
```

#### 2b. Opt-in: download from GitHub (requires internet access)

Only if the target host has internet access, set `INSTALL_DOWNLOAD=1` and
`INSTALL_REPO=owner/repo`; `install.sh` will fetch the tarball from the GitHub
Releases asset endpoint instead of expecting a local file:

```sh
sudo INSTALL_DOWNLOAD=1 INSTALL_REPO=OWNER/otel-php-agent \
     INSTALL_VERSION=v1.0.0 sh install.sh install
```

Without `INSTALL_DOWNLOAD=1`, `install.sh` never touches the network.

### 3. Configure egress (`newrelic.otel_*` INI keys)

The installer already wrote a `newrelic.ini` for each PHP. Edit it to point at
your collector. The agent forwards these INI keys to the daemon as standard
`OTEL_*` env vars when it launches it (see `agent/php_minit.c`).

Find the file(s) it created:

```sh
php --ini | grep newrelic            # location of the loaded newrelic.ini
```

Then edit that `newrelic.ini`:

```ini
[newrelic]
newrelic.license            = "..."                  ; any non-empty value (no phone-home by default)
newrelic.appname            = "php-checkout"

; OpenTelemetry / Splunk AlwaysOn Profiling
newrelic.otel_endpoint          = "http://127.0.0.1:4318"   ; enables OTLP egress + no-phone-home
newrelic.otel_service_name      = "php-checkout"
newrelic.otel_environment        = "production"
newrelic.otel_service_version   = "1.0.0"
newrelic.otel_profile_type      = "cpu"                      ; cpu (default) | wall
newrelic.otel_sample_period     = "10ms"
newrelic.otel_exporter_headers  = "X-Splunk-Access-Token=..."
; newrelic.otel_exporter_insecure = "1"                      ; set for self-signed collectors
; newrelic.otel_no_phone_home      = "1"                       ; already on when otel_endpoint is set
```

`OTEL_EXPORTER_OTLP_ENDPOINT` (set via `newrelic.otel_endpoint`) is the master
switch: it enables profiling + traces + metrics egress, defaults
no-phone-home on, and also auto-enables Code Level Metrics
(`newrelic.code_level_metrics.enabled`) so flamegraph frames carry
file/line ("module") info — unless that setting is explicitly configured
(true or false) in an ini file, in which case the explicit value always wins.
The full annotated list is in
[`agent/scripts/newrelic.ini.template`](./agent/scripts/newrelic.ini.template).

#### Daemon-only env vars (no INI key)

These are read directly by the daemon from its environment, so set them in the
daemon's launch environment (e.g. `/etc/default/newrelic-daemon`,
`/etc/sysconfig/newrelic-daemon`, or your container env):

| Env var | Purpose |
| --- | --- |
| `OTEL_PHP_TRACES_DISABLED` | Set (any value) ⇒ NR SpanEvents are NOT converted to OTLP `/v1/traces`. Default: traces egress is on. |
| `OTEL_PHP_METRICS_DISABLED` | Set (any value) ⇒ NR MetricTable is NOT converted to OTLP `/v1/metrics`. Default: metrics egress is on. |
| `OTEL_PHP_PHONE_HOME` | Set to `1` to opt *back in* to the New Relic connect/preconnect roundtrip. |

#### Full daemon env-var reference

The `newrelic.otel_*` INI keys above are the per-PHP way to set the standard
`OTEL_*` vars; the raw env vars below are useful when running the daemon
standalone / under systemd:

| Env var | Purpose |
| --- | --- |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Collector base URL, e.g. `http://127.0.0.1:4318`. **Setting this enables OTLP profiling + traces + metrics egress.** |
| `OTEL_SERVICE_NAME` | `service.name`. Falls back to the first `newrelic.appname` segment if unset. |
| `OTEL_SERVICE_VERSION` | `service.version` (optional). |
| `OTEL_DEPLOYMENT_ENVIRONMENT` | `deployment.environment` (optional). |
| `OTEL_EXPORTER_OTLP_HEADERS` | Comma-separated `key=value` headers (e.g. an access token). |
| `OTEL_EXPORTER_OTLP_INSECURE` | Truthy ⇒ skip TLS cert verification (self-signed collectors). |
| `OTEL_PHP_PROFILE_TYPE` | `cpu` (default) or `wall`. |
| `OTEL_PHP_SAMPLE_PERIOD` | CPU sample period (Go duration, default `10ms`). |
| `OTEL_PHP_NO_PHONE_HOME` | Truthy ⇒ skip the New Relic connect/preconnect roundtrip. **Defaulted on whenever `OTEL_EXPORTER_OTLP_ENDPOINT` is set.** |

Both `http://` and `https://` endpoints are supported; the body is OTLP/protobuf,
gzip-compressed.

### 4. Restart PHP and verify

```sh
sudo systemctl restart php-fpm            # or httpd / apache2 / php8.2-fpm
sudo systemctl restart newrelic-daemon    # if you use the init script; else the agent auto-launches it

php -m | grep newrelic                      # should print "newrelic"
php -i | grep -E 'newrelic.otel_endpoint|newrelic.appname'
```

Then hit your app a few times; within one harvest cycle (~5 s) you should see
profile/spans/metrics POSTs to `http://127.0.0.1:4318/v1/{logs,traces,metrics}`
(`sudo tcpdump -A -s0 'tcp port 4318'`).

#### Uninstall

From the same extracted release bundle (or the local tarball):

```sh
sudo sh install.sh ./otel-php-agent_1.0.0_linux_amd64.tar.gz uninstall
# or, if you extracted it manually:
sudo ./newrelic-install uninstall
```

---

## Testing

- `daemon/internal/pprof/profile_test.go` — segment-tree decode + pprof build,
  label assertions, and a **golden fixture test** (`TestBuildProfile_GoldenPaths`)
  that locks the exact root→leaf call stacks.
- `daemon/internal/otlp/exporter_test.go` — OTLP LogRecord contract shape and
  an optional live-collector test.
- `daemon/internal/newrelic/otlp_harvest_test.go` — no-phone-home connect and
  an optional live `harvestProfiles` test.
- `daemon/cmd/smoke/main.go` — **end-to-end smoke harness**: an in-process mock
  OTLP/HTTP collector validates the whole pprof→OTLP Logs round-trip and the
  Splunk AlwaysOn Profiling LogRecord contract with no external dependencies.

Run the smoke harness:

```sh
cd daemon && go run ./cmd/smoke
# or: make daemon_smoke
```

---

## v1 limitations / future work

- **Profiling + traces + metrics only.** Errors, slow SQLs, transaction
  events, custom/error/log events, and other New Relic signals are dropped
  from egress in v1.
- **No phoning home.** The New Relic preconnect/connect roundtrip is replaced by
  a local synthetic connect (`localConnect`); Infinite Tracing gRPC is disabled
  (the trace observer stays nil, span batches are dropped). The
  `internal/newrelic/infinite_tracing` package and gRPC dependencies remain in
  the tree as dead code pending cleanup — they are not invoked on the profiling
  path.
- **Function-level only.** No source line numbers (avoids C-extension changes).
- **W3C trace propagation**, NR→OTLP `/v1/traces` and `/v1/metrics` are
  implemented; line-level pprof remains deferred.

---

## License

The PHP agent is licensed under the [Apache 2.0](https://apache.org/licenses/LICENSE-2.0.txt)
License and also uses source code from third-party libraries. See the
third-party notices document for details. This fork retains that license.