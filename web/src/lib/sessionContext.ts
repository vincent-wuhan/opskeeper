// sessionContext — 从「已渲染的消息流」反推本会话的运维上下文
// (被 @提及的平台对象 + 知识库引用)。纯函数:零网络、零 UI 依赖、零 store 依赖。
//
// 数据来源即渲染中的 messages[]:mention token 存在于 user 消息文本内
// ("@{type}:{id}({label})",由 ChatInput 写入),工具调用存在于
// assistant 的 tool_calls[] 或流式合成的 tool_card 行。面板是「屏幕上
// 已有内容」的只读视图,不发起任何请求。
import type { ChatMessage, MentionType, ToolCallSummary } from '@/api/chat';
import { toolGroupKey } from './toolSkill';

export type MentionRef = {
  type: MentionType;
  id: string;
  label: string;
  /** 来源消息在 messages[] 中的 0 基下标(用于「定位来源消息」)。 */
  sourceIndex: number;
  /** 来源消息 id。 */
  messageId: string;
};

export type KnowledgeRef = {
  name: string;
  status: 'pending' | 'success' | 'error' | 'timeout';
  durationMs?: number;
  sourceIndex: number;
  messageId: string;
  toolCallId?: string;
};

export type SessionContext = {
  mentions: MentionRef[];
  knowledgeRefs: KnowledgeRef[];
};

const MENTION_TYPES: readonly MentionType[] = ['device', 'incident', 'rule', 'file'];

// MENTION_RE 匹配结构化 mention token。组1=type,组2=id,组3=label。
// 要求 '@' 之前是行首或空白,避免把 user@host 这类邮件地址当作提及;
// type 组仅收 [a-z]+,非法的 'user'/'foo' 交由白名单过滤。
const MENTION_RE = /(?:^|\s)@([a-z]+):([^\s()]+)\(([^)]*)\)/g;

export function deriveSessionContext(messages: ChatMessage[]): SessionContext {
  return {
    mentions: deriveMentions(messages),
    knowledgeRefs: deriveKnowledgeRefs(messages),
  };
}

function deriveMentions(messages: ChatMessage[]): MentionRef[] {
  const out: MentionRef[] = [];
  const seen = new Set<string>();
  messages.forEach((m, index) => {
    if (m.role !== 'user' || !m.content) return;
    // matchAll 使用正则的内部克隆,不依赖共享 lastIndex,可安全复用 MENTION_RE。
    for (const match of m.content.matchAll(MENTION_RE)) {
      const type = match[1] as MentionType;
      if (!MENTION_TYPES.includes(type)) continue; // 非法/伪造 type 一律忽略,防误报
      const id = match[2];
      const key = `${type}:${id}`;
      if (seen.has(key)) continue; // 按 type+id 去重
      seen.add(key);
      out.push({ type, id, label: match[3], sourceIndex: index, messageId: m.id });
    }
  });
  return out;
}

// toolCallsOf 展开一条消息携带的全部 ToolCallSummary —— assistant 气泡的
// tool_calls[] 与流式合成的 tool_card 行(tool_call)统一取齐。
function toolCallsOf(m: ChatMessage): ToolCallSummary[] {
  const out: ToolCallSummary[] = [];
  if (m.tool_calls) out.push(...m.tool_calls);
  if (m.tool_call) out.push(m.tool_call);
  return out;
}

function deriveKnowledgeRefs(messages: ChatMessage[]): KnowledgeRef[] {
  const out: KnowledgeRef[] = [];
  const seen = new Set<string>();
  messages.forEach((m, index) => {
    for (const tc of toolCallsOf(m)) {
      // 类别映射而非白名单:复用 toolGroupKey,'knowledge' 类新工具自动纳入。
      if (toolGroupKey(tc.name) !== 'knowledge') continue;
      const key = `${tc.name}|${stableArgs(tc.arguments)}`;
      if (seen.has(key)) continue; // 按 工具名+关键参数 去重,首次出现胜出
      seen.add(key);
      out.push({
        name: tc.name,
        status: tc.status,
        durationMs: tc.duration_ms,
        sourceIndex: index,
        messageId: m.id,
        toolCallId: tc.id,
      });
    }
  });
  return out;
}

// stableArgs 序列化工具参数用于去重。参数来自服务端 SSE 帧(已是 JSON),
// 同一调用形状键序稳定;缺失参数一律折成 "null",两条都缺席时可去重。
function stableArgs(args: unknown): string {
  try {
    return JSON.stringify(args ?? null);
  } catch {
    return '';
  }
}

// mentionRoute 把一个提及对象映射到 SPA 中既有的详情路由。rule / file
// 无逐 id 详情页,落到最接近的既有列表页。
export function mentionRoute(type: MentionType, id: string): string {
  switch (type) {
    case 'device':
      return `/devices/${encodeURIComponent(id)}`;
    case 'incident':
      return `/alerts/incidents/${encodeURIComponent(id)}`;
    case 'rule':
      return '/alerts/rules';
    case 'file':
      return '/logs';
  }
}
