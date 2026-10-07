package host

import (
	"context"
	"encoding/json"
)

// HostProcessTerminationRequest is what the recovery flow asks a host to
// terminate. It names an incident and the fixture manifest that recorded
// which processes belonged to it, and deliberately nothing else: there is
// no PID and no shell string, because a request that could carry either
// would be a second way to run a command on a host.
//
// The fixture resolves the process group from its own process table using
// these two fields. That is the whole point — the request identifies
// what to stop, and only the host that recorded it can say what that is.
type HostProcessTerminationRequest struct {
	IncidentID        string
	FixtureManifestID string
}

// HostProcessTerminator is the seam recovery executes through. An
// implementation resolves the incident-owned process group server-side
// and proves recovery with a fresh probe; the caller never learns the
// pids and never chooses them.
type HostProcessTerminator interface {
	Status(ctx context.Context, request HostProcessTerminationRequest) (HostFixtureStatus, error)
	Terminate(ctx context.Context, request HostProcessTerminationRequest) (json.RawMessage, error)
}
