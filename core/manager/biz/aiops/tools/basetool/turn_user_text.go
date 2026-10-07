// Shared ctx propagation for the live user turn.
//
// The transcript write path needs the text of the turn it is answering to
// decide whether a model-authored alert draft may stand (see the alertdraft
// package), and that path runs inside the kernel — it can see neither the
// *chatruntime.Request nor the caller. Same leaf-package rationale and shape
// as llm_choice.go / locale.go: chatruntime stamps it, the write path reads
// it back, and neither has to import the other.

package basetool

import "context"

type turnUserTextCtxKeyT struct{}

var turnUserTextCtxKey = turnUserTextCtxKeyT{}

// WithTurnUserText stamps ctx with the text of the user turn being answered.
//
// An empty text is stored rather than dropped: "no live turn" and "a turn
// with no text" are different questions, and the readers treat only the
// absent value as "not a turn" — a blank prompt is a real worker turn whose
// reply must not be rewritten by a rule about alert drafts.
func WithTurnUserText(ctx context.Context, text string) context.Context {
	return context.WithValue(ctx, turnUserTextCtxKey, text)
}

// TurnUserTextFromContext returns the live turn's text; ok is false when no
// turn was stamped.
func TurnUserTextFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(turnUserTextCtxKey).(string)
	return v, ok
}
