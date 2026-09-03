package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/skill"
)

func init() { skill.Register(&Traceroute{}) }

// Traceroute 追踪到目标的网络跳点 (类似 `traceroute -n -w 2 -q 1`).
// 数字模式 (-n) 避免 DNS 反查; 超时 2s, 每跳 1 次探测.
type Traceroute struct{}

func (Traceroute) Metadata() skill.Metadata {
	return skill.Metadata{
		Key:         "host_traceroute",
		Name:        "网络路由追踪",
		Description: "追踪到目标 host 的网络路由跳点 (traceroute -n -w 2 -q 1). 诊断路由环路 / 高延迟跳点 / 跨运营商丢包. NOT for: ICMP 被防火墙阻断 (用 mtr 更鲁棒) / 单点延迟 (probe_tcp).",
		Class:       skill.ClassSafe,
		Category:    "network",
		Params: skill.ParamSchema{
			{Name: "host", Param: skill.Param{Type: "string", Required: true, Desc: "目标主机 IP 或域名"}},
			{Name: "max_hops", Param: skill.Param{Type: "int", Default: 30, Desc: "最大跳数, 默认 30"}},
			{Name: "timeout_sec", Param: skill.Param{Type: "int", Default: 30, Desc: "总超时秒数, 默认 30"}},
		},
		ResultPreview: "{host, hops: [{hop, host, rtt_ms}], total_hops, truncated, error?}",
	}
}

type tracerouteParams struct {
	Host       string `json:"host"`
	MaxHops    int    `json:"max_hops"`
	TimeoutSec int    `json:"timeout_sec"`
}

type tracerouteHop struct {
	Hop   int     `json:"hop"`
	Host  string  `json:"host"`
	RTTMS float64 `json:"rtt_ms"`
}

type tracerouteResult struct {
	Host       string          `json:"host"`
	Hops       []tracerouteHop `json:"hops"`
	TotalHops  int             `json:"total_hops"`
	Truncated  bool            `json:"truncated"`
	SpillPath  string          `json:"spill_path,omitempty"`
	DurationMS int64           `json:"duration_ms"`
	Error      string          `json:"error,omitempty"`
}

func (Traceroute) Execute(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var p tracerouteParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("traceroute: decode params: %w", err)
		}
	}
	if p.Host == "" {
		return nil, fmt.Errorf("traceroute: host required")
	}
	if p.MaxHops <= 0 {
		p.MaxHops = 30
	}
	if p.TimeoutSec <= 0 {
		p.TimeoutSec = 30
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSec)*time.Second)
	defer cancel()

	start := time.Now()
	// 优先 traceroute, 退化到 tracepath (busybox).
	bin, args := pickTracerouteBin(p.MaxHops, p.Host)
	cmd := exec.CommandContext(ctx, bin, args...)
	output, err := cmd.CombinedOutput()
	res := tracerouteResult{
		Host:       p.Host,
		DurationMS: time.Since(start).Milliseconds(),
	}
	if err != nil {
		// traceroute 经常返回非 0 (部分跳点不可达), 不一定是错.
		res.Error = err.Error()
	}
	parseTraceroute(string(output), &res)
	if len(res.Hops) == 0 && res.Error != "" {
		return json.Marshal(res)
	}
	return json.Marshal(res)
}

// pickTracerouteBin 选 traceroute, 退化 tracepath.
func pickTracerouteBin(maxHops int, host string) (string, []string) {
	if _, err := exec.LookPath("traceroute"); err == nil {
		return "traceroute", []string{"-n", "-w", "2", "-q", "1", "-m", strconv.Itoa(maxHops), host}
	}
	return "tracepath", []string{"-n", "-m", strconv.Itoa(maxHops), host}
}

// parseTraceroute 解析 traceroute / tracepath 输出为结构化 hops.
func parseTraceroute(out string, res *tracerouteResult) {
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// 格式: " 5  10.0.0.1  1.123 ms"
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		hop, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		entry := tracerouteHop{Hop: hop, Host: fields[1]}
		// 找 RTT: 支持 "1.123 ms" 和 "1.123ms" 两种格式.
		for i := 2; i < len(fields); i++ {
			f := fields[i]
			candidate := f
			// 形式: 数值 + 空格 + ms
			if i+1 < len(fields) && fields[i+1] == "ms" {
				candidate = f + "ms"
			}
			if strings.HasSuffix(candidate, "ms") {
				rt, _ := strconv.ParseFloat(strings.TrimSuffix(candidate, "ms"), 64)
				entry.RTTMS = rt
				break
			}
		}
		res.Hops = append(res.Hops, entry)
	}
	res.TotalHops = len(res.Hops)
}
