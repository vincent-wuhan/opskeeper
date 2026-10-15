import { describe, expect, it } from 'vitest';
import type { Channel } from '@/api/alerts';
import {
  DAILY_BRIEF_CRON,
  DAILY_BRIEF_PROMPT,
  dailyBriefSeed,
  pickFeishuChannelIDs,
} from './dailyBrief';

function channel(partial: Partial<Channel>): Channel {
  return { id: 0, name: '', type: 'feishu', enabled: true, created_at: '', updated_at: '', ...partial };
}

describe('pickFeishuChannelIDs', () => {
  it('只取启用中的飞书渠道 id', () => {
    const chans = [
      channel({ id: 7, type: 'feishu', enabled: true }),
      channel({ id: 8, type: 'feishu', enabled: false }),
      channel({ id: 9, type: 'telegram', enabled: true }),
      channel({ id: 10, type: 'feishu', enabled: true }),
    ];
    expect(pickFeishuChannelIDs(chans)).toEqual([7, 10]);
  });

  it('无飞书渠道 → 空数组(降级为仅平台内生成)', () => {
    expect(pickFeishuChannelIDs([channel({ id: 1, type: 'slack' })])).toEqual([]);
  });
});

describe('dailyBriefSeed', () => {
  it('预填 daily + 09:00 + 飞书渠道 + 三节模板', () => {
    const seed = dailyBriefSeed([channel({ id: 7, type: 'feishu', enabled: true })], '每日值班简报');
    expect(seed.name).toBe('每日值班简报');
    expect(seed.kind).toBe('daily');
    expect(seed.cron_spec).toBe(DAILY_BRIEF_CRON);
    expect(seed.cron_spec).toBe('0 9 * * *');
    expect(seed.channel_ids).toEqual([7]);
    expect(seed.prompt_override).toBe(DAILY_BRIEF_PROMPT);
  });

  it('无飞书渠道 → channel_ids 为空', () => {
    expect(dailyBriefSeed([], 'x').channel_ids).toEqual([]);
  });

  it('三节模板含三节标题且不引入额外章节', () => {
    for (const marker of ['昨夜事件摘要', '待审批项', '告警趋势与今日关注']) {
      expect(DAILY_BRIEF_PROMPT).toContain(marker);
    }
    // 无源两节不得出现在模板里(spec「无源小节不渲染」)。
    expect(DAILY_BRIEF_PROMPT).not.toContain('结晶自愈');
    expect(DAILY_BRIEF_PROMPT).not.toContain('今日值班');
  });
});
