// fake_completer_test.go is the phase workers' scripted model, rebuilt on
// top of the shared PiG test stub.
//
// It used to be a production file implementing the deleted llm.Client
// interface. That was a mistake the rewrite corrects for two reasons. A fake
// shipped in the binary is a fake an operator can accidentally wire in
// production, and — more to the point — it asserted on a translated request
// struct, so a test could check lastUserPrompt while never seeing the
// transcript the provider would actually have received. The bug this class
// of fake hides is precisely the one that does not error.
package loop

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/pig/pigai"

	"github.com/vincent-wuhan/opskeeper/core/pig/pigmodel"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigtest"
)

// fakeCompleter scripts replies for one test.
//
// The script is held here rather than pushed into the shared stub as calls
// happen, because the phase workers' tests set their whole script up front
// and then assert on call count. Pigtest's own sequential replies would
// work too; this wrapper exists so those tests read the way they did.
type fakeCompleter struct {
	inner *pigtest.Completer

	mu             sync.Mutex
	responses      []string
	errors         []error
	defaultContent string
	defaultErr     error
	lastUserPrompt string
	lastSystem     string
}

// newFakeCompleter returns a scripted model with an empty script.
func newFakeCompleter() *fakeCompleter {
	return &fakeCompleter{inner: pigtest.NewCompleter()}
}

// setResponse scripts the seq-th call's text.
func (f *fakeCompleter) setResponse(seq int, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.responses) <= seq {
		f.responses = append(f.responses, "")
	}
	f.responses[seq] = content
	f.rebuildLocked()
}

// setError scripts the seq-th call's failure. An error wins over a text set
// for the same index, matching Go's own "error short-circuits" convention.
func (f *fakeCompleter) setError(seq int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.errors) <= seq {
		f.errors = append(f.errors, nil)
	}
	f.errors[seq] = err
	f.rebuildLocked()
}

func (f *fakeCompleter) setDefault(content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defaultContent = content
	f.rebuildLocked()
}

func (f *fakeCompleter) setDefaultError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defaultErr = err
	f.rebuildLocked()
}

// rebuildLocked re-derives the whole script from responses/errors.
//
// It rebuilds rather than patches because the two slices are indexed
// independently and a call is answered from whichever is set at its own
// position; keeping one derived list avoids the class of bug where setting
// an error at index 3 silently shifts the responses after it.
func (f *fakeCompleter) rebuildLocked() {
	n := len(f.responses)
	if len(f.errors) > n {
		n = len(f.errors)
	}
	replies := make([]pigtest.Reply, 0, n)
	for i := 0; i < n; i++ {
		var r pigtest.Reply
		if i < len(f.errors) {
			r.Err = f.errors[i]
		}
		if r.Err == nil && i < len(f.responses) {
			r.Text = f.responses[i]
		}
		replies = append(replies, r)
	}
	fallback := pigtest.Reply{Text: f.defaultContent, Err: f.defaultErr}
	if f.defaultContent == "" && f.defaultErr == nil {
		// An unset default must not mask the end of the script with an
		// error; the last scripted reply is what a test asserting on call
		// count actually wants to see.
		fallback = pigtest.Reply{}
	}
	f.inner = pigtest.NewCompleter(replies...)
	f.inner.SetFallback(fallback)
}

// Complete implements pigmodel.Completer, recording the prompt a worker
// built so a test can assert on it.
func (f *fakeCompleter) Complete(ctx context.Context, req pigmodel.Request) (*pigai.AssistantMessage, error) {
	f.mu.Lock()
	f.lastUserPrompt, f.lastSystem = "", ""
	for _, m := range req.Messages {
		switch m.(type) {
		case pigai.UserMessage:
			f.lastUserPrompt = pigmodel.MessageText(m)
		case pigai.SystemMessage:
			f.lastSystem = pigmodel.MessageText(m)
		}
	}
	f.mu.Unlock()
	return f.inner.Complete(ctx, req)
}

func (f *fakeCompleter) callCount() int { return f.inner.Calls() }

func (f *fakeCompleter) lastUser() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastUserPrompt
}

func (f *fakeCompleter) lastSystemPrompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastSystem
}

// assertPromptContains is the shape most phase-worker tests actually want:
// "the worker sent the model something mentioning X". It is a helper rather
// than a raw string comparison because the failures it produces are the ones
// a maintainer can act on.
func assertPromptContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("prompt does not contain %q\ngot: %s", want, got)
	}
}
