/**
 * /chat/history 响应解析（无 UI 依赖）。
 *
 * 从 pages/ChatPage.ets 抽出：首屏与向上翻页共用同一套解析口径，
 * 消息 id 由调用方提供的分配器给出（页面自己维护 id 计数器）。
 */

import { ChatMessage, ToolCallInfo, ChatAttachment } from '../model/Model';
import { parseAttachment } from './AttachmentMeta';
import { stringifyField } from './ChatFormat';

/** 分页历史解析结果：消息列表 + 服务端分页元数据 */
export interface ParsedHistory {
  msgs: ChatMessage[];
  /** 本页首条在服务端全量历史中的下标，作为下次向上翻页的 before 游标 */
  offset: number;
  /** 服务端是否还有更早的历史 */
  hasMore: boolean;
}

/** 解析后端 /chat/history 的响应体（含分页元数据），供首屏与翻页复用。 */
export function parseHistoryPayload(
  obj: Record<string, Object>, alloc: () => number): ParsedHistory {
  const rawList: Object | undefined = obj['messages'] as Object | undefined;
  if (rawList === undefined || rawList === null) {
    return { msgs: [], offset: 0, hasMore: false };
  }
  const arr: Object[] = rawList as Object[];
  const msgs: ChatMessage[] = [];
  for (let i = 0; i < arr.length; i++) {
    const item: Record<string, Object> = arr[i] as Record<string, Object>;
    const role: string = item['role'] as string ?? '';
    const content: string = item['content'] as string ?? '';
    const att: ChatAttachment | undefined = parseAttachment(item['attachment']);
    // 纯附件消息 content 可能为空，不能再按"无内容就丢弃"处理
    if (role.length === 0 || (content.length === 0 && att === undefined)) {
      continue;
    }
    const msg: ChatMessage = {
      id: alloc(),
      role: role,
      content: content,
      isFinal: true,
    };
    if (att !== undefined) {
      msg.attachment = att;
    }
    // 后端 handler.go 保证 history 不裁剪 reasoning_content / tool_calls，
    // 这里必须还原，否则刷新后思考与工具卡就凭空消失。
    const rc: string = item['reasoning_content'] as string ?? '';
    if (rc.length > 0) {
      msg.reasoningContent = rc;
    }
    const tcs: ToolCallInfo[] | undefined = parseHistoryToolCalls(item['tool_calls']);
    if (tcs !== undefined) {
      msg.toolCalls = tcs;
    }
    // 渠道/设备来源：后端 ChatMsg.source，用于区分 channel_output 等非 webui 消息
    const src: string = item['source'] as string ?? '';
    if (src.length > 0) {
      msg.source = src;
    }
    msgs.push(msg);
  }
  const offset: number = typeof obj['offset'] === 'number' ? obj['offset'] as number : 0;
  const hasMore: boolean = obj['has_more'] === true;
  return { msgs: msgs, offset: offset, hasMore: hasMore };
}

/** 后端 tool_calls 条目带 tool 和 name 两份；args/result 可能是对象也可能是字符串。 */
export function parseHistoryToolCalls(raw: Object | undefined): ToolCallInfo[] | undefined {
  if (raw === undefined || raw === null) {
    return undefined;
  }
  const arr: Object[] = raw as Object[];
  if (arr.length === 0) {
    return undefined;
  }
  const tcs: ToolCallInfo[] = [];
  for (let i = 0; i < arr.length; i++) {
    const item: Record<string, Object> = arr[i] as Record<string, Object>;
    const name: string = (item['tool'] as string ?? '') || (item['name'] as string ?? '');
    if (name.length === 0) {
      continue;
    }
    const tc: ToolCallInfo = {
      name: name,
      args: stringifyField(item['args']),
      result: stringifyField(item['result']),
      status: item['status'] as string ?? undefined,
      plugin: item['plugin'] as string ?? undefined,
    };
    tcs.push(tc);
  }
  return tcs.length > 0 ? tcs : undefined;
}
