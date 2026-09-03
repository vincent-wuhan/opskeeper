package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/basetool"
	"github.com/vincent-wuhan/opskeeper/internal/manager/model/aiops"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

// ToolNameChatToQuery is the stable wire name the LLM sees.
const ToolNameChatToQuery = "chat_to_query"

// chatToQueryWhenToUse is the routing hint shown to the LLM. Phrased
// to push the model toward this tool whenever the user asks an
// open-ended metric / log / trace question that doesn't fit the
// narrower query_promql / query_logql / query_traceql surfaces (which
// require hand-written expressions).
const chatToQueryWhenToUse = "When the user asks a metric / log / trace question in natural language " +
	"and you don't already know the exact expression — e.g. 'Redis memory usage', 'errors in nginx last hour', " +
	"'p99 latency for checkout'. " +
	"Returns a dry-run preview by default; pass execute=true ONLY after the user confirms the preview. " +
	"NOT for executing pre-written queries (use query_promql / query_logql / query_traceql directly)."

var ChatToQueryDescription = "Translate a natural-language question into PromQL / LogQL / TraceQL " +
	"and (optionally) execute it. By default returns a dry-run preview showing the generated query, " +
	"risk level, and explanation. Pass execute=true to run the query and cache the translation as a template " +
	"for future reuse. Translations that succeed 3+ times within 30 days skip the LLM round-trip."

var ChatToQuerySchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "question": {
      "type": "string",
      "description": "Natural-language question, e.g. 'Redis memory usage last 5 minutes'."
    },
    "signal": {
      "type": "string",
      "enum": ["auto", "promql", "logql", "traceql"],
      "description": "Query language hint. 'auto' (default) routes by keyword; pass an explicit value when the heuristic guesses wrong."
    },
    "execute": {
      "type": "boolean",
      "default": false,
      "description": "If true, run the generated query and cache it. Default false (dry-run preview)."
    },
    "lookback_seconds": {
      "type": "integer",
      "minimum": 60,
      "maximum": 604800,
      "description": "Lookback for the generated query (default 300). Only honored when execute=true."
    }
  },
  "required": ["question"]
}`)

// ChatToQueryArgs is the typed form of ChatToQuerySchema.
type ChatToQueryArgs struct {
	Question        string `json:"question"`
	Signal          string `json:"signal,omitempty"`
	Execute         bool   `json:"execute,omitempty"`
	LookbackSeconds int    `json:"lookback_seconds,omitempty"`
}

// ChatToQueryResult is the JSON object returned by InvokableRun.
type ChatToQueryResult struct {
	Mode        string          `json:"mode"`   // "preview" | "executed"
	Signal      string          `json:"signal"` // promql | logql | traceql
	Query       string          `json:"query"`
	Risk        string          `json:"risk"` // low | medium | high
	Explanation string          `json:"explanation"`
	ContextUsed []string        `json:"context_used,omitempty"`
	TemplateHit bool            `json:"template_hit"`
	Result      json.RawMessage `json:"result,omitempty"` // only when Mode=executed
	Error       string          `json:"error,omitempty"`
	RiskReason  string          `json:"risk_reason,omitempty"`
}

// TemplateSink is the narrow interface ChatToQueryTool needs from the
// QueryTemplateStore. Defined here (not imported from the store pkg)
// so the tool can be tested with an in-memory fake.
type TemplateSink interface {
	Get(ctx context.Context, tenantID, signal, nlHash string) (*aiops.QueryTemplate, error)
	Upsert(ctx context.Context, tpl *aiops.QueryTemplate) error
}

// Executor abstracts the three query clients behind one interface.
// Production wires this to a struct holding *promquery.Client,
// *logquery.Client, *tracequery.Client; tests pass a fake.
type Executor interface {
	Run(ctx context.Context, signal, expr string, lookback time.Duration) (json.RawMessage, error)
}

// ChatToQueryTool is the BaseTool implementation of chat_to_query.
type ChatToQueryTool struct {
	translator *Translator
	validator  *Validator
	templates  TemplateSink
	exec       Executor
	log        *slog.Logger
}

// NewChatToQueryTool builds the tool. Any dependency may be nil in
// tests; production wires all of them in main.go.
func NewChatToQueryTool(tr *Translator, va *Validator, tpl TemplateSink, exec Executor, log *slog.Logger) *ChatToQueryTool {
	if log == nil {
		log = slog.Default()
	}
	return &ChatToQueryTool{
		translator: tr,
		validator:  va,
		templates:  tpl,
		exec:       exec,
		log:        log,
	}
}

// Info returns the tool metadata.
func (t *ChatToQueryTool) Info(_ context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{
		Name:        ToolNameChatToQuery,
		Description: ChatToQueryDescription,
		WhenToUse:   chatToQueryWhenToUse,
		Parameters:  ChatToQuerySchema,
		Class:       "read",
	}, nil
}

// NLNormalize produces the canonical form used for NLHash lookup:
// lowercase + strip punctuation + collapse whitespace. The same
// function must be used on read and write paths or the cache misses
// spuriously.
var nonAlnumRE = regexp.MustCompile(`[^a-z0-9\p{Han}]+`)

func NLNormalize(q string) string {
	q = strings.ToLower(strings.TrimSpace(q))
	q = nonAlnumRE.ReplaceAllString(q, " ")
	return strings.TrimSpace(q)
}

// NLHash returns sha256(normalize(question)) as hex — the cache key.
func NLHash(q string) string {
	sum := sha256.Sum256([]byte(NLNormalize(q)))
	return hex.EncodeToString(sum[:])
}

// InvokableRun runs the chat_to_query pipeline. opts are unused —
// the tenant_id is pulled from ctx (via the tenant_bind decorator
// upstream); the device_id isn't relevant for a query-generation tool.
func (t *ChatToQueryTool) InvokableRun(ctx context.Context, argsJSON string, _ ...basetool.InvokeOption) (string, error) {
	var args ChatToQueryArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("%s: bad args: %w", ToolNameChatToQuery, err)
	}
	if args.Question == "" {
		return "", fmt.Errorf("%s: question required: %w", ToolNameChatToQuery, errs.ErrInvalid)
	}
	tenantID, _ := ctx.Value(tenantCtxKey{}).(string)
	if tenantID == "" {
		return "", fmt.Errorf("%s: tenant_id missing from context", ToolNameChatToQuery)
	}

	hash := NLHash(args.Question)
	res := &ChatToQueryResult{Signal: args.Signal}

	// Step 1: template hit?
	if t.templates != nil {
		tpl, err := t.templates.Get(ctx, tenantID, args.Signal, hash)
		if err != nil {
			t.log.Warn("chat_to_query: template lookup failed", "err", err)
		} else if tpl != nil {
			res.Signal = tpl.Signal
			res.Query = tpl.Expr
			res.Risk = tpl.Risk
			res.Explanation = tpl.Explanation
			res.TemplateHit = true
			t.log.Info("chat_to_query: template hit",
				"tenant", tenantID, "signal", tpl.Signal, "hits", tpl.Hits)
			return t.finishWithExecution(ctx, tenantID, tpl.ID, res, args)
		}
	}

	// Step 2: translate.
	if t.translator == nil {
		return "", fmt.Errorf("%s: translator not configured", ToolNameChatToQuery)
	}
	tr, err := t.translator.Translate(ctx, args.Question, args.Signal)
	if err != nil {
		return marshalError(ToolNameChatToQuery, err)
	}
	res.Signal = tr.Signal
	res.Query = tr.Query
	res.Explanation = tr.Explanation

	// Step 3: validate.
	if t.validator == nil {
		return "", fmt.Errorf("%s: validator not configured", ToolNameChatToQuery)
	}
	vres, err := t.validator.Validate(ctx, res.Signal, res.Query)
	if err != nil {
		res.Error = err.Error()
		return marshalResult(res)
	}
	res.Risk = vres.Risk
	res.RiskReason = vres.Reason

	// Step 4: dry-run or execute?
	res.Mode = "preview"
	if !args.Execute {
		return marshalResult(res)
	}
	return t.executeAndCache(ctx, tenantID, hash, args, res)
}

// finishWithExecution runs a template-hit query and bumps its hits
// count. Used when the template cache returned a warm row — we
// skip the LLM entirely.
func (t *ChatToQueryTool) finishWithExecution(ctx context.Context, tenantID string, tplID uint, res *ChatToQueryResult, args ChatToQueryArgs) (string, error) {
	res.Mode = "preview"
	if !args.Execute {
		return marshalResult(res)
	}
	return t.runAndTouch(ctx, tenantID, tplID, args, res, true)
}

// executeAndCache runs a freshly-translated query and (on success)
// inserts / bumps the cache row. Mirrors finishWithExecution but the
// Upsert path is "INSERT new row" rather than "Touch existing".
func (t *ChatToQueryTool) executeAndCache(ctx context.Context, tenantID, hash string, args ChatToQueryArgs, res *ChatToQueryResult) (string, error) {
	if t.exec == nil {
		res.Error = "executor not configured"
		return marshalResult(res)
	}
	if args.LookbackSeconds <= 0 {
		args.LookbackSeconds = 300
	}
	result, err := t.exec.Run(ctx, res.Signal, res.Query, time.Duration(args.LookbackSeconds)*time.Second)
	if err != nil {
		res.Error = err.Error()
		return marshalResult(res)
	}
	res.Result = result
	res.Mode = "executed"

	// Upsert template only on success. The validator already passed,
	// and exec returned no error — that's "good enough" for the LRU.
	if t.templates != nil {
		_ = t.templates.Upsert(ctx, &aiops.QueryTemplate{
			TenantID:    tenantID,
			NLHash:      hash,
			Signal:      res.Signal,
			Question:    args.Question,
			Expr:        res.Query,
			Risk:        res.Risk,
			Explanation: res.Explanation,
			Hits:        1,
			LastUsedAt:  time.Now().UTC(),
		})
	}
	return marshalResult(res)
}

// runAndTouch is the shared execute-and-bump path for template hits.
func (t *ChatToQueryTool) runAndTouch(ctx context.Context, tenantID string, tplID uint, args ChatToQueryArgs, res *ChatToQueryResult, _ bool) (string, error) {
	if t.exec == nil {
		res.Error = "executor not configured"
		return marshalResult(res)
	}
	if args.LookbackSeconds <= 0 {
		args.LookbackSeconds = 300
	}
	result, err := t.exec.Run(ctx, res.Signal, res.Query, time.Duration(args.LookbackSeconds)*time.Second)
	if err != nil {
		res.Error = err.Error()
		return marshalResult(res)
	}
	res.Result = result
	res.Mode = "executed"

	if t.templates != nil {
		// Touch bumps Hits and LastUsedAt without rewriting the
		// expression. We can't call Touch directly because the
		// TemplateSink interface only exposes Get / Upsert; Upsert
		// does the right thing (UPDATE path) when NLHash matches.
		_ = t.templates.Upsert(ctx, &aiops.QueryTemplate{
			TenantID:    tenantID,
			NLHash:      NLHash(args.Question),
			Signal:      res.Signal,
			Question:    args.Question,
			Expr:        res.Query,
			Risk:        res.Risk,
			Explanation: res.Explanation,
			Hits:        1,
			LastUsedAt:  time.Now().UTC(),
		})
		_ = tplID // Touch ID kept for future store API split.
	}
	return marshalResult(res)
}

// tenantCtxKey is the context key the tenant_bind decorator uses.
// Defined here as a private struct so a misconfigured call (no
// decorator upstream) fails fast instead of silently hitting a
// global.
type tenantCtxKey struct{}

// marshalResult serializes the result; always returns valid JSON
// even on partial failures.
func marshalResult(res *ChatToQueryResult) (string, error) {
	b, err := json.Marshal(res)
	if err != nil {
		return "", fmt.Errorf("chat_to_query: marshal result: %w", err)
	}
	return string(b), nil
}

func marshalError(tool string, err error) (string, error) {
	b, _ := json.Marshal(ChatToQueryResult{
		Mode:  "preview",
		Error: err.Error(),
	})
	return string(b), nil
}
