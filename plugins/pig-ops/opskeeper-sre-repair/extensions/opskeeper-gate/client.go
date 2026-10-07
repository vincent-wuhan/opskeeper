package opskeepergate

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/plugins/pig-ops/opskeeper-sre-repair/extensions/opskeeper-gate/wire"
)

// dialTimeout bounds establishing the connection.
//
// Short, because this is a local unix socket on the same host: if it is
// not there, it is not there, and a tool call must not sit through a
// connect timeout to learn what a stat would have said.
const dialTimeout = 5 * time.Second

// writeTimeout bounds sending one request.
//
// Also short. A verdict that takes longer than this to start arriving is
// the host being stuck, and the call is refused either way — the difference
// is whether the model gets its answer now or after another half minute of
// waiting for one that is not coming.
const writeTimeout = 10 * time.Second

// maxLine bounds one response line. The host answers with two fields; a
// megabyte is a runaway connection, not a verdict.
const maxLine = 1 << 20

// client speaks the gate protocol to the host over a unix socket.
//
// One connection is held and reused, and it is reopened on the first
// failure after a break. Reconnecting eagerly costs a handshake per call;
// never reconnecting means one dropped edge deafens the node until it is
// restarted, which is exactly when a node needs its agent most.
type client struct {
	path string

	mu   sync.Mutex
	conn net.Conn
	rd   *bufio.Reader
	// broken is set when the connection failed, so the next call reopens
	// rather than writing into a socket nobody is reading.
	broken bool
}

// ask asks the host about one call.
//
// Every failure is an error, and every error is a refusal at the call site.
// There is no path here that returns a permitted verdict the host did not
// send, and no path that returns a zero value that would read as one.
func (c *client) ask(req wire.GateRequest) (wire.GateVerdict, error) {
	if c.path == "" {
		return wire.GateVerdict{}, errors.New(
			"this node's agent was started without a gate socket (" + wire.GateSocketEnv + " is unset)")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// One reconnect per call, not a retry loop. A host that is genuinely
	// down must surface as a refusal promptly; a loop here would turn one
	// missing host into a tool call that hangs the model for a minute.
	if err := c.ensure(); err != nil {
		return wire.GateVerdict{}, err
	}
	verdict, err := c.roundTrip(req)
	if err != nil {
		// The connection is presumed bad rather than known bad: the host
		// may have closed it between calls, which is normal, or mid-write,
		// which is not. Either way the next call starts from scratch.
		c.drop()
		return wire.GateVerdict{}, err
	}
	return verdict, nil
}

// ensure opens the connection if there is not a live one.
func (c *client) ensure() error {
	if c.conn != nil && !c.broken {
		return nil
	}
	//nolint:gosec // G704: a local unix socket named by the host, not arbitrary input.
	conn, err := net.DialTimeout("unix", c.path, dialTimeout)
	if err != nil {
		return fmt.Errorf("connect to the node's gate socket: %w", err)
	}
	c.conn = conn
	c.rd = bufio.NewReaderSize(conn, 64*1024)
	c.broken = false
	return nil
}

// drop closes and forgets the connection.
func (c *client) drop() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn = nil
	c.rd = nil
	c.broken = true
}

// roundTrip sends one request and reads one verdict.
func (c *client) roundTrip(req wire.GateRequest) (wire.GateVerdict, error) {
	body, err := json.Marshal(req)
	if err != nil {
		// The arguments came from the model as decoded JSON, so this
		// should not be reachable. Refusing the call is still the right
		// answer to a request that cannot be stated.
		return wire.GateVerdict{}, fmt.Errorf("this tool call's arguments could not be sent: %w", err)
	}

	if err := c.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return wire.GateVerdict{}, err
	}
	if _, err := c.conn.Write(append(body, '\n')); err != nil {
		return wire.GateVerdict{}, fmt.Errorf("ask the host: %w", err)
	}

	if err := c.conn.SetReadDeadline(time.Now().Add(writeTimeout)); err != nil {
		return wire.GateVerdict{}, err
	}
	line, err := readLine(c.rd)
	if err != nil {
		return wire.GateVerdict{}, fmt.Errorf("the host did not answer: %w", err)
	}

	var verdict wire.GateVerdict
	if err := json.Unmarshal(line, &verdict); err != nil {
		return wire.GateVerdict{}, fmt.Errorf("the host's answer could not be read: %w", err)
	}
	switch verdict.Outcome {
	case wire.GateAllow, wire.GateBlock, wire.GateDeny:
		return verdict, nil
	default:
		// An outcome nobody defined is not a permission. Reading it as one
		// would be a fail-open with a plausible-looking payload, which is
		// the worst kind.
		return wire.GateVerdict{}, fmt.Errorf("the host answered with an outcome this agent does not understand (%q)", verdict.Outcome)
	}
}

// readLine reads one newline-terminated line, bounded by maxLine.
func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		if len(buf)+len(chunk) > maxLine {
			return nil, fmt.Errorf("the host's answer exceeded %d bytes", maxLine)
		}
		buf = append(buf, chunk...)
		if !isPrefix {
			return buf, nil
		}
	}
}
