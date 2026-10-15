import type { Channel } from '@/api/alerts';
import type { ScheduleInput } from '@/api/reports';

// 每日值班简报预设 —— 一次性表单预填的单一来源。与 UI 解耦的纯函数,便于单测。
// 预设不是后端存储:创建后走既有 schedule 编辑界面修改任何参数。

export const DAILY_BRIEF_NAME_ZH = '每日值班简报';
export const DAILY_BRIEF_NAME_EN = 'Daily on-call brief';

// 09:00 每日(cron 触发点同时保证「不在夜间推送」);后端 CronSpecForKind('daily')
// 的默认值也是它(biz/report/cron.go:41),此处显式预填以便表单可读。
export const DAILY_BRIEF_CRON = '0 9 * * *';

// 三节模板(Design Doc §3.1),经 schedule prompt_override 注入。只约束三节文字,
// 全部映射既有 Content 字段,不引入新 schema。persona 不在此设:后端 CreateSchedule
// 在 agent_persona 为空时默认 reporter(biz/report/usecase.go:152),ScheduleInput 也无该字段。
export const DAILY_BRIEF_PROMPT = [
  '你是值班简报生成场景。本次报告为「每日值班简报」,固定三节:',
  '1. 昨夜事件摘要:基于 KeyIncidents,昨日至今(统计窗口=cron 触发点回看 24h)',
  '2. 待审批项:当前 pending 审批数与双签进度(使用注入事实),辅以周期计数',
  '3. 告警趋势与今日关注:基于 AlertCounts 与 Advice',
  '约束:数字只使用注入事实,无来源留空;当日无事件且无待审批时 Hero 呈现「平静」;',
  '不执行任何变更,只汇总与建议;输出遵循 ContentJSON schema,只输出 JSON。',
].join('\n');

// pickFeishuChannelIDs 取默认推送用的飞书渠道 id;无飞书渠道时返回 []
// (回退为仅平台内生成,符合降级契约)。
export function pickFeishuChannelIDs(channels: Channel[]): number[] {
  return channels.filter((c) => c.type === 'feishu' && c.enabled).map((c) => c.id);
}

// dailyBriefSeed 构造「创建每日值班简报」的预填值(注入既有 ScheduleForm)。
export function dailyBriefSeed(channels: Channel[], name: string): Partial<ScheduleInput> {
  return {
    name,
    kind: 'daily',
    cron_spec: DAILY_BRIEF_CRON,
    channel_ids: pickFeishuChannelIDs(channels),
    prompt_override: DAILY_BRIEF_PROMPT,
  };
}
