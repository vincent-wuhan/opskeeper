package ports

import "context"

// sinkKey carries the EventSink for the turn currently in flight on a
// goroutine. The kernel reads it rather than taking a sink parameter,
// because the tool execution path runs inside PiG's loop where a parameter
// would have to be threaded through every callback.
type sinkKey struct{}

// WithSink returns a context carrying the streaming sink for a turn.
//
// A nil sink is stored as a no-op rather than rejected: a caller that wants
// a blocking, non-streaming turn should not have to construct a sink, and a
// kernel that treats "no sink" as an error would make the simplest call the
// hardest to make.
func WithSink(ctx context.Context, s EventSink) context.Context {
	if s == nil {
		s = DiscardSink{}
	}
	return context.WithValue(ctx, sinkKey{}, s)
}

// FromContext returns the sink for the turn in flight. It never returns nil.
func FromContext(ctx context.Context) EventSink {
	if s, ok := ctx.Value(sinkKey{}).(EventSink); ok && s != nil {
		return s
	}
	return DiscardSink{}
}

// DiscardSink drops every frame. It is the zero-cost default for a turn
// nobody is watching.
type DiscardSink struct{}

// Emit implements EventSink.
func (DiscardSink) Emit(context.Context, StreamEvent) error { return nil }
