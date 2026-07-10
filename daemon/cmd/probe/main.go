// Command probe sends synthetic agent records directly into a running
// newrelic-daemon over its listener socket, to verify the daemon is alive
// and accepting/processing data — without needing a real PHP agent.
//
// It performs the same handshake a PHP agent does:
//
//  1. Connect to the daemon socket (default @newrelic on Linux,
//     /tmp/.newrelic.sock elsewhere).
//  2. Send an AppInfo message and read the AppReply; on status Connected,
//     capture the returned agent_run_id.
//  3. Send N Transaction messages carrying a TxnTrace (→ pprof via OTLP
//     Logs), SpanEvents (→ OTLP Traces), and Metrics (→ OTLP Metrics),
//     reusing the shared flatbuffersdata.SampleTxn fixture.
//
// Whether the daemon then exports to an OTel backend depends on the env the
// daemon was launched with (OTEL_EXPORTER_OTLP_ENDPOINT etc.); check the
// daemon log for the "otlp: failed to export ..." lines the probe's records
// should trigger (or success). Exit code 0 if every stage round-tripped,
// non-zero otherwise.
//
// Usage:
//
//	go run ./cmd/probe [--port @newrelic] [--app probe] [--count 1]
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"github.com/newrelic/newrelic-php-agent/daemon/internal/flatbuffersdata"
	"github.com/newrelic/newrelic-php-agent/daemon/internal/newrelic"
	"github.com/newrelic/newrelic-php-agent/daemon/internal/newrelic/protocol"
)

const helpMessage = `Usage: probe [OPTIONS]

Send synthetic records directly into a running newrelic-daemon socket.

OPTIONS
  --port=ADDR    Daemon socket: TCP host:port or unix path/abstract @name
                 [default: platform default, @newrelic on Linux]
  --app=NAME     Application name to register [default: probe]
  --count=N      Number of transaction messages to send [default: 1]
  --retries=N    AppInfo handshake attempts before giving up [default: 10]
  --timeout=D    Per-message dial/read timeout [default: 10s]

DESCRIPTION
  The probe verifies the daemon is accepting connections and processing
  agent records. It performs the AppInfo handshake then sends transaction
  messages that exercise the profiling (TxnTrace), traces (SpanEvents), and
  metrics paths. If the daemon is configured with an OTLP endpoint
  (OTEL_EXPORTER_OTLP_ENDPOINT), those records flow to the backend; check the
  daemon log for export success or the "otlp: failed to export ..." errors.

EXAMPLES
  probe                        # one txn to the default socket
  probe --count 5 --app myapp    # five txns, custom app name
  probe --port 127.0.0.1:33142   # a daemon on a TCP port
`

func main() {
	addr := flag.String("port", newrelic.DefaultListenSocket(), "")
	app := flag.String("app", "probe", "")
	count := flag.Int("count", 1, "")
	timeout := flag.Duration("timeout", 10*time.Second, "")
	retries := flag.Int("retries", 10, "number of AppInfo handshake attempts before giving up")
	flag.Usage = func() { fmt.Fprint(os.Stderr, helpMessage, '\n') }
	flag.Parse()
	if *count < 1 {
		fmt.Fprintln(os.Stderr, "probe: --count must be >= 1")
		os.Exit(2)
	}

	if err := run(*addr, *app, *count, *timeout, *retries); err != nil {
		fmt.Fprintf(os.Stderr, "probe: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("probe: PASS")
}

func run(addr, appName string, count int, timeout time.Duration, retries int) error {
	conn, err := net.DialTimeout(networkFor(addr), addr, timeout)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()
	fmt.Printf("probe: connected to %s\n", addr)

	mw := newrelic.MessageWriter{W: conn, Type: newrelic.MessageTypeBinary}

	info := flatbuffersdata.SampleAppInfo
	info.Appname = appName
	appInfoMsg, err := flatbuffersdata.MarshalAppInfo(&info)
	if err != nil {
		return fmt.Errorf("marshal appinfo: %w", err)
	}

	// The daemon replies to the first AppInfo with the current app state
	// (Unknown for a brand-new app) and only then kicks off the async
	// connect. Poll until the app reports Connected/StillValid, exactly as a
	// real agent / the stressor does.
	var runID newrelic.AgentRunID
	waiting := false
	for attempt := 1; attempt <= retries; attempt++ {
		if timeout > 0 {
			_ = conn.SetDeadline(time.Now().Add(timeout))
		}
		if _, err := mw.Write(appInfoMsg); err != nil {
			return fmt.Errorf("send appinfo: %w", err)
		}
		reply, err := readMessage(conn)
		if err != nil {
			return fmt.Errorf("read appinfo reply: %w", err)
		}
		id, status, err := parseAppReply(reply)
		if err != nil {
			return err
		}
		switch status {
		case protocol.AppStatusConnected, protocol.AppStatusStillValid:
			runID = id
			fmt.Printf("probe: app %q connected (attempt %d) with run id %q\n", appName, attempt, string(runID))
			goto connected
		case protocol.AppStatusUnknown, protocol.AppStatusDisconnected:
			if !waiting {
				fmt.Printf("probe: app %q is %s; waiting for daemon to connect (will retry up to %d times)\n", appName, status, retries)
				waiting = true
			}
			time.Sleep(500 * time.Millisecond)
		default: // InvalidLicense etc.
			return fmt.Errorf("app not connected: status=%s", status)
		}
	}
	return fmt.Errorf("app %q did not connect after %d attempts", appName, retries)

connected:
	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	txn := flatbuffersdata.SampleTxn
	txn.RunID = string(runID)
	txnMsg, err := txn.MarshalBinary()
	if err != nil {
		return fmt.Errorf("marshal txn: %w", err)
	}

	for i := range count {
		n, err := mw.Write(txnMsg)
		if err != nil {
			return fmt.Errorf("send txn #%d: %w", i+1, err)
		}
		fmt.Printf("probe: sent txn %d (%d bytes)\n", i+1, n)
	}

	fmt.Println("probe: records delivered; check the daemon log for OTLP export results")
	return nil
}

// parseAppReply decodes an AppReply message into the agent run id and app
// status. Returns the run id (empty when not Connected) and the status.
func parseAppReply(reply []byte) (newrelic.AgentRunID, protocol.AppStatus, error) {
	msg := protocol.GetRootAsMessage(reply, 0)
	if msg.DataType() != protocol.MessageBodyAppReply {
		return "", 0, fmt.Errorf("unexpected appinfo reply type: %v", msg.DataType())
	}
	var tbl flatbuffers.Table
	if !msg.Data(&tbl) {
		return "", 0, fmt.Errorf("appinfo reply missing body")
	}
	var ar protocol.AppReply
	ar.Init(tbl.Bytes, tbl.Pos)
	status := ar.Status()
	if status != protocol.AppStatusConnected && status != protocol.AppStatusStillValid {
		return "", status, nil
	}
	var r struct {
		ID *newrelic.AgentRunID `json:"agent_run_id"`
	}
	if err := json.Unmarshal(ar.ConnectReply(), &r); err != nil || r.ID == nil {
		return "", status, fmt.Errorf("parse agent_run_id: %w", err)
	}
	return *r.ID, status, nil
}

// readMessage reads one framed daemon message (8-byte LE header + body).
func readMessage(r io.Reader) ([]byte, error) {
	header := make([]byte, 8)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	length := binary.LittleEndian.Uint32(header[0:4])
	if length > 4<<20 {
		return nil, fmt.Errorf("reply too large: %d", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

func networkFor(addr string) string {
	if len(addr) > 0 && addr[0] == '/' {
		return "unix"
	}
	if len(addr) > 0 && addr[0] == '@' {
		return "unix" // abstract socket (Linux)
	}
	return "tcp"
}