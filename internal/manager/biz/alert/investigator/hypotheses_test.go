package investigator

import (
	"context"
	"strings"
	"sync"
	"testing"

	chatruntime "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/chatruntime"
	alertmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/alert"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/llm"
	"log/slog"
)

// 并行假设：每个角度一个 worker；失败角度被剔除。
func TestRunHypothesesParallelAndFiltering(t *testing.T) {
	var mu sync.Mutex
	var prompts []string
	uc := &Usecase{
		cfg:     Config{AgentName: "incident-investigator"},
		log:     slog.Default(),
		spawner: &fakeSpawner{},
	}
	uc.spawner = &promptRoutingSpawner{mu: &mu, prompts: &prompts}
	results := uc.runHypotheses(context.Background(), &alertmodel.Incident{}, []string{"容量", "依赖", "变更"}, "zh", 0)
	if len(prompts) != 3 {
		t.Fatalf("spawned = %d, want 3", len(prompts))
	}
	if len(results) != 2 { // 依赖角度失败被剔除
		t.Fatalf("results = %d, want 2", len(results))
	}
	for _, r := range results {
		if r.Angle == "依赖" || r.Answer == "" {
			t.Fatalf("bad result: %+v", r)
		}
	}
}

// promptRoutingSpawner 按提示内容路由结果："依赖"角度失败，其余成功。
type promptRoutingSpawner struct {
	mu      *sync.Mutex
	prompts *[]string
}

func (p *promptRoutingSpawner) SpawnWorker(_ context.Context, req chatruntime.SpawnRequest) (*chatruntime.Worker, error) {
	p.mu.Lock()
	*p.prompts = append(*p.prompts, req.Prompt)
	angle := ""
	if strings.Contains(req.Prompt, "依赖") {
		angle = "依赖"
	}
	p.mu.Unlock()
	if angle == "依赖" {
		return nil, context.DeadlineExceeded
	}
	return &chatruntime.Worker{Result: "结论:" + angle, SessionID: "s"}, nil
}
func (p *promptRoutingSpawner) StopWorker(context.Context, string) error { return nil }

// Verifier：LLM 成功 → 返回复核结论；LLM 失败 → 保底拼接。
func TestVerifyHypotheses(t *testing.T) {
	uc := &Usecase{log: slog.Default(), cfg: Config{}}
	results := []hypothesisResult{{Angle: "容量", Answer: "根因A"}, {Angle: "变更", Answer: "根因B"}}

	uc.summarizer = &fakeSummarizer{resp: &llm.ChatResp{Assistant: llm.Message{Content: "最终根因：根因A\n支撑证据：容量指标"}}}
	if got := uc.verifyHypotheses(context.Background(), &alertmodel.Incident{}, results, "zh"); !strings.Contains(got, "最终根因") {
		t.Fatalf("verifier output missing verdict: %q", got)
	}

	uc.summarizer = &fakeSummarizer{err: context.DeadlineExceeded}
	if got := uc.verifyHypotheses(context.Background(), &alertmodel.Incident{}, results, "zh"); !strings.Contains(got, "根因A") {
		t.Fatalf("fallback should keep hypotheses: %q", got)
	}
}
