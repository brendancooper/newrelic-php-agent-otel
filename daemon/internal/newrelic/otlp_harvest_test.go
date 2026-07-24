package newrelic

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/newrelic/newrelic-php-agent/daemon/internal/otlp"
	"github.com/newrelic/newrelic-php-agent/daemon/internal/pprof"
)

// realTraceData is a TxnTrace.Data JSON string in the envelope shape produced
// by axiom/nr_segment_traces.c (see daemon/internal/pprof/profile_test.go for
// the backtick-index-wrapper rationale).
const realTraceData = `[
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

func TestLocalConnect_SynthesizesConnectedReply(t *testing.T) {
	var reply struct {
		CollectTraces bool `json:"collect_traces"`
	}
	if err := json.Unmarshal(syntheticConnectReply("test"), &reply); err != nil {
		t.Fatalf("unmarshal synthetic connect reply: %v", err)
	}
	if !reply.CollectTraces {
		t.Fatal("synthetic connect reply must enable collect_traces")
	}

	args := &ConnectArgs{
		AppKey: AppKey{License: "x", Appname: "test"},
	}
	rep := localConnect(args, "php-test")

	if rep.Err != nil {
		t.Fatalf("localConnect Err: %v", rep.Err)
	}
	if rep.Reply == nil || rep.Reply.ID == nil || *rep.Reply.ID == "" {
		t.Fatal("localConnect did not synthesize a run id")
	}
	if !strings.HasPrefix(string(*rep.Reply.ID), "local-") {
		t.Errorf("run id = %q, want local- prefix", *rep.Reply.ID)
	}
	if rep.Reply.EventHarvestConfig.ReportPeriod == 0 {
		t.Error("synthetic EventHarvestConfig.ReportPeriod is zero")
	}
	// processConnectAttempt must accept this and reach Connected.
	app := &App{state: AppStateUnknown, info: &AppInfo{}}
	p := &Processor{
		apps:     map[AppKey]*App{args.AppKey: app},
		harvests: map[AgentRunID]*AppHarvest{},
	}
	p.processConnectAttempt(rep)

	if app.state != AppStateConnected {
		t.Fatalf("app state = %v, want AppStateConnected", app.state)
	}
	if len(p.harvests) != 1 {
		t.Fatalf("harvests registered = %d, want 1", len(p.harvests))
	}
}

// TestHarvestProfiles_LiveCollector end-to-end exercises the OTLP harvest
// hook: build a TxnTraces bucket with a real segment-tree JSON, run
// harvestProfiles against the local Splunk collector on :4318, and confirm
// the splunk_hec/profiling exporter advances its sent counter.
func TestHarvestProfiles_LiveCollector(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live collector test in short mode")
	}
	endpoint := "http://127.0.0.1:4318"
	if !reachability("http://127.0.0.1:13133/health") {
		t.Skip("no splunk otel collector reachable on :13133")
	}
	if sentCounterSelector(t) == "" {
		t.Skip("collector metrics endpoint does not expose splunk_hec/profiling counter")
	}

	before := profilingSentCounter(t)

	traces := NewTxnTraces()
	tt := &TxnTrace{
		MetricName:          "WebTransaction/Function/index",
		UnixTimestampMillis: float64(time.Now().Add(-1 * time.Second).UnixMilli()),
		DurationMillis:      2000,
		Data:                []byte(realTraceData),
		GUID:                "test-guid",
	}
	if !traces.IsKeeper(tt) {
		t.Fatal("trace rejected by IsKeeper")
	}
	traces.AddTxnTrace(tt)

	exp, err := otlp.New(otlp.Config{
		Endpoint:    endpoint,
		ServiceName: "php-otel-test",
		Environment: "test",
	})
	if err != nil {
		t.Fatalf("otlp.New: %v", err)
	}

	cfg := OTLPConfig{Endpoint: endpoint, ServiceName: "php-otel-test", Environment: "test"}
	harvestProfiles(traces, exp, pprof.TypeCPU, time.Now(), cfg)

	for i := 0; i < 40; i++ {
		time.Sleep(500 * time.Millisecond)
		if after := profilingSentCounter(t); after > before {
			t.Logf("profiling sent counter %d -> %d", before, after)
			return
		}
	}
	t.Fatalf("profiling sent counter did not advance (before=%d)", before)
}

func reachability(url string) bool {
	resp, err := http.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return resp.StatusCode < 500
}

// sentCounterSelector returns the metric name substring used to find the
// profiling sent counter in the collector's /metrics scrape.
func sentCounterSelector(t *testing.T) string {
	resp, err := http.Get("http://127.0.0.1:8888/metrics")
	if err != nil {
		t.Fatalf("scrape metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, `exporter="splunk_hec/profiling"`) &&
			strings.HasPrefix(line, "otelcol_exporter_sent_log_records") {
			return line
		}
	}
	return ""
}

func profilingSentCounter(t *testing.T) int {
	resp, err := http.Get("http://127.0.0.1:8888/metrics")
	if err != nil {
		t.Fatalf("scrape metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, line := range strings.Split(string(body), "\n") {
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
		for _, ch := range line[idx+1:] {
			if ch >= '0' && ch <= '9' {
				n = n*10 + int(ch-'0')
			}
		}
		return n
	}
	t.Fatalf("splunk_hec/profiling sent counter not found")
	return 0
}
