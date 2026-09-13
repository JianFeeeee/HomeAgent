/**
 * 聊天流状态源（单例）。
 *
 * 从 pages/ChatPage.ets 抽出：消息数组、分页游标、"哪几条是新消息"、
 * SSE 连接与历史拉取都属于同一个状态机；页面只剩渲染与输入。
 *
 * 为什么数组不进 AppStorage：StatusStore 已经踩过一次 —— 数组同步语义不可靠。
 * 这里沿用同一套做法：数组留在 store 内部，标量走 AppStorage 广播，
 * 另加一个自增版本号 K_CHAT_REV 通知订阅组件"重取一次快照"。
 *
 * 订阅组件的接法（见 components/ChatStream.ets）：
 *   @StorageProp(K_CHAT_REV) @Watch('onRev') private rev: number = 0;
 *   onRev(): void { this.messages = chatStore.messages(); }
 * ForEach 拿到的仍然是"每次刷新一个新数组引用"，与拆分前
 * （this.messages = this.messages.slice()）的渲染语义完全一致。
 *
 * SSE 事件的翻译在 common/ChatSse.ets：本类实现它的 ChatStreamSink 接口，
 * 依赖方向只有"ChatStore → ChatSse"一条，不构成循环。
 */

import { ChatMessage, ToolCallInfo, ChatAttachment } from '../model/Model';
import { SseClient, SseEvent } from './SseClient';
import { connStore } from './ConnStore';
import { apiClient } from './ApiClient';
import { CHAT_PAGE_SIZE } from './Constants';
import { ParsedHistory, parseHistoryPayload } from './ChatHistory';
import { applyChatSse, ChatStreamSink } from './ChatSse';

// ===== AppStorage 键：页面/聊天流/输入区共用 =====
export const K_CHAT_REV: string = 'chatRev';
export const K_CHAT_SCROLL_REV: string = 'chatScrollRev';
export const K_CHAT_LOADING: string = 'chatBusy';
export const K_CHAT_STAGE: string = 'chatStageText';
export const K_CHAT_CONNECTED: string = 'chatSseUp';

const SSE_RECONNECT_MS: number = 5000;

class ChatStore implements ChatStreamSink {
  private msgs: ChatMessage[] = [];
  private nextId: number = 1;
  private refreshTimer: number = -1;
  private reconnectTimer: number = -1;
  /** 分页历史：当前已加载消息在服务端全量中的起始下标 */
  private offset: number = 0;
  /** 是否还有更早历史可向上加载 */
  private hasEarlier: boolean = false;
  private loadingOlder: boolean = false;
  /** 正在为新消息播入场动画的 id */
  private newIds: number[] = [];
  // SSE 正在为当前轮次推送内容时置 true，阻止 POST 响应重复创建消息
  private sseActiveForTurn: boolean = false;
  private sse: SseClient = new SseClient();

  init(): void {
    AppStorage.setOrCreate<boolean>(K_CHAT_LOADING, false);
    AppStorage.setOrCreate<string>(K_CHAT_STAGE, '');
    AppStorage.setOrCreate<boolean>(K_CHAT_CONNECTED, false);
    AppStorage.setOrCreate<number>(K_CHAT_REV, 0);
    AppStorage.setOrCreate<number>(K_CHAT_SCROLL_REV, 0);
  }

  // ===================== 读取 =====================

  messages(): ChatMessage[] {
    return this.msgs;
  }

  findMessage(id: number): ChatMessage | undefined {
    for (let i = 0; i < this.msgs.length; i++) {
      if (this.msgs[i].id === id) {
        return this.msgs[i];
      }
    }
    return undefined;
  }

  lastMessage(): ChatMessage | null {
    if (this.msgs.length === 0) {
      return null;
    }
    return this.msgs[this.msgs.length - 1];
  }

  isFresh(id: number): boolean {
    return this.newIds.indexOf(id) >= 0;
  }

  hasMore(): boolean {
    return this.hasEarlier;
  }

  isFetchingOlder(): boolean {
    return this.loadingOlder;
  }

  isLoading(): boolean {
    return AppStorage.get<boolean>(K_CHAT_LOADING) ?? false;
  }

  setLoading(v: boolean): void {
    AppStorage.set<boolean>(K_CHAT_LOADING, v);
  }

  setStage(s: string): void {
    AppStorage.set<string>(K_CHAT_STAGE, s);
  }

  setConnected(v: boolean): void {
    AppStorage.set<boolean>(K_CHAT_CONNECTED, v);
  }

  sseActive(): boolean {
    return this.sseActiveForTurn;
  }

  setSseActive(v: boolean): void {
    this.sseActiveForTurn = v;
  }

  /** 聊天流里最后一个带附件的消息：宽屏进入 Split 时用它填充右栏 */
  latestAttachment(): ChatAttachment | undefined {
    for (let i = this.msgs.length - 1; i >= 0; i--) {
      const a: ChatAttachment | undefined = this.msgs[i].attachment;
      if (a !== undefined) {
        return a;
      }
    }
    return undefined;
  }

  // ===================== 列表变更 =====================

  allocId(): number {
    return this.nextId++;
  }

  /** 追加一条消息并播入场动画 */
  pushNew(msg: ChatMessage): void {
    this.msgs.push(msg);
    this.markNew(msg.id);
  }

  /** 标记新消息，触发入场动画 */
  markNew(msgId: number): void {
    const arr: number[] = this.newIds.slice();
    arr.push(msgId);
    this.newIds = arr;
    // 这里不切片也不广播：与拆分前一致，入场动画的开场交给紧随其后的
    // refresh()（50ms 防抖后换新数组引用）那一次一起触发。
    setTimeout(() => {
      const idx: number = this.newIds.indexOf(msgId);
      if (idx >= 0) {
        const updated: number[] = this.newIds.slice();
        updated.splice(idx, 1);
        this.newIds = updated;
        this.forceRefresh();
      }
    }, 250);
  }

  /**
   * 取当前助手消息里名为 name 的未完成工具卡，没有就建一张。
   * 顺带保证一定存在一条"未定稿的助手消息"来挂这些卡。
   */
  ensureToolCall(name: string): ToolCallInfo {
    const last: ChatMessage = this.ensureAssistant();
    if (last.toolCalls === undefined) {
      last.toolCalls = [];
    }
    for (let i = 0; i < last.toolCalls.length; i++) {
      const t: ToolCallInfo = last.toolCalls[i];
      // 只复用"仍在执行"的同名卡：同一轮里同名工具被多次调用时，
      // 已完成的那张不能被后来的调用覆盖。
      const st: string | undefined = t.status;
      if (t.name === name && (st === undefined || st.length === 0 || st === 'running')) {
        return t;
      }
    }
    const created: ToolCallInfo = { name: name, args: '' };
    last.toolCalls.push(created);
    return created;
  }

  /**
   * 保证存在一条"未定稿的助手消息"，返回它。
   * 聚合 reasoning 可能先于任何 delta 到达（非流式后端就只有这一条），
   * 此时还没有"未完成的助手消息"，必须新建一条，否则内容直接丢失。
   */
  ensureAssistant(): ChatMessage {
    const last: ChatMessage | null = this.lastMessage();
    if (last !== null && last.role === 'assistant' && last.isFinal !== true) {
      return last;
    }
    const msg: ChatMessage = {
      id: this.allocId(),
      role: 'assistant',
      content: '',
      isStreaming: true,
      isFinal: false,
    };
    this.pushNew(msg);
    return msg;
  }

  /** 流式/高频变更的通知信号：让 ChatStream 滚到底 */
  requestScroll(): void {
    const cur: number = AppStorage.get<number>(K_CHAT_SCROLL_REV) ?? 0;
    AppStorage.set<number>(K_CHAT_SCROLL_REV, cur + 1);
  }

  /** 防抖刷新：合并高频 SSE delta，最多 ~20fps */
  refresh(): void {
    if (this.refreshTimer >= 0) {
      return;
    }
    this.refreshTimer = setTimeout(() => {
      this.refreshTimer = -1;
      this.msgs = this.msgs.slice();
      this.bump();
    }, 50);
  }

  /** 强制立即刷新（用于状态切换等需要即时响应的场景） */
  forceRefresh(): void {
    if (this.refreshTimer >= 0) {
      clearTimeout(this.refreshTimer);
      this.refreshTimer = -1;
    }
    this.msgs = this.msgs.slice();
    this.bump();
  }

  /** 明细数组不进 AppStorage，用一个自增版本号触发订阅组件重取 */
  bump(): void {
    const cur: number = AppStorage.get<number>(K_CHAT_REV) ?? 0;
    AppStorage.set<number>(K_CHAT_REV, cur + 1);
  }

  cancelRefresh(): void {
    if (this.refreshTimer >= 0) {
      clearTimeout(this.refreshTimer);
      this.refreshTimer = -1;
    }
  }

  // ===================== 折叠开关 =====================

  reasoningOpen(msgId: number): boolean {
    const m: ChatMessage | undefined = this.findMessage(msgId);
    return m !== undefined && m.reasoningOpen === true;
  }

  setReasoningOpen(msgId: number, open: boolean): void {
    const m: ChatMessage | undefined = this.findMessage(msgId);
    if (m !== undefined) {
      m.reasoningOpen = open;
    }
  }

  toolOpen(msgId: number, index: number): boolean {
    const m: ChatMessage | undefined = this.findMessage(msgId);
    if (m === undefined || m.toolCalls === undefined || index >= m.toolCalls.length) {
      return false;
    }
    return m.toolCalls[index].open === true;
  }

  setToolOpen(msgId: number, index: number, open: boolean): void {
    const m: ChatMessage | undefined = this.findMessage(msgId);
    if (m !== undefined && m.toolCalls !== undefined && index < m.toolCalls.length) {
      m.toolCalls[index].open = open;
    }
  }

  // ===================== 历史 =====================

  private parseHistory(obj: Record<string, Object>): ParsedHistory {
    return parseHistoryPayload(obj, () => this.allocId());
  }

  async loadHistory(): Promise<void> {
    try {
      // 分段懒加载：首屏只拉最新 CHAT_PAGE_SIZE 条，向上滚动触顶再拉更早的。
      const resp = await apiClient.getWithTimeout('/chat/history?limit=' + CHAT_PAGE_SIZE, 8000);
      const obj: Record<string, Object> = JSON.parse(resp.body) as Record<string, Object>;
      const parsed: ParsedHistory = this.parseHistory(obj);
      if (parsed.msgs.length === 0) {
        return;
      }
      this.msgs = parsed.msgs;
      this.offset = parsed.offset;
      this.hasEarlier = parsed.hasMore;
      this.forceRefresh();
      this.requestScroll();
    } catch (e) {
      // ignore history load failure
    }
  }

  /**
   * 向上翻页：拉 offset 之前的更早一页，前置到 messages 头部并保持滚动位置。
   * 触顶（yOffset 接近 0）且有更早历史时由 onDidScroll 触发。
   */
  async loadOlder(): Promise<void> {
    if (this.loadingOlder || !this.hasEarlier) {
      return;
    }
    this.loadingOlder = true;
    try {
      const before: number = this.offset;
      if (before <= 0) {
        this.hasEarlier = false;
        return;
      }
      const resp = await apiClient.getWithTimeout(
        '/chat/history?limit=' + CHAT_PAGE_SIZE + '&before=' + before, 8000);
      const obj: Record<string, Object> = JSON.parse(resp.body) as Record<string, Object>;
      const parsed: ParsedHistory = this.parseHistory(obj);
      if (parsed.msgs.length === 0) {
        this.hasEarlier = false;
        return;
      }
      // 前置插入新页（更早的在前），追加到当前列表头部；id 用新分配的避免与新消息撞号
      this.msgs = parsed.msgs.concat(this.msgs);
      this.offset = parsed.offset;
      this.hasEarlier = parsed.hasMore;
      this.bump();
    } catch (e) {
      // 失败保留 hasEarlier，允许下次滚动重试
    } finally {
      this.loadingOlder = false;
    }
  }

  /** sync_required：断线重连后服务端要求补拉历史 */
  reloadHistory(): void {
    this.loadHistory();
  }

  // ===================== SSE 连接 =====================

  connect(): void {
    const cur = connStore.getCurrentConnection();
    if (cur === null) {
      return;
    }
    this.sse.close();
    this.sse.connect(cur, '/chat/events',
      (ev: SseEvent) => {
        applyChatSse(ev, this);
      },
      () => {
        this.setConnected(false);
        this.scheduleReconnect();
      },
      () => {
        this.setConnected(true);
      }).catch(() => {
      this.setConnected(false);
      this.scheduleReconnect();
    });
  }

  scheduleReconnect(): void {
    if (this.reconnectTimer >= 0) {
      return;
    }
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = -1;
      this.connect();
    }, SSE_RECONNECT_MS);
  }

  cancelReconnect(): void {
    if (this.reconnectTimer >= 0) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = -1;
    }
  }

  /** 页面消失：断线、停表，避免后台空转 */
  disconnect(): void {
    this.cancelReconnect();
    this.sse.close();
    this.cancelRefresh();
  }
}

export const chatStore: ChatStore = new ChatStore();
