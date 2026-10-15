// tools.go implements the broker-neutral read-only tools.
//
// Each function asks the connected backend the same question in that
// backend's own terms. The mapping is stated here once rather than being
// pushed up into the tool layer, because "consumer lag" means a specific
// computation on each broker — a committed offset against the end of a log on
// Kafka, a consumer's acknowledgement rate against a queue's depth on
// RabbitMQ — and a tool layer that averaged the two would report a number
// nobody could act on.
package mq

import (
	"context"
	"errors"
	"fmt"
)

// runQueueList lists queues (RabbitMQ) or topics (Kafka).
func runQueueList(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	rabbit, kf, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	p := params(args)
	limit, err := intArg(args, "limit", 200, 5000)
	if err != nil {
		return nil, "", err
	}
	if rabbit != nil {
		return rabbit.queueRows(ctx, p, limit, 0)
	}
	return kf.topicRows(ctx, limit)
}

// runRebalanceHistory reports how a consumer group's membership changed.
//
// Kafka is the only backend with an answer here, and the refusal for the
// other one names itself rather than answering a question about a broker
// that cannot answer it. RabbitMQ's queue membership changes are visible in
// the queue rows themselves, and there is no per-group assignment to keep a
// history of.
func runRebalanceHistory(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	_, kf, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	if kf == nil {
		return nil, "", errors.New("mq: rebalance history is a Kafka capability; this connection is RabbitMQ, whose queue membership is reported by inspect_queue")
	}
	limit, err := intArg(args, "limit", 50, 500)
	if err != nil {
		return nil, "", err
	}
	return kf.rebalanceHistory(ctx, params(args), limit)
}

// runInspectLag reports what is behind, and how far.
func runInspectLag(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	rabbit, kf, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	p := params(args)
	limit, err := intArg(args, "limit", 200, 5000)
	if err != nil {
		return nil, "", err
	}
	if kf != nil {
		return kf.lagRows(ctx, p, limit)
	}
	// RabbitMQ reports depth per queue rather than a lag figure, because a
	// queue is a push model: the backlog *is* the measure of how far behind
	// its consumers are.
	rows, summary, err := rabbit.queueRows(ctx, p, limit, 1)
	if err != nil {
		return nil, "", err
	}
	stalled := 0
	for _, row := range rows {
		if asInt(row, "consumers") == 0 && asInt(row, "messages_ready") > 0 {
			// A queue with a backlog and no consumers is the RabbitMQ
			// equivalent of an abandoned consumer group, and it is the one
			// case where "the backlog is growing" and "nobody is reading"
			// are the same finding.
			row["stalled"] = true
			stalled++
		}
	}
	if stalled > 0 {
		summary = fmt.Sprintf("%s; %d with no consumers at all", summary, stalled)
	}
	return rows, summary, nil
}

// runBrokerStatus reports the broker nodes and where the leaders are.
func runBrokerStatus(ctx context.Context, a *Adapter, _ map[string]any) ([]map[string]any, string, error) {
	rabbit, kf, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	if kf != nil {
		return kf.brokerRows(ctx)
	}
	return rabbit.nodeRows(ctx)
}
