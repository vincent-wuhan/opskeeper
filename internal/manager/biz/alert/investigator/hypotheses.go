package investigator

// hypotheses.go — 多假设并行调查 + Verifier 反向复核（BENY-912，借鉴
// Resolve AI 的 Incidents Agent 结构）。
//
// 动机：单链推理的根因只有"一个模型的第一次猜测"。改成：一个 incident
// 按角度拉起 N 个假设 worker 并行查证（容量/依赖/变更/配置），Verifier
// 用另一次 LLM 调用把每条假设的证据**反向核一遍**，只输出带支撑证据与
// 被反驳清单的合成结论——不可证伪的根因不再单独出现。
//
// 配置（默认关闭，行为与单 worker 完全一致）：
//   - cfg.HypothesisAngles 非空 → 多假设模式（角度数 = 并行 worker 数）
//   - cfg.VerifierEnabled     → 多假设结果经 Verifier 复核合成

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	chatruntime "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/chatruntime"
	alertmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/alert"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/llm"

	"log/slog"
)

// DefaultHypothesisAngles 默认假设角度：覆盖生产事故的四大根因族。
var DefaultHypothesisAngles = []string{
	"容量与资源饱和（连接池/线程池/内存/磁盘IO/CPU 是否到顶，采集指标证据）",
	"依赖与下游故障（依赖服务/数据库/缓存/网络是否异常，采集链路与日志证据）",
	"近期变更与发布（告警时间窗内的部署/配置修改/迁移，采集变更记录证据）",
	"配置漂移与默认值（参数与基线不一致、静默变更，采集配置对比证据）",
}

type hypothesisResult struct {
	Angle   string
	Answer  string
	Session string
	Err     error
}

// runHypotheses 并行 spawn N 个假设 worker。任一角度失败不致命——
// Verifier 收到的是"成功角度"的子集；全部失败才返回空。
func (uc *Usecase) runHypotheses(ctx context.Context, incident *alertmodel.Incident, angles []string, locale string, ownerUserID uint64) []hypothesisResult {
	results := make([]hypothesisResult, len(angles))
	var wg sync.WaitGroup
	for i, angle := range angles {
		wg.Add(1)
		go func(i int, angle string) {
			defer wg.Done()
			prompt := renderAlertPrompt(incident, locale) + hypothesisDirective(angle)
			worker, err := uc.spawner.SpawnWorker(ctx, chatruntime.SpawnRequest{
				AgentName:   uc.cfg.AgentName,
				Prompt:      prompt,
				Background:  false,
				SessionKind: "investigation",
				OwnerUserID: ownerUserID,
			})
			if err != nil || worker == nil {
				results[i] = hypothesisResult{Angle: angle, Err: fmt.Errorf("spawn: %v", err)}
				return
			}
			results[i] = hypothesisResult{
				Angle:   angle,
				Answer:  strings.TrimSpace(worker.Result),
				Session: worker.SessionID,
				Err:     nil,
			}
			if e := strings.TrimSpace(worker.Err); e != "" {
				results[i].Err = fmt.Errorf("worker: %s", e)
			}
		}(i, angle)
	}
	wg.Wait()
	var ok []hypothesisResult
	for _, r := range results {
		if r.Err == nil && r.Answer != "" {
			ok = append(ok, r)
		} else if r.Err != nil {
			uc.log.Warn("hypothesis worker failed",
				slog.String("angle", r.Angle), slog.Any("err", r.Err))
		}
	}
	return ok
}

// hypothesisDirective 把角度注入 worker 提示：限定查证方向 + 要求证据链。
func hypothesisDirective(angle string) string {
	return fmt.Sprintf(
		"\n\n假设分工指令：本次调查你**只**负责验证以下假设角度，其他角度交给并行同事：\n  %s\n"+
			"要求：1) 结论必须附带可证伪的证据链（具体指标名+数值+时间、日志片段、变更单号）；"+
			"2) 若证据不支持该假设，明确写「假设不成立」并给出反驳证据；3) 不要泛泛而谈。\n", angle)
}

// verifyHypotheses 用 LLM 把各假设结论反向复核并合成最终答案。
// 失败时回落到"拼接各角度结论"（保底可用，不因 Verifier 故障丢调查结果）。
func (uc *Usecase) verifyHypotheses(ctx context.Context, incident *alertmodel.Incident, results []hypothesisResult, locale string) string {
	var b strings.Builder
	b.WriteString("多个并行调查员分别验证了不同根因假设。请做反向复核（Verifier）：\n")
	b.WriteString("对每条假设判断其证据是否真实支撑结论，指出被反驳的假设，")
	b.WriteString("然后只把能在证据中复现的那条根因（或合并结论）作为最终根因输出。\n")
	b.WriteString("输出格式：\n最终根因：<一句话>\n支撑证据：<逐条列出，注明来自哪个假设角度>\n被反驳假设：<角度+反驳理由，没有则写无>\n置信度：<0-1>\n\n")
	for i, r := range results {
		fmt.Fprintf(&b, "=== 假设 %d（角度：%s）===\n%s\n\n", i+1, r.Angle, truncate(r.Answer, 4000))
	}

	if uc.summarizer != nil {
		resp, err := uc.summarizer.Chat(ctx, llm.ChatReq{
			Model:       uc.cfg.SummarizerModel,
			Provider:    uc.cfg.SummarizerProvider,
			Temperature: 0,
			Messages: []llm.Message{
				{Role: "system", Content: "你是 SRE 根因复核员（Verifier）。你只输出带证据的复核结论，不引入新假设。"},
				{Role: "user", Content: b.String()},
			},
		})
		if err == nil && resp != nil && strings.TrimSpace(resp.Assistant.Content) != "" {
			return strings.TrimSpace(resp.Assistant.Content)
		}
		uc.log.Warn("verifier LLM failed, falling back to concatenated hypotheses",
			slog.Any("err", err))
	}
	// 保底：拼接各角度结论（不丢调查产出）
	var fb strings.Builder
	fb.WriteString("（Verifier 不可用，以下为各假设角度未复核结论）\n\n")
	for _, r := range results {
		fmt.Fprintf(&fb, "=== %s ===\n%s\n\n", r.Angle, truncate(r.Answer, 4000))
	}
	return fb.String()
}

// runHypothesisMode 多假设并行主流程：并行假设 worker →（可选）Verifier
// 复核合成 → 统一收尾。全部角度失败时报告 failed（与单 worker 路径一致）。
func (uc *Usecase) runHypothesisMode(ctx context.Context, reportID string, incident alertmodel.Incident, locale string, ownerUserID uint64, start time.Time) {
	uc.log.Info("hypothesis mode: spawning parallel investigators",
		slog.Int("angles", len(uc.cfg.HypothesisAngles)),
		slog.Uint64("incident_id", incident.ID))

	results := uc.runHypotheses(ctx, &incident, uc.cfg.HypothesisAngles, locale, ownerUserID)
	if len(results) == 0 {
		_ = uc.repo.UpdateStatus(context.Background(), reportID,
			alertmodel.InvestigationStatusFailed, "all hypothesis workers failed")
		return
	}

	var finalAnswer string
	if uc.cfg.VerifierEnabled {
		finalAnswer = uc.verifyHypotheses(ctx, &incident, results, locale)
	} else {
		var b strings.Builder
		for _, r := range results {
			fmt.Fprintf(&b, "=== %s ===\n%s\n\n", r.Angle, truncate(r.Answer, 4000))
		}
		finalAnswer = b.String()
	}

	toolCalls := 0
	for _, r := range results {
		toolCalls += uc.countToolCalls(r.Session)
	}
	uc.finalizeAnswer(reportID, incident, finalAnswer, toolCalls, locale)
}
