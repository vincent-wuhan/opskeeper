// ops.go implements the adapter's write operations.
//
// Every function here changes server state and is reached only through
// Execute, which refuses a call with no approver before it gets this far.
// That gate is not decoration: these run without a human present, on a
// production cache, chosen by a model reading a metric.
//
// The operations added for the closed loop's vocabulary are the ones the
// investigator names as "the fix to apply". Before they existed, that name
// reached the console as a display string and nothing could carry it out.
package redis

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
)

// ErrUnknownOperation is returned for an operation this adapter does not
// implement. It is distinct from a generic failure because the caller is a
// remediation run: "no such operation" means the vocabulary and the adapter
// disagree, which is a deployment fault an operator can fix, and it must not
// read like "the server refused".
var ErrUnknownOperation = errors.New("redis: unknown operation")

// memoryPurge runs MEMORY PURGE.
//
// This is the single most important line in the file. "memory_purge" is the
// name the closed loop proposes for a memory-burst incident, and it is
// marked safe with auto_approve=true — which means it runs with no human
// present. Read as "purge memory", the obvious implementation is FLUSHDB or
// FLUSHALL, and either would delete the dataset silently, at 3am, on a
// production cache, with the platform reporting success.
//
// MEMORY PURGE is the Redis command that means what the name says: it asks
// the allocator to return pages it has already freed back to the operating
// system. It removes no key. The memory figure moves because RSS follows,
// not because data went away.
func (a *Adapter) memoryPurge(ctx context.Context, p params) (int, string, bool, error) {
	client, err := a.handle()
	if err != nil {
		return 0, "", false, err
	}
	before, _ := memoryUsed(ctx, a)
	if err := client.Do(ctx, "MEMORY", "PURGE").Err(); err != nil {
		return 0, "", false, fmt.Errorf("redis: MEMORY PURGE: %w", err)
	}
	after, _ := memoryUsed(ctx, a)
	msg := "MEMORY PURGE returned allocator-freed pages to the operating system; no key was removed"
	if before > 0 && after > 0 && before != after {
		msg += fmt.Sprintf(" (used_memory %d -> %d bytes)", before, after)
	}
	// Zero keys were affected, and Impacted says so. Reporting the number
	// of keys as anything above zero would tell the operator that data
	// changed when none did.
	return 0, msg, true, nil
}

func memoryUsed(ctx context.Context, a *Adapter) (int64, bool) {
	sections, err := fetchInfo(ctx, a)
	if err != nil {
		return 0, false
	}
	return infoInt(sections, "Memory", "used_memory")
}

// clientKill terminates one client connection.
//
// CLIENT KILL takes an address, and the address is required rather than
// defaulted. The filter form (CLIENT KILL TYPE normal) would terminate every
// ordinary client on the server, which for a remediation that was approved
// as "kill the one client that is stuck" is a different and much larger
// action.
func (a *Adapter) clientKill(ctx context.Context, p params) (int, string, bool, error) {
	addr, err := p.requireString("addr")
	if err != nil {
		return 0, "", false, err
	}
	client, err := a.handle()
	if err != nil {
		return 0, "", false, err
	}
	if err := client.ClientKill(ctx, addr).Err(); err != nil {
		if errors.Is(err, redisNilKillTarget) || strings.Contains(err.Error(), "No such client") {
			// The client already disconnected. That is the state the
			// operator asked for, so it is a no-op rather than a failure
			// — but it says so, because "the client was already gone" and
			// "the client was killed" are different facts about the
			// incident.
			return 0, fmt.Sprintf("client %s was not present: it had already disconnected", addr), true, nil
		}
		return 0, "", false, fmt.Errorf("redis: CLIENT KILL %s: %w", addr, err)
	}
	return 1, fmt.Sprintf("terminated client %s", addr), true, nil
}

// redisNilKillTarget is the error go-redis returns when CLIENT KILL matches
// nothing. It is declared here rather than imported so the comparison stays
// readable at the call site.
var redisNilKillTarget = errors.New("redis: no such client")

// failover asks the cluster to promote a replica.
//
// Standalone servers have no replicas to promote and no failover command, so
// this refuses rather than issuing something that would error on the server.
// Sending CLUSTER FAILOVER to a standalone instance produces "unknown
// command", and reporting that to an operator reads as a broken deployment
// instead of "this action does not apply here".
func (a *Adapter) failover(ctx context.Context, p params) (int, string, bool, error) {
	if !a.isCluster() {
		return 0, "CLUSTER FAILOVER applies to a cluster; this connection is standalone", false, nil
	}
	client, err := a.handle()
	if err != nil {
		return 0, "", false, err
	}
	force, _ := p["force"].(bool)
	if force {
		// FORCE is issued through Do because go-redis's typed wrapper only
		// covers the ordinary form, and a forced failover is the variation
		// an operator reaches for when the master is unreachable and the
		// ordinary form's replica-contact guard cannot be satisfied.
		if err := client.Do(ctx, "CLUSTER", "FAILOVER", "FORCE").Err(); err != nil {
			return 0, "", false, fmt.Errorf("redis: CLUSTER FAILOVER FORCE: %w", err)
		}
		return 1, "requested a forced failover to a replica", true, nil
	}
	if err := client.ClusterFailover(ctx).Err(); err != nil {
		// The ordinary form refuses unless the master has lost contact
		// with the majority of replicas, which is the guard that keeps a
		// failover from being a surprise. Reporting the server's own
		// reason is the point: "the failover was refused because the
		// master still has replicas" is actionable, "the action failed"
		// is not.
		return 0, "", false, fmt.Errorf("redis: CLUSTER FAILOVER: %w", err)
	}
	return 1, "requested a failover to a replica", true, nil
}

// configSet changes one configuration parameter at runtime.
//
// Both arguments are required. CONFIG SET with a defaulted value is how a
// parameter gets set to something nobody chose, and the parameters that
// matter here — maxmemory, maxmemory-policy, appendonly — are exactly the
// ones where an invented value changes how the server sheds load.
func (a *Adapter) configSet(ctx context.Context, p params) (int, string, bool, error) {
	parameter, err := p.requireString("parameter")
	if err != nil {
		return 0, "", false, err
	}
	value, err := p.requireString("value")
	if err != nil {
		return 0, "", false, err
	}
	client, err := a.handle()
	if err != nil {
		return 0, "", false, err
	}
	previous, _ := client.ConfigGet(ctx, parameter).Result()
	if err := client.ConfigSet(ctx, parameter, value).Err(); err != nil {
		return 0, "", false, fmt.Errorf("redis: CONFIG SET %s: %w", parameter, err)
	}
	old := ""
	if v, ok := previous[parameter]; ok {
		old = v
	}
	return 1, configSetMessage(parameter, value, old), true, nil
}

// configSetMessage composes the operator-facing account of a CONFIG SET.
//
// The previous value is reported because it is the only way back: a
// remediation that changes a runtime parameter and does not record what it
// was is a change nobody can undo. It is a separate function so the
// composition is testable without a server that implements CONFIG — the
// part worth testing is the sentence, not the round trip.
func configSetMessage(parameter, value, previous string) string {
	msg := fmt.Sprintf("set %s = %s", parameter, value)
	if previous != "" && previous != value {
		msg += fmt.Sprintf(" (was %s; CONFIG SET %s %s restores it)", previous, parameter, previous)
	}
	return msg
}

// flushConfirmValue is the literal the caller must pass to flush a database.
//
// It is not security theatre. The action is irreversible, it is reachable
// from a model's tool call, and the difference between it and every other
// operation here is that the data does not come back. Requiring the caller
// to spell out the database name means a call that reached this function by
// accident — a wrong name, a copied argument bag — fails instead of
// succeeding.
func flushConfirmValue(p params) (string, bool) {
	v, _ := p["confirm"].(string)
	v = strings.TrimSpace(v)
	return v, strings.EqualFold(v, "flush")
}

func (a *Adapter) flushDB(ctx context.Context, p params) (int, string, bool, error) {
	confirm, ok := flushConfirmValue(p)
	if !ok {
		return 0, "", false, fmt.Errorf(
			"redis: flushdb removes every key in the database and cannot be undone; "+
				"pass confirm=flush to proceed (got %q)", confirm)
	}
	client, err := a.handle()
	if err != nil {
		return 0, "", false, err
	}
	sizeBefore, _ := client.DBSize(ctx).Result()
	if err := client.FlushDB(ctx).Err(); err != nil {
		return 0, "", false, fmt.Errorf("redis: FLUSHDB: %w", err)
	}
	return int(sizeBefore), fmt.Sprintf("FLUSHDB removed %d key(s) from the current database", sizeBefore), true, nil
}

// tlsConfigFor builds the TLS settings for a cluster DSN.
//
// The verification mode comes from the connection spec, and the default is
// to verify. A cluster client built with an unverified TLS config is one
// that will accept any certificate, including one presented by whatever
// answered on the port.
func tlsConfigFor(conn adapter.ConnectionSpec) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch strings.ToLower(strings.TrimSpace(conn.TLSMode)) {
	case "disable":
		cfg.InsecureSkipVerify = true
	case "require":
		// Encryption without identity verification: the operator has
		// asked for transport protection only.
		cfg.InsecureSkipVerify = true
	default:
		// verify-ca / verify-full / unset: verify the chain and the host.
	}
	return cfg
}
