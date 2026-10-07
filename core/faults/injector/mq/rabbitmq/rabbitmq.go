// Package rabbitmq is the RabbitMQ fault injector.
//
// Decision 300 made Kafka real; this is its sibling, and the sibling is
// instructive because the two brokers have the *same* shape of problem and
// answer it differently:
//
//   - In Kafka the fault lives in a **log** and records cannot be removed.
//   - In RabbitMQ the fault lives in a **queue**, and a queue *can* be removed
//     in one call — but only if it is yours to remove.
//
// That second clause is the whole design. Undoing a burst means
// `QueueDelete`, and `QueueDelete` on a queue that was already there takes
// the operator's real backlog with it. So the fault is staged in a queue
// this injector created, and the queue name in the case file is a **label**,
// exactly as `key_prefix` is in the redis injector (decision 298) and as
// `topic` is in the kafka one (decision 300).
//
// A second thing is worth saying out loud because it cost a full round in
// Kafka: **publishing is not acknowledgement**. `Channel.Publish` without
// publisher confirms returns nil the moment the frame is written to the
// socket. A burst staged on that basis is a burst whose size nobody has
// measured, and the queue depth a minute later is whatever the broker felt
// like accepting. So every publish here is confirmed, and the fault's size
// is read back off the queue with an independent connection.
//
// The client is rabbitmq/amqp091-go: pure Go, no cgo, and its `Close()` is
// the same "close, don't kill and hope" that decided kafka-go over
// confluent-kafka-go.
package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

// URLEnv is the AMQP URL this injector connects with
// (amqp://user:pass@host:port/vhost).
const URLEnv = "OPSKEEPER_HARNESS_RABBITMQ_URL"

const connectTimeout = 10 * time.Second

// supportedTypes is every injection type this injector knows.
var supportedTypes = map[string]bool{
	"rabbitmq.inject_message_burst": true,
}

// SupportedTypes returns every known type, sorted.
func SupportedTypes() []string {
	out := make([]string, 0, len(supportedTypes))
	for typ := range supportedTypes {
		out = append(out, typ)
	}
	sort.Strings(out)
	return out
}

// live is one injection in progress.
type live struct {
	id       string
	typ      string
	started  time.Time
	rollback []func(context.Context) error
	timer    *time.Timer
	duration time.Duration
	// queue is the queue this injection actually touched.
	queue string
	// queueCreated records whether we made it — only ours may be deleted.
	queueCreated bool
	// depth is the measured backlog, carried into the result so a human can
	// reconcile it.
	depth  int
	bytes  int64
	ctx    context.Context
	cancel context.CancelFunc
	err    error
}

// Injector is the RabbitMQ fault injector.
type Injector struct {
	url    string
	urlSet bool
	mu     sync.Mutex
	seq    int
	live   map[string]*live
}

// Option configures an Injector.
type Option func(*Injector)

// WithURL names the AMQP URL.
func WithURL(url string) Option {
	return func(i *Injector) { i.url = url; i.urlSet = true }
}

// New builds a RabbitMQ injector.
func New(opts ...Option) *Injector {
	i := &Injector{live: map[string]*live{}}
	for _, opt := range opts {
		opt(i)
	}
	if !i.urlSet {
		i.url = os.Getenv(URLEnv)
	}
	return i
}

// Type returns the type prefix.
func (i *Injector) Type() string { return "rabbitmq." }

// CheckAvailable reports whether this injector can actually stage a fault.
func (i *Injector) CheckAvailable(ctx context.Context) error {
	if i.url == "" {
		return fmt.Errorf("%w: 没有配置 AMQP 连接（设 %s，或用 WithURL 传进来）",
			injector.ErrUnavailable, URLEnv)
	}
	conn, err := amqp.DialConfig(i.url, amqp.Config{
		Dial:      amqp.DefaultDial(connectTimeout),
		Heartbeat: 10 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("%w: 连不上 %s: %w", injector.ErrUnavailable, redact(i.url), err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("%w: %s 连上了但开不出 channel: %w",
			injector.ErrUnavailable, redact(i.url), err)
	}
	return ch.Close()
}

// Inject stages one fault.
func (i *Injector) Inject(ctx context.Context, spec injector.InjectSpec) (*injector.InjectResult, error) {
	if !supportedTypes[spec.Type] {
		return nil, fmt.Errorf("%w: %s", injector.ErrUnsupportedType, spec.Type)
	}
	if err := i.CheckAvailable(ctx); err != nil {
		return nil, err
	}

	l := i.begin(spec)
	// A refused injection must leave no trace. Without this, Cleanup later
	// reports a fault that never happened as "rolled back", and the count of
	// live faults only ever goes up.
	defer func() {
		if l != nil && l.err != nil {
			_ = i.runRollback(context.Background(), l)
			i.forget(l.id)
		}
	}()

	var err error
	switch spec.Type {
	case "rabbitmq.inject_message_burst":
		err = i.injectMessageBurst(ctx, spec, l)
	default:
		err = fmt.Errorf("%w: %s", injector.ErrUnsupportedType, spec.Type)
	}
	if err != nil {
		l.err = err
		return nil, err
	}
	l.err = nil
	i.startExpiry(l, spec.Duration)
	return i.result(l), nil
}

func (i *Injector) begin(spec injector.InjectSpec) *live {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.seq++
	id := spec.InjectID
	if id == "" {
		id = fmt.Sprintf("rabbitmq-inj-%d-%d", time.Now().UTC().UnixNano(), i.seq)
	}
	lctx, lcancel := context.WithCancel(context.Background())
	l := &live{
		id:       id,
		typ:      spec.Type,
		started:  time.Now().UTC(),
		duration: spec.Duration,
		ctx:      lctx,
		cancel:   lcancel,
	}
	i.live[id] = l
	return l
}

func (i *Injector) startExpiry(l *live, d time.Duration) {
	if d <= 0 {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	l.timer = time.AfterFunc(d, func() {
		_ = i.Cleanup(context.Background(), l.id)
	})
}

// forget drops an entry from the live ledger without running any undo.
func (i *Injector) forget(id string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.live, id)
}

// Live returns the IDs currently in effect.
func (i *Injector) Live() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]string, 0, len(i.live))
	for id := range i.live {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (i *Injector) result(l *live) *injector.InjectResult {
	meta := map[string]string{"client": "amqp091-go"}
	if l.queue != "" {
		meta["queue"] = l.queue
		meta["queue_created_by_injector"] = fmt.Sprintf("%t", l.queueCreated)
	}
	if l.depth > 0 {
		meta["queue_depth"] = fmt.Sprintf("%d", l.depth)
		meta["queue_bytes"] = fmt.Sprintf("%d", l.bytes)
	}
	if l.duration > 0 {
		meta["expires"] = l.started.Add(l.duration).Format(time.RFC3339)
	}
	return &injector.InjectResult{
		InjectID:   l.id,
		Type:       l.typ,
		ResourceID: l.queue,
		StartedAt:  l.started,
		Metadata:   meta,
	}
}

// Cleanup undoes one injection. It is idempotent.
func (i *Injector) Cleanup(ctx context.Context, injectID string) error {
	if injectID == "" {
		return injector.ErrInjectionNotFound
	}
	i.mu.Lock()
	l, ok := i.live[injectID]
	if ok {
		delete(i.live, injectID)
	}
	i.mu.Unlock()
	if !ok {
		return injector.ErrInjectionNotFound
	}
	if l.timer != nil {
		l.timer.Stop()
	}
	if l.cancel != nil {
		l.cancel()
	}
	return i.runRollback(ctx, l)
}

// runRollback runs the undo steps in reverse order.
func (i *Injector) runRollback(ctx context.Context, l *live) error {
	var errs []error
	for n := len(l.rollback) - 1; n >= 0; n-- {
		if err := l.rollback[n](ctx); err != nil {
			errs = append(errs, err)
		}
	}
	l.rollback = nil
	return errors.Join(errs...)
}

// connect opens one connection and closes it when f returns.
func (i *Injector) connect(fn func(*amqp.Connection, *amqp.Channel) error) error {
	conn, err := amqp.DialConfig(i.url, amqp.Config{
		Dial:      amqp.DefaultDial(connectTimeout),
		Heartbeat: 10 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("connect to %s: %w", redact(i.url), err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("open channel: %w", err)
	}
	if err := fn(conn, ch); err != nil {
		_ = ch.Close()
		return err
	}
	return ch.Close()
}

// redact strips the password out of an AMQP URL before it reaches an error
// message or a log line.
func redact(url string) string {
	at := strings.LastIndex(url, "@")
	scheme := strings.Index(url, "://")
	if at < 0 || scheme < 0 || at < scheme {
		return url
	}
	return url[:scheme+3] + "***@" + url[at+1:]
}

var _ injector.Injector = (*Injector)(nil)
