// rabbit.go is the RabbitMQ backend, spoken over the management HTTP API.
//
// Why the management API rather than AMQP. Everything this adapter does to a
// queue — how deep it is, who is consuming it and how fast, purge it, take N
// messages off it and put them back on an exchange — is an administrative
// question, and RabbitMQ answers exactly those questions over HTTP with the
// same credentials as AMQP. Doing the same over AMQP would mean opening a
// channel, passive-declaring, and then consuming with manual acks to measure
// depth — a measurement that changes what it measures, because a consumer
// that connects and acks is a consumer, and a queue with a consumer reports a
// different utilisation than the one an operator was looking at.
//
// What this backend will not do: silently degrade. When the management plugin
// is not enabled the API answers 404 and every operation here fails with that
// fact, rather than reporting an empty queue list — "no queues" and "I cannot
// see any queues" are the two answers an incident reader must be able to tell
// apart.
package mq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMgmtPort = 15672
	maxReplayBatch  = 1000
	maxBodyBytes    = 8 << 20
)

// rabbitClient talks to one RabbitMQ management endpoint.
type rabbitClient struct {
	base   string // http://host:15672
	vhost  string // "/" or "prod"
	user   string
	pass   string
	http   *http.Client
	vhosts bool // whether the DSN named a vhost explicitly
}

// newRabbitClient derives the management endpoint from an amqp DSN.
//
//	amqp://user:pass@host:5672/prod
//	 └── http://host:15672, vhost "/prod"
//
// The vhost is percent-decoded because RabbitMQ's own default vhost is the
// single character "/", which a URL always carries as "%2F". Reading it
// without decoding produces a vhost named "%2F" and a 404 that says the vhost
// does not exist.
func newRabbitClient(dsn string, timeout time.Duration) (*rabbitClient, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("mq: parse amqp DSN: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("mq: amqp DSN has no host")
	}
	q := u.Query()
	scheme := "http"
	if strings.EqualFold(u.Scheme, "amqps") {
		scheme = "https"
	}
	if s := q.Get("mgmt_scheme"); s != "" {
		scheme = strings.ToLower(s)
	}
	port := defaultMgmtPort
	if s := q.Get("mgmt_port"); s != "" {
		p, err := strconv.Atoi(s)
		if err != nil || p <= 0 || p > 65535 {
			return nil, fmt.Errorf("mq: mgmt_port %q is not a port number", s)
		}
		port = p
	}
	host := u.Hostname()
	base := fmt.Sprintf("%s://%s", scheme, host)
	if port != 80 && port != 443 {
		base = fmt.Sprintf("%s://%s:%d", scheme, host, port)
	}

	vhost := ""
	rawPath := strings.TrimPrefix(u.EscapedPath(), "/")
	if rawPath != "" {
		decoded, err := url.PathUnescape(rawPath)
		if err != nil {
			return nil, fmt.Errorf("mq: decode vhost %q: %w", rawPath, err)
		}
		vhost = decoded
	}

	user, pass := "", ""
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	if user == "" {
		// The management API requires a credential; an anonymous request is
		// answered with 401 and no explanation of why.
		return nil, fmt.Errorf("mq: amqp DSN carries no user; the management API needs one")
	}
	return &rabbitClient{
		base:   base,
		vhost:  vhost,
		user:   user,
		pass:   pass,
		http:   &http.Client{Timeout: timeout},
		vhosts: rawPath != "",
	}, nil
}

// defaultVhost resolves which vhost an operation applies to.
//
// The DSN's vhost wins when it named one; otherwise the argument does; and if
// neither did, the operation is refused. RabbitMQ's own default is "/", but
// defaulting to it would silently act on a differently-named vhost's queue
// than the caller meant — the two look identical from the response.
func (c *rabbitClient) resolveVhost(p params) (string, error) {
	if c.vhosts && c.vhost != "" {
		return c.vhost, nil
	}
	v, err := p.optionalString("vhost")
	if err != nil {
		return "", err
	}
	if v == "" {
		if c.vhost != "" {
			return c.vhost, nil
		}
		return "", fmt.Errorf("mq: vhost is required: this DSN names no vhost, and defaulting to %q would act on a queue the caller may not have meant", "/")
	}
	return v, nil
}

func (c *rabbitClient) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("mq: encode request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return fmt.Errorf("mq: build %s %s: %w", method, path, err)
	}
	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("mq: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("mq: read %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("mq: %s %s: 404 (is the rabbitmq_management plugin enabled, and does the queue exist?)", method, path)
		}
		return fmt.Errorf("mq: %s %s: %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("mq: decode %s %s: %w", method, path, err)
	}
	return nil
}

// probe verifies the management endpoint answers.
func (c *rabbitClient) probe(ctx context.Context) error {
	var overview map[string]any
	return c.do(ctx, http.MethodGet, "/api/overview", nil, &overview)
}

func (c *rabbitClient) health(ctx context.Context) (string, error) {
	var overview map[string]any
	if err := c.do(ctx, http.MethodGet, "/api/overview", nil, &overview); err != nil {
		return "", err
	}
	return fmt.Sprintf("RabbitMQ %s, %d node(s)", asString(overview, "rabbitmq_version"), len(asSlice(overview, "listeners"))), nil
}

func asSlice(m map[string]any, key string) []any {
	if m == nil {
		return nil
	}
	s, _ := m[key].([]any)
	return s
}

func asMap(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	v, _ := m[key].(map[string]any)
	return v
}

// overview returns the broker-level figures Collect reports.
func (c *rabbitClient) overview(ctx context.Context) (map[string]interface{}, error) {
	var doc map[string]any
	if err := c.do(ctx, http.MethodGet, "/api/overview", nil, &doc); err != nil {
		return nil, err
	}
	totals := asMap(doc, "queue_totals")
	objects := asMap(doc, "object_totals")
	metrics := map[string]interface{}{
		"rabbitmq_version":        asString(doc, "rabbitmq_version"),
		"cluster_name":            asString(doc, "cluster_name"),
		"messages":                asInt(totals, "messages"),
		"messages_ready":          asInt(totals, "messages_ready"),
		"messages_unacknowledged": asInt(totals, "messages_unacknowledged"),
		"queues":                  asInt(objects, "queues"),
		"consumers":               asInt(objects, "consumers"),
		"connections":             asInt(objects, "connections"),
		"channels":                asInt(objects, "channels"),
	}
	if rate := asMap(asMap(doc, "message_stats"), "ack_details"); rate != nil {
		metrics["ack_rate"] = asFloat(rate, "rate")
	}
	return metrics, nil
}

// queueRows lists queues, optionally filtered to those with a backlog.
func (c *rabbitClient) queueRows(ctx context.Context, p params, limit, minMessages int) ([]map[string]any, string, error) {
	if limit <= 0 {
		limit = 200
	}
	prefix, err := p.optionalString("prefix")
	if err != nil {
		return nil, "", err
	}
	var doc []map[string]any
	if err := c.do(ctx, http.MethodGet, "/api/queues", nil, &doc); err != nil {
		return nil, "", err
	}
	rows := make([]map[string]any, 0, len(doc))
	for _, q := range doc {
		if prefix != "" && !strings.HasPrefix(asString(q, "name"), prefix) {
			continue
		}
		messages := asInt(q, "messages")
		if messages < minMessages {
			continue
		}
		rows = append(rows, map[string]any{
			"name":                    asString(q, "name"),
			"vhost":                   asString(q, "vhost"),
			"type":                    asString(q, "type"),
			"state":                   asString(q, "state"),
			"messages":                messages,
			"messages_ready":          asInt(q, "messages_ready"),
			"messages_unacknowledged": asInt(q, "messages_unacknowledged"),
			"consumers":               asInt(q, "consumers"),
			"consumer_utilisation":    asFloat(q, "consumer_utilisation"),
			"node":                    asString(q, "node"),
			"memory_bytes":            asInt(q, "memory"),
		})
	}
	// Deepest first: the queue an incident is about is the one holding the
	// backlog, and the API returns them in declaration order.
	sort.Slice(rows, func(i, j int) bool {
		return rows[i]["messages"].(int) > rows[j]["messages"].(int)
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	summary := fmt.Sprintf("%d queue(s)", len(rows))
	if minMessages > 0 {
		summary = fmt.Sprintf("%d queue(s) with at least one message", len(rows))
	}
	return rows, summary, nil
}

func (c *rabbitClient) nodeRows(ctx context.Context) ([]map[string]any, string, error) {
	var doc []map[string]any
	if err := c.do(ctx, http.MethodGet, "/api/nodes", nil, &doc); err != nil {
		return nil, "", err
	}
	rows := make([]map[string]any, 0, len(doc))
	for _, n := range doc {
		rows = append(rows, map[string]any{
			"name":      asString(n, "name"),
			"type":      asString(n, "type"),
			"running":   n["running"] == true,
			"mem_used":  asInt(n, "mem_used"),
			"mem_limit": asInt(n, "mem_limit"),
			"disk_free": asInt(n, "disk_free"),
			"fd_used":   asInt(n, "fd_used"),
			"proc_used": asInt(n, "proc_used"),
			"uptime_ms": asInt(n, "uptime"),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["name"].(string) < rows[j]["name"].(string) })
	return rows, fmt.Sprintf("%d node(s)", len(rows)), nil
}

// queueURL builds the path for one queue.
func queueURL(vhost, name string) string {
	return "/api/queues/" + url.PathEscape(vhost) + "/" + url.PathEscape(name)
}

// drainQueue purges a queue's ready messages.
//
// `confirm: "drain"` is required. Redis's flushdb in this codebase asks for
// the same kind of confirmation, and for the same reason: this call deletes
// messages that no backup contains, and a tool that can be invoked with a
// name and nothing else is a tool a model will invoke by accident. The
// confirmation is a string rather than a boolean so that it cannot arrive
// from a coerced "true" anywhere along the JSON path.
func (c *rabbitClient) drainQueue(ctx context.Context, p params) (int, string, bool, error) {
	queue, err := p.requireString("queue")
	if err != nil {
		return 0, "", false, err
	}
	confirm, err := p.optionalString("confirm")
	if err != nil {
		return 0, "", false, err
	}
	if confirm != "drain" {
		return 0, "", false, fmt.Errorf("mq: purge of queue %q was not confirmed: pass confirm=\"drain\" to acknowledge that the messages are unrecoverable", queue)
	}
	vhost, err := c.resolveVhost(p)
	if err != nil {
		return 0, "", false, err
	}
	var before map[string]any
	if err := c.do(ctx, http.MethodGet, queueURL(vhost, queue), nil, &before); err != nil {
		return 0, "", false, err
	}
	ready := asInt(before, "messages_ready")
	unacked := asInt(before, "messages_unacknowledged")
	if err := c.do(ctx, http.MethodDelete, queueURL(vhost, queue)+"/contents", nil, nil); err != nil {
		return 0, "", false, err
	}
	message := fmt.Sprintf("purged %d ready message(s) from queue %s (vhost %s)", ready, queue, vhost)
	if unacked > 0 {
		// Unacknowledged messages survive a purge, and saying so is the
		// difference between an operator who knows the queue is now empty
		// and one who finds out when the consumers NACK.
		message += fmt.Sprintf("; %d unacknowledged message(s) remain with their consumers and will be requeued if those consumers disconnect", unacked)
	}
	return ready, message, true, nil
}

// replayMessages moves messages off a queue and publishes them again.
//
// The two halves are what RabbitMQ's own shovel does, written out: take a
// bounded batch off the queue with `ack_requeue_false` (which removes them),
// then publish each one to the exchange. A publish that fails is counted, and
// the operation reports failure — the messages are gone from the queue at
// that point, and reporting success would leave an operator believing they
// had arrived somewhere.
func (c *rabbitClient) replayMessages(ctx context.Context, p params) (int, string, bool, error) {
	queue, err := p.requireString("queue")
	if err != nil {
		return 0, "", false, err
	}
	exchange, err := p.requireString("exchange")
	if err != nil {
		return 0, "", false, err
	}
	routingKey, err := p.requireString("routing_key")
	if err != nil {
		return 0, "", false, err
	}
	vhost, err := c.resolveVhost(p)
	if err != nil {
		return 0, "", false, err
	}
	limit, err := intArg(p, "limit", 100, maxReplayBatch)
	if err != nil {
		return 0, "", false, err
	}
	requeue, err := p.optionalBool("requeue_failed", true)
	if err != nil {
		return 0, "", false, err
	}

	var got []map[string]any
	getBody := map[string]any{
		"count":    limit,
		"ackmode":  "ack_requeue_false",
		"encoding": "auto",
	}
	if err := c.do(ctx, http.MethodPost, queueURL(vhost, queue)+"/get", getBody, &got); err != nil {
		return 0, "", false, err
	}
	if len(got) == 0 {
		return 0, fmt.Sprintf("queue %s is empty; nothing to replay", queue), true, nil
	}

	published, failed := 0, 0
	var firstErr string
	for _, msg := range got {
		payload := asString(msg, "payload")
		payloadEncoding := asString(msg, "payload_encoding")
		if payloadEncoding == "" {
			payloadEncoding = "string"
		}
		body := map[string]any{
			"properties":       msg["properties"],
			"routing_key":      routingKey,
			"payload":          payload,
			"payload_encoding": payloadEncoding,
		}
		var out map[string]any
		if err := c.do(ctx, http.MethodPost, "/api/exchanges/"+url.PathEscape(vhost)+"/"+url.PathEscape(exchange)+"/publish", body, &out); err != nil {
			failed++
			if firstErr == "" {
				firstErr = err.Error()
			}
			continue
		}
		if out["routed"] == false {
			// `routed: false` is the exchange accepting the message and
			// matching no binding. The message is dropped, which is a
			// different outcome from a publish error and has a different
			// fix (a binding), so it is counted as a failure with its own
			// reason.
			failed++
			if firstErr == "" {
				firstErr = "the exchange matched no binding for routing key " + routingKey + ", so the message was dropped"
			}
			continue
		}
		published++
	}
	if failed > 0 {
		message := fmt.Sprintf("replayed %d of %d message(s) from %s to exchange %s; %d failed: %s",
			published, len(got), queue, exchange, failed, firstErr)
		if requeue {
			message += "; messages already taken off the queue are not restored — re-publish them from the exchange's dead-letter queue if that is where they went"
		}
		return published, message, false, nil
	}
	return published, fmt.Sprintf("replayed %d message(s) from queue %s to exchange %s with routing key %s",
		published, queue, exchange, routingKey), true, nil
}
