package frontierbound

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/vincent-wuhan/opskeeper/core/floor/prom"
)

// deploy/install/grafana/provisioning/dashboards/json/manager-internals.json
// ships a panel reading
//
//	opskeeper_edge_connections{status="connected"}
//
// and for the life of the deployment nothing wrote to that gauge. The panel
// showed a flat zero next to a live latency series, and an operator reading
// that pair concludes "no nodes are connected" -- or "the metric is
// broken" -- and neither conclusion is the truth, which is that the number
// was never produced.
//
// The count is derived from the transport bindings the manager already
// keeps, because those bindings *are* the manager's answer to which brokers
// have proved an edge identity. A separate counter could drift from the
// routing table; this one cannot.

func gaugeValues(t *testing.T, registry *prometheus.Registry) map[string]float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	out := map[string]float64{}
	for _, family := range families {
		if family.GetName() != "opskeeper_edge_connections" {
			continue
		}
		for _, metric := range family.Metric {
			for _, pair := range metric.GetLabel() {
				if pair.GetName() == "status" {
					out[pair.GetValue()] = metric.GetGauge().GetValue()
				}
			}
		}
	}
	return out
}

func TestTheConnectionGaugeFollowsTheEdges(t *testing.T) {
	registry := prometheus.NewRegistry()
	prom.RegisterManagerMetrics(registry, nil)
	client := newWithService(nil, nil)

	client.bindEdgeTransport(1, 100)
	client.bindEdgeTransport(2, 200)
	got := gaugeValues(t, registry)
	if got["connected"] != 2 {
		t.Fatalf("connected = %v, want 2: the panel is asking how many nodes the manager can reach", got["connected"])
	}

	client.unbindTransport(1)
	got = gaugeValues(t, registry)
	if got["connected"] != 1 || got["disconnected"] != 1 {
		t.Fatalf("after one disconnect: %v, want connected 1 / disconnected 1", got)
	}
}

// A flapping edge occupies one slot in each series rather than inflating
// the disconnected count on every reconnect. The alternative is a counter,
// and a counter on a metric whose other series is a gauge is a query that
// looks like a rate and is not one.
func TestAReconnectingEdgeIsCountedOnceNotOncePerAttempt(t *testing.T) {
	registry := prometheus.NewRegistry()
	prom.RegisterManagerMetrics(registry, nil)
	client := newWithService(nil, nil)

	for i := 0; i < 3; i++ {
		client.bindEdgeTransport(7, 700)
		client.unbindTransport(7)
	}
	got := gaugeValues(t, registry)
	if got["connected"] != 0 {
		t.Errorf("connected = %v, want 0", got["connected"])
	}
	if got["disconnected"] != 1 {
		t.Errorf("disconnected = %v, want 1: one edge flapped three times and there is still one edge to chase", got["disconnected"])
	}

	client.bindEdgeTransport(7, 700)
	got = gaugeValues(t, registry)
	if got["connected"] != 1 || got["disconnected"] != 0 {
		t.Errorf("after the edge came back: %v, want connected 1 / disconnected 0 -- the series reads "+
			"where the edges are now, not how many times anything happened", got)
	}
}

// Unbinding a transport the manager never bound is a broker telling it
// about a dial that failed authentication. Counting that as a disconnect
// would report edges that were never online.
func TestAnUnboundTransportIsNotCountedAsADisconnect(t *testing.T) {
	registry := prometheus.NewRegistry()
	prom.RegisterManagerMetrics(registry, nil)
	client := newWithService(nil, nil)

	client.unbindTransport(99)
	got := gaugeValues(t, registry)
	if got["disconnected"] != 0 {
		t.Errorf("disconnected = %v, want 0: a dial that never proved an identity was never a connection", got["disconnected"])
	}
}
