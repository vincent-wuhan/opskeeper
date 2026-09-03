package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/skill"
)

func init() { skill.Register(&MTR{}) }

// MTR 综合 traceroute + ping (类似 `mtr -n -r -c 1 -w`).
// 报告每一跳的丢包率 / 平均 / 最差 RTT.
type MTR struct{}

func (MTR) Metadata() skill.Metadata {
	return skill.Metadata{
		Key:         "host_mtr",
		Name:        "路由+丢包综合探测",
		Description: "mtr -n -r -c 1 -w 输出 JSON; 报告每跳丢包率 + Avg/Best/Worst/StDev RTT. 诊断跨运营商丢包 / 中段路由黑洞. NOT for: 单点延迟 (probe_tcp) / 跳点定位 (traceroute).",
		Class:       skill.ClassSafe,
		Category:    "network",
		Params: skill.ParamSchema{
			{Name: "host", Param: skill.Param{Type: "string", Required: true, Desc: "目标主机"}},
			{Name: "timeout_sec", Param: skill.Param{Type: "int", Default: 30, Desc: "总超时秒数"}},
		},
		ResultPreview: "{host, report: {hubs: [{hop, host, loss_pct, avg_ms, ...}]}, duration_ms, error?}",
	}
}

type mtrParams struct {
	Host       string `json:"host"`
	TimeoutSec int    `json:"timeout_sec"`
}

type mtrHub struct {
	Hop     int     `json:"hop"`
	Host    string  `json:"host"`
	LossPct float64 `json:"loss_pct"`
	AvgMS   float64 `json:"avg_ms,omitempty"`
	BestMS  float64 `json:"best_ms,omitempty"`
	WorstMS float64 `json:"worst_ms,omitempty"`
	StDevMS float64 `json:"stdev_ms,omitempty"`
}

type mtrReport struct {
	Hubs []mtrHub `json:"hubs"`
}

type mtrResult struct {
	Host       string    `json:"host"`
	Report     mtrReport `json:"report"`
	DurationMS int64     `json:"duration_ms"`
	Error      string    `json:"error,omitempty"`
}

func (MTR) Execute(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var p mtrParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("mtr: decode params: %w", err)
		}
	}
	if p.Host == "" {
		return nil, fmt.Errorf("mtr: host required")
	}
	if p.TimeoutSec <= 0 {
		p.TimeoutSec = 30
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSec)*time.Second)
	defer cancel()

	start := time.Now()
	// mtr -n -r -c 1 -w -J 输出 JSON
	cmd := exec.CommandContext(ctx, "mtr", "-n", "-r", "-c", "1", "-w", "-J", p.Host)
	output, err := cmd.Output()
	res := mtrResult{
		Host:       p.Host,
		DurationMS: time.Since(start).Milliseconds(),
	}
	if err != nil {
		res.Error = err.Error()
		// mtr 不可用 (macOS 等) — 返回明确错误
		return json.Marshal(res)
	}
	parseMTRJSON(output, &res)
	return json.Marshal(res)
}

func parseMTRJSON(raw []byte, res *mtrResult) {
	// mtr JSON schema: {"report": {"hubs": [{"host": "...", "Loss%": 0.0, ...}]}}
	var wrap struct {
		Report struct {
			Hubs []map[string]any `json:"hubs"`
		} `json:"report"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		res.Error = fmt.Sprintf("mtr: parse json: %v", err)
		return
	}
	for _, h := range wrap.Report.Hubs {
		hub := mtrHub{Host: fmt.Sprint(h["host"])}
		if n, ok := h["count"].(float64); ok {
			hub.Hop = int(n)
		}
		if l, ok := h["Loss%"].(float64); ok {
			hub.LossPct = l
		}
		if v, ok := h["Avg"].(float64); ok {
			hub.AvgMS = v
		}
		if v, ok := h["Best"].(float64); ok {
			hub.BestMS = v
		}
		if v, ok := h["Wrst"].(float64); ok {
			hub.WorstMS = v
		}
		if v, ok := h["StDev"].(float64); ok {
			hub.StDevMS = v
		}
		res.Report.Hubs = append(res.Report.Hubs, hub)
	}
}
