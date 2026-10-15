import { describe, expect, it } from 'vitest';
import { deriveSessionContext, mentionRoute } from './sessionContext';
import type { ChatMessage } from '@/api/chat';

function userMsg(id: string, content: string): ChatMessage {
  return { id, role: 'user', content };
}

describe('deriveSessionContext · mentions', () => {
  it('从 user 消息解析结构化 mention 并按 type+id 去重,携带来源', () => {
    const messages = [
      userMsg('m1', '看看 @device:7(核心网关) 的情况'),
      userMsg('m2', '再看 @device:7(核心网关) 和 @incident:42(磁盘满)'),
    ];
    const { mentions } = deriveSessionContext(messages);
    expect(mentions).toHaveLength(2);
    expect(mentions[0]).toMatchObject({
      type: 'device', id: '7', label: '核心网关', messageId: 'm1', sourceIndex: 0,
    });
    expect(mentions[1]).toMatchObject({ type: 'incident', id: '42', messageId: 'm2', sourceIndex: 1 });
  });

  it('忽略 type 不在 device/incident/rule/file 之内的伪 token', () => {
    const messages = [userMsg('m1', '伪装 @user:1(张三) 与 @device:7(核心网关)')];
    const { mentions } = deriveSessionContext(messages);
    expect(mentions).toHaveLength(1);
    expect(mentions[0]).toMatchObject({ type: 'device', id: '7' });
  });

  it('纯文本消息、邮件地址不产生条目', () => {
    const messages = [userMsg('m1', 'hello world, mail me at ops@example.com please')];
    expect(deriveSessionContext(messages).mentions).toEqual([]);
  });

  it('非 user 角色不参与 mention 解析', () => {
    const messages: ChatMessage[] = [{ id: 'a1', role: 'assistant', content: '@device:7(核心网关)' }];
    expect(deriveSessionContext(messages).mentions).toEqual([]);
  });

  it('空会话返回空结构', () => {
    expect(deriveSessionContext([])).toEqual({ mentions: [], knowledgeRefs: [] });
  });
});

function assistantWithTools(id: string, tool_calls: ChatMessage['tool_calls']): ChatMessage {
  return { id, role: 'assistant', content: '', tool_calls };
}
function toolCard(id: string, tool_call: ChatMessage['tool_call']): ChatMessage {
  return { id, role: 'tool', kind: 'tool_card', tool_call };
}

describe('deriveSessionContext · knowledgeRefs', () => {
  it('assistant tool_calls 中知识类工具计入,携带 status/duration/来源', () => {
    const messages = [
      assistantWithTools('a1', [
        { name: 'query_knowledge', status: 'success', duration_ms: 120, arguments: { q: 'nginx 502' } },
      ]),
    ];
    const { knowledgeRefs } = deriveSessionContext(messages);
    expect(knowledgeRefs).toHaveLength(1);
    expect(knowledgeRefs[0]).toMatchObject({
      name: 'query_knowledge', status: 'success', durationMs: 120, messageId: 'a1', sourceIndex: 0,
    });
  });

  it('按工具类别映射识别(web_search / *source*),而非逐个名称白名单', () => {
    const messages = [
      assistantWithTools('a1', [
        { name: 'web_search', status: 'success' },
        { name: 'fetch_knowledge_source', status: 'success' },
      ]),
    ];
    expect(deriveSessionContext(messages).knowledgeRefs.map((k) => k.name))
      .toEqual(['web_search', 'fetch_knowledge_source']);
  });

  it('非知识类工具(设备/观测类)不计入', () => {
    const messages = [
      assistantWithTools('a1', [
        { name: 'restart_service', status: 'success' },
        { name: 'query_promql', status: 'success' },
      ]),
    ];
    expect(deriveSessionContext(messages).knowledgeRefs).toEqual([]);
  });

  it('合成 tool_card 行的 tool_call 计入,保留 toolCallId 与 error 状态', () => {
    const messages = [toolCard('c1', { id: 'tc1', name: 'query_knowledge', status: 'error', duration_ms: 8 })];
    const refs = deriveSessionContext(messages).knowledgeRefs;
    expect(refs).toHaveLength(1);
    expect(refs[0]).toMatchObject({ name: 'query_knowledge', status: 'error', toolCallId: 'tc1', messageId: 'c1' });
  });

  it('按工具名+关键参数去重', () => {
    const messages = [
      userMsg('m1', 'x'),
      assistantWithTools('a1', [{ name: 'query_knowledge', status: 'success', arguments: { q: 'a' } }]),
      assistantWithTools('a2', [{ name: 'query_knowledge', status: 'success', arguments: { q: 'a' } }]),
      assistantWithTools('a3', [{ name: 'query_knowledge', status: 'success', arguments: { q: 'b' } }]),
    ];
    expect(deriveSessionContext(messages).knowledgeRefs).toHaveLength(2);
  });
});

describe('mentionRoute · 既有详情路由映射', () => {
  it('device → 设备详情,incident → 事件详情', () => {
    expect(mentionRoute('device', '7')).toBe('/devices/7');
    expect(mentionRoute('incident', '42')).toBe('/alerts/incidents/42');
  });
  it('rule / file 落到既有列表页(SPA 无逐 id 详情路由)', () => {
    expect(mentionRoute('rule', '9')).toBe('/alerts/rules');
    expect(mentionRoute('file', 'x')).toBe('/logs');
  });
});
