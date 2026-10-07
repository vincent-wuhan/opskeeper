package opskeepersre

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// dialTimeout bounds establishing the connection.
//
// Short, because this is a local unix socket on the same host: if it is
// not there, it is not there, and a tool call must not sit through a
// connect timeout to learn what a stat would have said.
const dialTimeout = 5 * time.Second

// writeTimeout bounds sending one request.
//
// Short, and unlike the gate's write timeout this one is not about
// refusing a hung call — a tool that legitimately takes minutes is
// allowed to. It bounds only the time to get the request in, because a
// broker that is not reading is a broker that is gone.
const writeTimeout = 10 * time.Second

// maxLine bounds one reply line.
//
// Generous compared to the gate's: a tool reply is the tool's whole
// output, and host_grep_file legitimately returns a page of matches. It is
// still a bound, because an unbounded reader on a socket a model can
// influence is an unbounded allocation.
const maxLine = 8 << 20

// client speaks the tool-broker protocol to the host over a unix socket.
//
// One connection is held and reused across a turn, and reopened on the
// first failure after a break. Reconnecting eagerly costs a handshake per
// call; never reconnecting means one dropped edge deafens the node until
// it is restarted, which is exactly when a node needs its agent most.
type client struct {
	path string

	mu   sync.Mutex
	conn net.Conn
	rd   *bufio.Reader
	// broken is set when the connection failed, so the next call reopens
	// rather than writing into a socket nobody is reading.
	broken bool
}

// run asks the host to run one tool.
//
// Every failure is an error, and the call site turns every error into text
// the model reads rather than a retry. There is no path here that returns
// a result the host did not send.
func (c *client) run(req wire.ToolRequest) (wire.ToolReply, error) {
	if c.path == "" {
		return wire.ToolReply{}, errors.New(
			"this node's agent was started without a tool socket (" + wire.ToolSocketEnv + " is unset)")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return wire.ToolReply{}, fmt.Errorf("encoding the tool call: %w", err)
	}

	reply, err := c.attempt(req, body)
	if err == nil {
		return reply, nil
	}
	// A failure to send means the request never left, so sending it again
	// on a fresh connection runs nothing twice. A failure to read does
	// not mean that, which is why the two are separated here rather than
	// folded into one retry.
	var unsent *unsentError
	if errors.As(err, &unsent) {
		if again, retryErr := c.attempt(req, body); retryErr == nil {
			return again, nil
		}
	}
	return wire.ToolReply{}, err
}

// attempt sends one request on the current connection and reads its reply.
func (c *client) attempt(req wire.ToolRequest, body []byte) (wire.ToolReply, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.connect(); err != nil {
		return wire.ToolReply{}, err
	}

	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := c.conn.Write(append(body, '\n')); err != nil {
		c.reset()
		return wire.ToolReply{}, &unsentError{fmt.Errorf("sending the tool call: %w", err)}
	}

	// The read deadline is the host's own ceiling plus room for the reply
	// to arrive. The host abandons a call that overruns and says so, so
	// this is a backstop against a host that stopped answering at all.
	_ = c.conn.SetReadDeadline(time.Now().Add(hostCallCeiling))
	line, err := c.rd.ReadBytes('\n')
	if err != nil {
		c.reset()
		// The request was delivered. Whether the host ran it is now
		// unknowable from here, and a blind resend would be a second
		// execution of a call whose first execution may have succeeded.
		// For a read that is merely wasteful; for a write — a restart, an
		// applied config change — it is a second action taken on a live
		// system, which is the failure this broker exists to make
		// impossible. The honest answer is that the outcome is unknown,
		// and the model is told so rather than sent round again.
		return wire.ToolReply{}, fmt.Errorf(
			"the host stopped answering after %s was sent, so whether it ran is unknown: %w", req.ToolName, err)
	}
	if len(line) > maxLine {
		c.reset()
		return wire.ToolReply{}, fmt.Errorf("the host sent more than %d bytes for one tool call", maxLine)
	}

	var reply wire.ToolReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return wire.ToolReply{}, fmt.Errorf("the host sent a result this agent could not read: %w", err)
	}
	return reply, nil
}

// unsentError marks a failure that happened before the request reached the
// host, which is the only failure safe to retry.
type unsentError struct{ err error }

func (e *unsentError) Error() string { return e.err.Error() }
func (e *unsentError) Unwrap() error { return e.err }

// hostCallCeiling mirrors the host's own tool-call ceiling plus slack.
//
// The two have to agree or the agent gives up on a call the host is still
// legitimately running. It is a mirror, not a shared constant, because
// this module is built and shipped by the agent runtime and must not
// depend on the host's internals to know how long the host will wait.
const hostCallCeiling = 6 * time.Minute

// connect returns a usable connection, reopening a broken one once.
func (c *client) connect() error {
	if c.conn != nil && !c.broken {
		return nil
	}
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn, c.rd, c.broken = nil, nil, false
	}
	conn, err := net.DialTimeout("unix", c.path, dialTimeout)
	if err != nil {
		return fmt.Errorf("reaching the OpsKeeper host on this node: %w", err)
	}
	c.conn = conn
	c.rd = bufio.NewReaderSize(conn, 64*1024)
	return nil
}

// reset drops a connection that failed, so the next call redials.
func (c *client) reset() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn, c.rd, c.broken = nil, nil, true
}

// close releases the connection.
func (c *client) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn, c.rd, c.broken = nil, nil, false
}
