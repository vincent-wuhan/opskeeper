//go:build e2e

package e2e

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/tests/e2e/testenv"
)

// The plan's second end-to-end line is "unplug the network → telemetry lands on
// disk → it replays on recovery → the audit comes back", and its first half is
// the claim the whole 1.1 item rests on: a node that cannot reach the center
// must not lose the samples it took while it could not.
//
// The two halves of that claim are tested at very different places, and both
// matter. The spool unit tests prove the log records, replays, acks and
// expires; the manager-side tests prove a replayed record is accepted. What
// neither can show is the seam: a real node, a real tunnel, a real socket
// that stops answering, and a real question of whether the samples taken in
// the dark are still there in the morning. That is what this file is for, and
// the harness it needs is why the outage is scoped to one edge through a
// severable proxy rather than by stopping the shared broker.
//
// Three things are asserted, and the middle one is the one the plan is
// actually about:
//
//  1. while the link is cut, the write-ahead log holds rows — the samples
//     exist somewhere durable instead of having been dropped on the floor;
//  2. once the link comes back, the log empties — the backlog was sent and
//     acked, not merely still sitting there;
//  3. the center received series across the outage that it had not received
//     before it — the drain went somewhere real, not into a void that returns
//     200.
//
// The counts are deliberately not compared one-for-one: a log row carries a
// batch that may travel the point RPC and the samples RPC separately, so the
// rows in the file and the series in Prometheus are not the same unit, and a
// test that pretended they were would be asserting an arithmetic coincidence
// rather than a property.

// walRows counts the rows waiting in a node's telemetry write-ahead log.
//
// The file is newline-delimited JSON, so this is an exact count of what the
// node has not yet been told the center took. It counts lines rather than
// bytes because "the file got smaller" would also be true of a log losing
// data, and that is the failure this test exists to catch.
func walRows(t *testing.T, dir string) int {
	t.Helper()
	file, err := os.Open(filepath.Join(dir, "telemetry-wal.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("open the node's telemetry log: %v", err)
	}
	defer file.Close()
	rows := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		if len(scanner.Bytes()) > 0 {
			rows++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read the node's telemetry log: %v", err)
	}
	return rows
}

// waitFor polls until condition holds, and fails with the last reading.
func waitFor(t *testing.T, what string, within time.Duration, read func() string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	last := ""
	for time.Now().Before(deadline) {
		last = read()
		if condition() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s; last reading: %s", within, what, last)
}

// watchRowsSpotsALoss samples the node's log repeatedly and reports the first
// moment the pending row count went *down* while the link was supposed to be
// down.
//
// This is the assertion that catches the failure the shape of the test cannot
// see on its own. A write-ahead log can hold rows during an outage for two
// very different reasons: the drain refused to run, or the drain ran, the
// send failed, and the row was acked anyway. Both look identical from
// outside — rows on disk — and the second one is silent data loss wearing the
// costume of durability. "The log is not empty" cannot tell them apart; "the
// log never shrank" can, because a shrink is only possible if something
// acknowledged a row it had not delivered.
//
// The reason this is worth an e2e at all, given the unit tests cover the
// ack rule: the unit test proves the rule in isolation, and the isolation is
// exactly what is in doubt here. A node whose heartbeat has not yet noticed
// the outage is a node whose drain is still being *attempted*, and that is
// the only window in which a wrong ack is reachable at all.
func watchRowsForLoss(t *testing.T, dir string, forHowLong time.Duration) (lost string) {
	t.Helper()
	deadline := time.Now().Add(forHowLong)
	peak := 0
	for time.Now().Before(deadline) {
		rows := walRows(t, dir)
		if rows < peak {
			return fmt.Sprintf("the pending row count fell from %d to %d with the link down", peak, rows)
		}
		peak = rows
		time.Sleep(300 * time.Millisecond)
	}
	return ""
}

func TestANodeKeepsItsTelemetryThroughAnOutage(t *testing.T) {
	frontier := testenv.SharedFrontier(t)
	env := testenv.Start(t, testenv.WithFrontier(frontier))
	login := env.LoginAdmin()

	// The node reaches the broker through a proxy this test can cut. The
	// broker itself is shared by every test in this process, and a broker
	// that is down is a different failure from a node that lost its link.
	link := testenv.NewLinkProxy(t, frontier.EdgeAddr)

	edgeID, access, secret := env.CreateEdge(t, login.AccessToken, "outage-node")
	edge := testenv.StartEdge(t, env, login.AccessToken, testenv.EdgeOptions{
		FrontierEdgeAddr:  link.Addr(),
		AccessKey:         access,
		SecretKey:         secret,
		GatewayBaseURL:    env.BaseURL() + "/v1",
		Model:             "fake-gpt",
		CollectorMode:     "embedded",
		CollectorInterval: 2 * time.Second,
		// A one-second heartbeat with a two-minute give-up window. The
		// pairing matters more than either number: what this test needs
		// from the node is that it *notices* the outage quickly, so the
		// drain is attempted inside the window and the pending count is
		// sampled against a node that already knows the link is gone.
		// Decision 133 had a 30s tick, which meant the node only learned
		// about the cut somewhere inside an unrelated constant, and the
		// outage had to be kept short enough to stay clear of a 150s
		// give-up threshold it had no business being near. The tolerance
		// here is deliberately still ~2 minutes, so what changed is the
		// resolution and not the safety margin.
		HeartbeatInterval:    time.Second,
		TunnelStuckThreshold: 120,
	})
	edge.ID = edgeID
	edge.WaitForRunningAgent(t, env, login.AccessToken, edgeID, 3*time.Minute)

	// 1. A healthy node's samples reach the center. Everything after this
	//    point is about what happens when that stops being true, and a test
	// that never saw the healthy case would happily pass on a node that has
	//    never pushed anything at all.
	link.WaitForLive(t, 1, 30*time.Second)
	waitFor(t, "the center to receive the node's first samples", 90*time.Second,
		func() string {
			total, _, requests := env.FakeProm().Written()
			return seriesReading(total, requests, walRows(t, edge.TelemetryWALDir))
		},
		func() bool {
			total, _, _ := env.FakeProm().Written()
			return total > 0
		})

	atCut, _, _ := env.FakeProm().Written()
	t.Logf("before the outage the center holds %d series", atCut)

	// 2. Unplug it. The refusal count is the evidence that the node tried to
	//    use the link and found it gone; without it, "the link is down" is
	//    something the test decided rather than something that happened.
	link.Cut()
	link.WaitForRefusedDials(t, 1, 60*time.Second)

	// 3. The samples taken in the dark are on disk. The node keeps sampling
	//    throughout — that is the whole point of a log, and a node that
	//    stopped sampling would also produce an empty log, which is why the
	//    assertion is "rows appeared" rather than "rows are zero".
	waitFor(t, "the node to spool samples while the link is down", 60*time.Second,
		func() string { return seriesReading(0, 0, walRows(t, edge.TelemetryWALDir)) },
		func() bool { return walRows(t, edge.TelemetryWALDir) > 0 })

	queued := walRows(t, edge.TelemetryWALDir)
	t.Logf("%d row(s) waiting on disk with the link down", queued)

	// Let the outage run long enough that the backlog is several rows rather
	// than one, and watch the whole time for the log shrinking. A single row
	// makes every later claim cheap: "the log drained" is also what a log
	// that only ever held one row would look like, and "the center got more
	// series" is satisfied by ordinary sampling the moment the link comes
	// back. The bound below only means something once there is a backlog to
	// account for, and the watch is what makes the backlog mean something.
	//
	// The window matters: for the first heartbeat tick after the cut the
	// node still believes it is online, so its drain is still being
	// attempted against a dead socket. That is the only moment a wrong ack
	// is reachable, and it is over long before the log has grown much.
	lossWindow := 45 * time.Second
	if lost := watchRowsForLoss(t, edge.TelemetryWALDir, lossWindow); lost != "" {
		t.Fatalf("%s; a row that was never delivered was acknowledged, and the samples taken "+
			"during the outage are gone with no trace anywhere", lost)
	}

	waitFor(t, "the node to spool more than one row while the link is down", 60*time.Second,
		func() string { return seriesReading(0, 0, walRows(t, edge.TelemetryWALDir)) },
		func() bool { return walRows(t, edge.TelemetryWALDir) > 1 })

	queued = walRows(t, edge.TelemetryWALDir)
	duringOutage, _, _ := env.FakeProm().Written()
	if queued == 0 {
		t.Fatalf("the node's log emptied while the link was down (%d series at the center, "+
			"%d before the cut): the samples taken in the outage were not kept, and a "+
			"write-ahead log that drops rows during the outage it exists for is worse "+
			"than no log", duringOutage, atCut)
	}
	t.Logf("%d row(s) survived the outage; the center went from %d to %d series while the "+
		"link was down", queued, atCut, duringOutage)

	// 4. Plug it back in. The backlog has to leave the disk.
	link.Heal()

	waitFor(t, "the node's log to drain after the link returned", 90*time.Second,
		func() string { return seriesReading(0, 0, walRows(t, edge.TelemetryWALDir)) },
		func() bool { return walRows(t, edge.TelemetryWALDir) == 0 })

	// 5. And the drain went to the center rather than into a 200 that
	//    accepts nothing. The bound is per row, not per series: a log row
	//    and a Prometheus series are not the same unit — a row may carry the
	//    point fast path, the open-set samples, or both — so the strongest
	//    claim that is actually true is that every row which left the disk
	//    became at least one series at the center. Anything weaker ("the
	//    count went up") is satisfied by the first ordinary sample after the
	//    link returns, which says nothing about the backlog.
	atEnd, _, _ := env.FakeProm().Written()
	if gained := atEnd - atCut; gained < queued {
		t.Errorf("the center gained %d series across the outage while %d row(s) left the "+
			"node's disk; every row that drained must account for at least one series, so "+
			"the backlog was accepted and discarded rather than stored",
			gained, queued)
	} else {
		t.Logf("the center went from %d to %d series across the outage; %d row(s) left the disk",
			atCut, atEnd, queued)
	}

	// 6. The node is still a node. Five consecutive failed heartbeats make a
	//    node exit for its supervisor to respawn, and a test that let the
	//    outage run long enough to trigger that would be measuring the
	//    respawn rather than the log — so the assertion is that the outage
	//    stayed inside the window where a node is supposed to ride it out.
	if pids := edge.AgentPIDs(t); len(pids) == 0 {
		t.Errorf("the node's agent process is gone after the outage\n=== edge logs ===\n%s", edge.Logs())
	}
	health := edge.WaitForRunningAgent(t, env, login.AccessToken, edgeID, 60*time.Second)
	if degraded, _ := health["degraded"].(bool); degraded {
		t.Errorf("the node reports itself degraded after the outage: %s", testenv.MustJSON(health))
	}
}

// seriesReading is one line of "what the three witnesses said", so a timeout
// reports the state of the node, the link and the center together instead of
// only the one that happened to be polled last.
func seriesReading(total, requests, rows int) string {
	return fmt.Sprintf("center series=%d write requests=%d rows on disk=%d", total, requests, rows)
}
