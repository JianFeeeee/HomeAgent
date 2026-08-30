import { apiClient, ApiError } from '../common/ApiClient';
import { userMessage, isTimeout } from '../common/UserError';
import { SseClient, SseEvent } from '../common/SseClient';
import { connStore } from '../common/ConnStore';
import { navBar } from '../common/NavBarController';
import { registerNavStack, unregisterNavStack } from '../common/NavStackRegistry';
import { ChatMessage, ToolCallInfo, HistoryMessage, ChatAttachment } from '../model/Model';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE } from '../common/Constants';
import { RADIUS_MD, RADIUS_PILL, RADIUS_SM } from '../common/Constants';
import { WIDE_NAV_BAR_WIDTH, WIDE_MIN_CONTENT } from '../common/Constants';
import { CHAT_PAGE_SIZE } from '../common/Constants';
import { ANIM_FAST, ANIM_NORMAL, ANIM_ENTER } from '../common/Constants';
import { MotionBase } from '../components/MotionBase';
import { MarkdownView } from '../components/MarkdownView';
import { PageTopBar, NavFloatOverlay, NavFloatRow, FloatIconButton } from '../components/PageTopBar';
import { SubPageLayer, markSubPageOpen, subPageParam } from '../components/SubPage';
import {
  AttachmentCard, AttachmentDetailContent, parseAttachment, attachmentFromChannelOutput,
  fileNameOf, formatBytes,
} from '../components/Attachment';
import { http } from '@kit.NetworkKit';
import { hilog } from '@kit.PerformanceAnalysisKit';
import { picker, fileIo } from '@kit.CoreFileKit';
import { common } from '@kit.AbilityKit';
import { MeasureOptions } from '@ohos.measure';

const SSE_RECONNECT_MS: number = 5000;

/** 输入框字号与内边距：文字测量必须和 TextArea 的实际排版参数一致 */
const INPUT_FONT_SIZE: number = 14;
const INPUT_INNER_PAD: number = 16;
/** 单行态左右让位：左边加号 42+8，右边发送键 44+8 */
const INPUT_LEFT_GAP: number = 50;
const INPUT_RIGHT_GAP: number = 52;

/** 二级页面标识：附件（图片/文件）详情 */
const SUB_NONE: string = '';
const SUB_ATTACHMENT: string = 'attachment';

interface SendChatBody {
  message: string;
  client_msg_id: string;
  /** 非空时后端编码 source = "webui/<device_id>"，agent 可见来源设备 */
  device_id?: string;
  device_name?: string;
}

/** 分页历史解析结果：消息列表 + 服务端分页元数据 */
interface ParsedHistory {
  msgs: ChatMessage[];
  /** 本页首条在服务端全量历史中的下标，作为下次向上翻页的 before 游标 */
  offset: number;
  /** 服务端是否还有更早的历史 */
  hasMore: boolean;
}

/**
 * 由文件名后缀推断 Content-Type。
 * 后端按 multipart 部件的 Content-Type 判定 image/file，
 * 给错会让图片被当成普通文件（缩略图就没了）。
 */
function mimeOf(name: string, isImage: boolean): string {
  const i: number = name.lastIndexOf('.');
  const ext: string = i >= 0 ? name.substring(i + 1).toLowerCase() : '';
  if (ext === 'png') {
    return 'image/png';
  }
  if (ext === 'jpg' || ext === 'jpeg') {
    return 'image/jpeg';
  }
  if (ext === 'webp') {
    return 'image/webp';
  }
  if (ext === 'gif') {
    return 'image/gif';
  }
  if (ext === 'bmp') {
    return 'image/bmp';
  }
  if (ext === 'heic' || ext === 'heif') {
    return 'image/heic';
  }
  if (isImage) {
    return 'image/jpeg';
  }
  if (ext === 'pdf') {
    return 'application/pdf';
  }
  if (ext === 'txt' || ext === 'log' || ext === 'md') {
    return 'text/plain';
  }
  if (ext === 'json') {
    return 'application/json';
  }
  return 'application/octet-stream';
}

@Component
export struct ChatPage {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @StorageProp('currentTab') private currentTab: number = 0;
  /** 宽屏：左边聊天流（含底部导航栏），右边附件详情 */
  @StorageProp('isWideScreen') private isWide: boolean = false;
  @State messages: ChatMessage[] = [];
  @State inputText: string = '';
  @State chatLoading: boolean = false;
  @State chatStage: string = '';
  @State connected: boolean = false;
  /** 分页历史：当前已加载消息在服务端全量中的起始下标 */
  private chatOffset: number = 0;
  /** 是否还有更早历史可向上加载 */
  private chatHasMore: boolean = false;
  @StorageProp('navVisible') private navVisible: boolean = true;
  @State newMsgIds: number[] = [];
  @State inputMultiLine: boolean = false;
  /** 当前在右栏/二级页展示的附件；未打开时为 undefined */
  @State activeAtt: ChatAttachment | undefined = undefined;
  @State activeSub: string = SUB_NONE;
  /** 加号菜单是否展开 */
  @State attachMenuOpen: boolean = false;
  /** 待发送附件：沙箱内的本地副本路径 + 展示名 + 字节数 + 类型 */
  @State pendingName: string = '';
  @State pendingSize: number = 0;
  @State pendingIsImage: boolean = false;
  @State uploading: boolean = false;
  private pendingPath: string = '';
  private pendingMime: string = '';
  /** 底部固定行的实测宽度：用于文字测量，判断是否需要换行 */
  private inputRowWidth: number = 0;
  private navStack: NavPathStack = new NavPathStack();
  private sse: SseClient = new SseClient();
  private scroller: Scroller = new Scroller();
  private reconnectTimer: number = -1;
  private refreshTimer: number = -1;
  private autoScrolling: boolean = false;
  private navHidden: boolean = false;
  private nextMsgId: number = 1;
  // SSE 正在为当前轮次推送内容时置 true，阻止 POST 响应重复创建消息
  private sseActiveForTurn: boolean = false;

  aboutToAppear(): void {
    // 把本页导航栈登记给 Index：返回手势/三键返回按当前 Tab 精确派发到这里
    registerNavStack(0, this.navStack, () => {
      this.activeSub = SUB_NONE;
      this.activeAtt = undefined;
    });
    const cur = connStore.getCurrentConnection();
    if (cur !== null) {
      this.loadHistory();
      this.connectSSE();
    }
  }

  aboutToDisappear(): void {
    unregisterNavStack(0);
    this.cancelReconnect();
    this.sse.close();
    if (this.refreshTimer >= 0) {
      clearTimeout(this.refreshTimer);
      this.refreshTimer = -1;
    }
  }

  private allocId(): number {
    return this.nextMsgId++;
  }

  private cancelReconnect(): void {
    if (this.reconnectTimer >= 0) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = -1;
    }
  }

  private connectSSE(): void {
    const cur = connStore.getCurrentConnection();
    if (cur === null) {
      return;
    }
    this.sse.close();
    this.sse.connect(cur, '/chat/events',
      (ev: SseEvent) => {
        this.handleSSE(ev);
      },
      () => {
        this.connected = false;
        this.scheduleReconnect();
      },
      () => {
        this.connected = true;
      }).catch(() => {
        this.connected = false;
        this.scheduleReconnect();
      });
  }

  private scheduleReconnect(): void {
    if (this.reconnectTimer >= 0) {
      return;
    }
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = -1;
      this.connectSSE();
    }, SSE_RECONNECT_MS);
  }

  private async loadHistory(): Promise<void> {
    try {
      // 分段懒加载：首屏只拉最新 CHAT_PAGE_SIZE 条，向上滚动触顶再拉更早的。
      const resp = await apiClient.getWithTimeout('/chat/history?limit=' + CHAT_PAGE_SIZE, 8000);
      const obj: Record<string, Object> = JSON.parse(resp.body) as Record<string, Object>;
      const parsed: ParsedHistory = this.parseHistoryPayload(obj);
      if (parsed.msgs.length === 0) {
        return;
      }
      this.messages = parsed.msgs;
      this.chatOffset = parsed.offset;
      this.chatHasMore = parsed.hasMore;
      this.scrollToBottom();
    } catch (e) {
      // ignore history load failure
    }
  }

  /**
   * 向上翻页：拉 offset 之前的更早一页，前置到 messages 头部并保持滚动位置。
   * 触顶（yOffset 接近 0）且有更早历史时由 onDidScroll 触发。
   */
  private _loadingOlder: boolean = false;
  private async loadOlderChat(): Promise<void> {
    if (this._loadingOlder || !this.chatHasMore) {
      return;
    }
    this._loadingOlder = true;
    try {
      const before: number = this.chatOffset;
      if (before <= 0) {
        this.chatHasMore = false;
        return;
      }
      const resp = await apiClient.getWithTimeout(
        '/chat/history?limit=' + CHAT_PAGE_SIZE + '&before=' + before, 8000);
      const obj: Record<string, Object> = JSON.parse(resp.body) as Record<string, Object>;
      const parsed: ParsedHistory = this.parseHistoryPayload(obj);
      if (parsed.msgs.length === 0) {
        this.chatHasMore = false;
        return;
      }
      // 前置插入新页（更早的在前），追加到当前列表头部；id 用新分配的避免与新消息撞号
      const older: ChatMessage[] = parsed.msgs;
      this.messages = older.concat(this.messages);
      this.chatOffset = parsed.offset;
      this.chatHasMore = parsed.hasMore;
    } catch (e) {
      // 失败保留 hasMore，允许下次滚动重试
    } finally {
      this._loadingOlder = false;
    }
  }

  /** 解析后端 /chat/history 的响应体（含分页元数据），供首屏与翻页复用。 */
  private parseHistoryPayload(obj: Record<string, Object>): ParsedHistory {
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
        id: this.allocId(),
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
      const tcs: ToolCallInfo[] | undefined = this.parseHistoryToolCalls(item['tool_calls']);
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
  private parseHistoryToolCalls(raw: Object | undefined): ToolCallInfo[] | undefined {
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
        args: this.stringifyField(item['args']),
        result: this.stringifyField(item['result']),
        status: item['status'] as string ?? undefined,
        plugin: item['plugin'] as string ?? undefined,
      };
      tcs.push(tc);
    }
    return tcs.length > 0 ? tcs : undefined;
  }

  private handleSSE(ev: SseEvent): void {
    try {
      // 服务端 data 字段是完整 sdk.Event：{type, source, payload, timestamp}
      // 业务字段全部在 payload 之下，历史实现直接读顶层导致流式/思考/工具调用全部失效。
      const frame: Record<string, Object> = JSON.parse(ev.data) as Record<string, Object>;
      const inner: Object | undefined = frame['payload'];
      const payload: Record<string, Object> =
        inner !== undefined && inner !== null ? inner as Record<string, Object> : frame;
      const frameType: string = frame['type'] as string ?? '';
      const type: string = ev.event.length > 0 ? ev.event : frameType;
      // 诊断只记事件类型（内容可能含隐私，不落盘）
      hilog.debug(0x0000, 'HomeAgent', 'sse %{public}s', type);

      if (type === 'agent_output') {
        const content: string = payload['content'] as string ?? '';
        // channel_output 携带图片/文件：url/size/output_type 三个字段在 payload 顶层，
        // 它是一条独立的附件消息，不能合并进上一条文本气泡。
        const kind: string = payload['kind'] as string ?? '';
        if (kind === 'channel_output') {
          const att: ChatAttachment | undefined = attachmentFromChannelOutput(
            payload['output_type'] as string ?? '',
            payload['url'] as string ?? '',
            payload['size'] as number ?? 0);
          if (att !== undefined) {
            const amsg: ChatMessage = {
              id: this.allocId(),
              role: 'assistant',
              content: content,
              isFinal: true,
              source: 'channel',
              attachment: att,
            };
            this.messages.push(amsg);
            this.markNew(amsg.id);
            this.chatLoading = false;
            this.chatStage = '';
            this.sseActiveForTurn = false;
            this.refreshMessages();
            this.scrollToBottom();
            return;
          }
        }
        const last: ChatMessage | null = this.lastMessage();
        if (last !== null && last.role === 'assistant' && !last.isFinal) {
          last.content = content;
          last.isFinal = true;
          last.isStreaming = false;
        } else if (last !== null && last.role === 'assistant' && last.isFinal) {
          // POST 已经创建了最终消息，仅合并内容（如果有增量）
          if (content.length > last.content.length) {
            last.content = content;
          }
        } else {
          const msg: ChatMessage = {
            id: this.allocId(),
            role: 'assistant',
            content: content,
            isFinal: true,
          };
          this.messages.push(msg);
          this.markNew(msg.id);
        }
        this.chatLoading = false;
        this.chatStage = '';
        this.sseActiveForTurn = false;
        this.refreshMessages();
        this.scrollToBottom();
      } else if (type === 'reasoning') {
        const rc: string = payload['content'] as string ?? '';
        if (rc.length > 0) {
          this.chatStage = 'AI 思考中...';
          // 聚合 reasoning 可能先于任何 delta 到达（非流式后端就只有这一条），
          // 此时还没有"未完成的助手消息"，必须新建一条，否则思考内容直接丢失。
          let last: ChatMessage | null = this.lastMessage();
          if (last === null || last.role !== 'assistant' || last.isFinal === true) {
            const msg: ChatMessage = {
              id: this.allocId(),
              role: 'assistant',
              content: '',
              isStreaming: true,
              isFinal: false,
            };
            this.messages.push(msg);
            this.markNew(msg.id);
            last = msg;
          }
          last.reasoningContent = rc;
          this.refreshMessages();
          this.scrollToBottom();
        }
      } else if (type === 'sync_required') {
        // 断线重连时服务端要求补拉历史（ring 里没有可重放的聚合事件）
        this.loadHistory();
      } else if (type === 'agent_error') {
        // 后端错误一律转人话，技术细节不上 UI
        this.chatLoading = false;
        this.chatStage = '本轮处理失败，请重试';
        this.refreshMessages();
      } else if (type === 'content_delta') {
        const delta: string = payload['content'] as string ?? '';
        if (delta.length > 0) {
          this.sseActiveForTurn = true;
          let last: ChatMessage | null = this.lastMessage();
          if (last === null || last.role !== 'assistant' || last.isFinal) {
            const msg: ChatMessage = {
              id: this.allocId(),
              role: 'assistant',
              content: '',
              isStreaming: true,
              isFinal: false,
            };
            this.messages.push(msg);
            this.markNew(msg.id);
            last = msg;
          }
          last.content += delta;
          this.refreshMessages();
          this.scrollToBottom();
        }
      } else if (type === 'reasoning_delta') {
        const delta: string = payload['content'] as string ?? '';
        if (delta.length > 0) {
          this.sseActiveForTurn = true;
          this.chatStage = 'AI 思考中...';
          let last: ChatMessage | null = this.lastMessage();
          if (last === null || last.role !== 'assistant' || last.isFinal) {
            const msg: ChatMessage = {
              id: this.allocId(),
              role: 'assistant',
              content: '',
              isStreaming: true,
              isFinal: false,
            };
            this.messages.push(msg);
            this.markNew(msg.id);
            last = msg;
          }
          if (last.reasoningContent === undefined) {
            last.reasoningContent = '';
          }
          last.reasoningContent += delta;
          this.refreshMessages();
        }
      } else if (type === 'tool_call') {
        const toolName: string = payload['tool'] as string ?? '';
        const toolStatus: string = payload['status'] as string ?? '';
        const toolPlugin: string = payload['plugin'] as string ?? '';
        if (toolName.length > 0) {
          this.chatStage = '工具调用: ' + toolName;
          const target: ToolCallInfo = this.ensureToolCall(toolName);
          if (toolPlugin.length > 0) {
            target.plugin = toolPlugin;
          }
          const argsText: string = this.stringifyField(payload['args']);
          if (argsText.length > 0) {
            target.args = argsText;
          }
          if (toolStatus.length > 0) {
            // 后端只在工具执行【结束】时发 tool_call（status=ok/denied/interrupted），
            // 所以拿到 status 就意味着这次调用已收尾，result 一并落卡。
            target.status = toolStatus;
            target.result = this.stringifyField(payload['result']);
          } else {
            target.status = 'running';
          }
          this.refreshMessages();
          this.scrollToBottom();
        }
      } else if (type === 'stage') {
        const phase: string = payload['phase'] as string ?? '';
        const channel: string = payload['channel'] as string ?? '';
        const stageTool: string = payload['tool'] as string ?? '';
        if (channel !== '_consolidation_') {
          if (phase === 'pre_action') {
            this.chatStage = 'AI 思考中...';
          } else if (phase === 'before_toolcall') {
            this.chatStage = '工具调用: ' + stageTool;
            // 关键：tool_call 事件只在执行【结束】后才发，所以"调用中"这一态
            // 必须由 before_toolcall 建卡，否则用户永远看不到工具正在跑。
            if (stageTool.length > 0) {
              const tc: ToolCallInfo = this.ensureToolCall(stageTool);
              if (tc.status === undefined) {
                tc.status = 'running';
              }
            }
          } else if (phase === 'before_output') {
            this.chatStage = '生成回复中...';
          }
          this.refreshMessages();
        }
      }
    } catch (e) {
      // ignore parse errors
    }
  }

  /**
   * 取当前助手消息里名为 name 的未完成工具卡，没有就建一张。
   * 顺带保证一定存在一条"未定稿的助手消息"来挂这些卡。
   */
  private ensureToolCall(name: string): ToolCallInfo {
    let last: ChatMessage | null = this.lastMessage();
    if (last === null || last.role !== 'assistant' || last.isFinal === true) {
      const msg: ChatMessage = {
        id: this.allocId(),
        role: 'assistant',
        content: '',
        isStreaming: true,
        isFinal: false,
      };
      this.messages.push(msg);
      this.markNew(msg.id);
      last = msg;
    }
    if (last.toolCalls === undefined) {
      last.toolCalls = [];
    }
    for (let i = 0; i < last.toolCalls.length; i++) {
      const t: ToolCallInfo = last.toolCalls[i];
      // 只复用"仍在执行"的同名卡：同一轮里同名工具被多次调用时，
      // 已完成的那张不能被后来的调用覆盖。
      if (t.name === name && this.tcRunning(t)) {
        return t;
      }
    }
    const created: ToolCallInfo = { name: name, args: '' };
    last.toolCalls.push(created);
    return created;
  }

  /** payload 字段可能是字符串、对象或数组，统一转成可展示文本。 */
  private stringifyField(raw: Object | undefined): string {
    if (raw === undefined || raw === null) {
      return '';
    }
    if (typeof raw === 'string') {
      return raw as string;
    }
    try {
      return JSON.stringify(raw);
    } catch (e) {
      return String(raw);
    }
  }

  private lastMessage(): ChatMessage | null {
    if (this.messages.length === 0) {
      return null;
    }
    return this.messages[this.messages.length - 1];
  }

  /** 防抖刷新：合并高频 SSE delta，最多 ~20fps */
  private refreshMessages(): void {
    if (this.refreshTimer >= 0) {
      return;
    }
    this.refreshTimer = setTimeout(() => {
      this.refreshTimer = -1;
      this.messages = this.messages.slice();
    }, 50);
  }

  /** 强制立即刷新（用于状态切换等需要即时响应的场景） */
  private forceRefresh(): void {
    if (this.refreshTimer >= 0) {
      clearTimeout(this.refreshTimer);
      this.refreshTimer = -1;
    }
    this.messages = this.messages.slice();
  }

  /**
   * ForEach 键：消息结构变化即换键 → 旧气泡销毁重建 → @Builder 里的
   * if 分支重新求值。这是 ArkUI V1 渲染模型决定的：ForEach 对相同键
   * 只更新 @Prop/@Link 绑定，不重新执行 @Builder 体，所以
   * 「思考卡/工具卡/附件」这些用 if 包裹的条件分支在首次渲染后
   * 永远不会再次求值——气泡里的这些面板就永远不出现。
   *
   * 反过来，content_delta 不进 structSig：正文文本靠 MarkdownView
   * 的 @Prop content 响应式更新，不重建气泡 → 流式渲染平滑。
   * 实测 SSE 里 reasoning_delta 与 content_delta 不交错（思考阶段
   * 先于输出阶段），所以思考期间重建气泡不会打断正文流式动画。
   */
  private structSig(msg: ChatMessage): string {
    let s: string = msg.id.toString();
    const rc: string | undefined = msg.reasoningContent;
    s += '_r' + (rc !== undefined ? rc.length.toString() : '0');
    s += '_ro' + (msg.reasoningOpen === true ? '1' : '0');
    const tcs: ToolCallInfo[] | undefined = msg.toolCalls;
    if (tcs !== undefined) {
      s += '_t' + tcs.length.toString();
      for (let i = 0; i < tcs.length; i++) {
        const tc: ToolCallInfo = tcs[i];
        s += '_' + (tc.status ?? '');
        s += '_' + (tc.open === true ? 'o' : 'c');
        s += '_' + (tc.args !== undefined ? tc.args.length.toString() : '0');
        s += '_' + (tc.result !== undefined ? tc.result.length.toString() : '0');
        s += '_' + (tc.plugin ?? '');
      }
    } else {
      s += '_t0';
    }
    s += '_a' + (msg.attachment !== undefined ? '1' : '0');
    s += '_src' + (msg.source ?? '');
    s += '_f' + (msg.isFinal === true ? '1' : '0');
    s += '_s' + (msg.isStreaming === true ? '1' : '0');
    return s;
  }

  /**
   * 消息列表底部留白：悬浮输入区（导航栏上方 94 + 输入行 44 ≈ 138）
   * 之上再留 ~72 的安全间距，保证最后一条气泡的思考卡/工具卡/附件
   * 不被遮挡；加号菜单与待发送附件预览展开时再加高。
   */
  private listBottomPad(): number {
    let pad: number = 210;
    if (this.inputMultiLine) {
      pad += 60;
    }
    if (this.attachMenuOpen) {
      pad += 70;
    }
    if (this.pendingName.length > 0) {
      pad += 46;
    }
    return pad;
  }

  /** 标记新消息，触发入场动画 */
  private markNew(msgId: number): void {
    const arr: number[] = this.newMsgIds.slice();
    arr.push(msgId);
    this.newMsgIds = arr;
    setTimeout(() => {
      const idx: number = this.newMsgIds.indexOf(msgId);
      if (idx >= 0) {
        const updated: number[] = this.newMsgIds.slice();
        updated.splice(idx, 1);
        this.newMsgIds = updated;
        this.forceRefresh();
      }
    }, 250);
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  /**
   * 底部淡出遮罩的两个端色：背景底色的全不透明 / 全透明版本。
   * bgPrimary 是 6 位十六进制，这里手拼 8 位 ARGB —— 与 PageTopBar
   * 顶部淡出用的是同一手法，保证上下两端的融入观感一致。
   */
  private opaqueBottomBg(): string {
    return '#FF' + this.palette().bgPrimary.substring(1);
  }

  private transparentBottomBg(): string {
    return '#00' + this.palette().bgPrimary.substring(1);
  }

  /** 输入框底：高不透明度 + blur，保证背景内容不会透过输入文字 */
  private inputSolidBg(): string {
    return this.isDark ? 'rgba(28, 28, 30, 0.94)' : 'rgba(245, 245, 247, 0.92)';
  }

  private scrollToBottom(): void {
    this.autoScrolling = true;
    setTimeout(() => {
      this.scroller.scrollEdge(Edge.Bottom);
    }, 50);
    setTimeout(() => {
      this.autoScrolling = false;
      this.navHidden = false;
      navBar.setVisible(true);
    }, 450);
  }

  /**
   * 由【文本本身】判断输入框是否需要换行，而不是回读控件高度。
   *
   * 用 MeasureUtils 在单行态可用宽度下测量文字：宽度超了就是多行。
   * 测量宽度恒定取单行态（窄）宽度，与控件当前实际宽度无关，
   * 所以"多行时变宽"不会反过来改变判定结果 —— 没有反馈环，也就不抖。
   */
  private recomputeMultiLine(text: string): void {
    const avail: number = this.inputRowWidth - INPUT_LEFT_GAP - INPUT_RIGHT_GAP
      - INPUT_INNER_PAD * 2;
    if (avail <= 0) {
      return;
    }
    let multi: boolean = text.indexOf('\n') >= 0;
    if (!multi && text.length > 0) {
      const opt: MeasureOptions = {
        textContent: text,
        fontSize: INPUT_FONT_SIZE,
      };
      const size: SizeOptions = this.getUIContext().getMeasureUtils().measureTextSize(opt);
      // measureTextSize 返回 px，可用宽度是 vp，换算后再比
      const widthVp: number = this.getUIContext().px2vp(size.width as number);
      multi = widthVp > avail;
    }
    if (multi !== this.inputMultiLine) {
      this.getUIContext().animateTo({ duration: 260, curve: Curve.Friction }, () => {
        this.inputMultiLine = multi;
      });
    }
  }

  // ===================== 附件：选择与上传 =====================

  /** 从图库挑一张图 */
  private async pickImage(): Promise<void> {
    this.getUIContext().animateTo({ duration: ANIM_NORMAL, curve: Curve.EaseOut }, () => {
      this.attachMenuOpen = false;
    });
    try {
      const options = new picker.PhotoSelectOptions();
      options.MIMEType = picker.PhotoViewMIMETypes.IMAGE_TYPE;
      options.maxSelectNumber = 1;
      const photoPicker = new picker.PhotoViewPicker();
      const result = await photoPicker.select(options);
      if (result.photoUris.length === 0) {
        return;
      }
      this.stagePickedFile(result.photoUris[0], true);
    } catch (e) {
      this.chatStage = userMessage('chat.pickImage', e);
      this.forceRefresh();
    }
  }

  /** 从文件管理器挑一个文件 */
  private async pickFile(): Promise<void> {
    this.getUIContext().animateTo({ duration: ANIM_NORMAL, curve: Curve.EaseOut }, () => {
      this.attachMenuOpen = false;
    });
    try {
      const options = new picker.DocumentSelectOptions();
      options.maxSelectNumber = 1;
      const docPicker = new picker.DocumentViewPicker();
      const uris: string[] = await docPicker.select(options);
      if (uris.length === 0) {
        return;
      }
      this.stagePickedFile(uris[0], false);
    } catch (e) {
      this.chatStage = userMessage('chat.pickFile', e);
      this.forceRefresh();
    }
  }

  /**
   * 把 picker 给的 URI 复制到应用沙箱。
   * http 的 multiFormDataList.filePath 只能读应用自己的沙箱路径，
   * 直接把 picker 的 media:// URI 交过去会读不到内容。
   */
  private stagePickedFile(srcUri: string, isImage: boolean): void {
    try {
      const ctx = getContext(this) as common.UIAbilityContext;
      const name: string = fileNameOf(srcUri);
      const destPath: string = ctx.filesDir + '/up_' + Date.now().toString(36) + '_' + name;
      const srcFile = fileIo.openSync(srcUri, fileIo.OpenMode.READ_ONLY);
      const destFile = fileIo.openSync(destPath,
        fileIo.OpenMode.READ_WRITE | fileIo.OpenMode.CREATE | fileIo.OpenMode.TRUNC);
      fileIo.copyFileSync(srcFile.fd, destFile.fd);
      fileIo.closeSync(srcFile);
      fileIo.closeSync(destFile);
      const stat = fileIo.statSync(destPath);
      this.clearPendingFile();
      this.pendingPath = destPath;
      this.pendingName = name;
      this.pendingSize = stat.size;
      this.pendingIsImage = isImage;
      this.pendingMime = mimeOf(name, isImage);
    } catch (e) {
      this.chatStage = userMessage('chat.stageFile', e);
    }
    this.forceRefresh();
  }

  /** 丢弃待发送附件，并删掉沙箱里的临时副本 */
  private clearPendingFile(): void {
    if (this.pendingPath.length > 0) {
      try {
        fileIo.unlinkSync(this.pendingPath);
      } catch (e) {
        // 已不存在，忽略
      }
    }
    this.pendingPath = '';
    this.pendingName = '';
    this.pendingSize = 0;
    this.pendingIsImage = false;
    this.pendingMime = '';
  }

  /**
   * 带附件发送：POST /chat/file（multipart），字段与 WebGUI 一致。
   * 后端收下后自身会把用户消息与附件写进会话并触发 agent，
   * 回复照常从 SSE 过来，所以这里不再走 /chat。
   */
  private async sendWithAttachment(text: string): Promise<void> {
    const path: string = this.pendingPath;
    const name: string = this.pendingName;
    const size: number = this.pendingSize;
    const isImage: boolean = this.pendingIsImage;
    const mime: string = this.pendingMime;

    const att: ChatAttachment = {
      type: isImage ? 'image' : 'file',
      // 本地待上传：先用沙箱路径预览，上传成功后替换成服务端 URL
      url: 'file://' + path,
      size: size,
      name: name,
    };
    const userMsg: ChatMessage = { id: this.allocId(), role: 'user', content: text };
    userMsg.attachment = att;
    this.messages.push(userMsg);
    this.markNew(userMsg.id);
    this.inputText = '';
    this.inputMultiLine = false;
    this.uploading = true;
    this.chatLoading = true;
    this.chatStage = '正在上传附件...';
    this.sseActiveForTurn = false;
    this.forceRefresh();
    this.scrollToBottom();

    const parts: http.MultiFormData[] = [
      { name: 'file', contentType: mime, remoteFileName: name, filePath: path },
      { name: 'message', contentType: 'text/plain', data: text },
      { name: 'client_msg_id', contentType: 'text/plain', data: Date.now().toString(36) },
      { name: 'device_id', contentType: 'text/plain', data: connStore.ensureDeviceId() },
      { name: 'device_name', contentType: 'text/plain', data: connStore.getDeviceName() },
    ];

    try {
      const resp = await apiClient.postMultipart('/chat/file', parts, 180000);
      const obj: Record<string, Object> = JSON.parse(resp.body) as Record<string, Object>;
      const uploaded: ChatAttachment | undefined = parseAttachment(obj['file']);
      if (uploaded !== undefined) {
        userMsg.attachment = uploaded;
      }
      this.chatStage = '等待 AI 回复...';
    } catch (e) {
      this.chatStage = userMessage('chat.upload', e);
      this.chatLoading = false;
    }
    this.uploading = false;
    this.clearPendingFile();
    this.forceRefresh();
    this.scrollToBottom();
  }

  private async sendChat(): Promise<void> {
    const text: string = this.inputText.trim();
    if (this.chatLoading || this.uploading) {
      return;
    }
    const cur = connStore.getCurrentConnection();
    if (cur === null) {
      return;
    }
    if (this.pendingPath.length > 0) {
      await this.sendWithAttachment(text);
      return;
    }
    if (text.length === 0) {
      return;
    }
    this.inputText = '';
    this.inputMultiLine = false;
    // 重置 SSE 标记
    this.sseActiveForTurn = false;

    const userMsg: ChatMessage = { id: this.allocId(), role: 'user', content: text };
    this.messages.push(userMsg);
    this.markNew(userMsg.id);
    this.chatLoading = true;
    this.chatStage = '等待 AI 回复...';
    this.forceRefresh();
    this.scrollToBottom();

    const bodyObj: SendChatBody = {
      message: text,
      client_msg_id: Date.now().toString(36),
      // 必须带设备身份：后端没有 device_id 就把来源编码成 webui，
      // agent 会以为消息来自网页端。device_id 非空时后端编码
      // source = "webui/<device_id>" 并注入设备上下文。
      device_id: connStore.ensureDeviceId(),
      device_name: connStore.getDeviceName(),
    };

    // 如果 SSE 已连接，POST 作为触发器（响应由 SSE 推送渲染）；
    // 仅在 SSE 未推送内容时才用 POST 响应兜底创建消息。
    try {
      const resp = await apiClient.postWithTimeout('/chat', bodyObj, 120000);

      // SSE 已经处理了响应，跳过 POST 消息创建
      if (this.sseActiveForTurn) {
        this.chatLoading = false;
        this.chatStage = '';
        this.forceRefresh();
        this.scrollToBottom();
        return;
      }

      const parsed: Record<string, string> = JSON.parse(resp.body) as Record<string, string>;
      const respText: string = parsed['response'] ?? '(无响应)';
      const reasoning: string = parsed['reasoning_content'] ?? '';
      const last: ChatMessage | null = this.lastMessage();
      if (last !== null && last.role === 'assistant' && !last.isFinal) {
        last.content = respText;
        last.isFinal = true;
        last.isStreaming = false;
        if (reasoning.length > 0 && last.reasoningContent === undefined) {
          last.reasoningContent = reasoning;
        }
      } else if (last !== null && last.role === 'assistant' && last.isFinal) {
        // 已有最终消息，合并（不应发生，但防御性处理）
        if (respText.length > last.content.length) {
          last.content = respText;
        }
      } else {
        const msg: ChatMessage = {
          id: this.allocId(),
          role: 'assistant',
          content: respText,
          isFinal: true,
        };
        if (reasoning.length > 0) {
          msg.reasoningContent = reasoning;
        }
        this.messages.push(msg);
        this.markNew(msg.id);
      }
      this.chatLoading = false;
      this.chatStage = '';
      this.forceRefresh();
      this.scrollToBottom();
    } catch (e) {
      // 超时通常意味着后端仍在生成，不算失败；其余一律显示人话，
      // 原始错误只进 hilog（之前把 e.message 拼进 chatStage 会把
      // "Failed to connect to the server."、内网地址直接摆到聊天流里）。
      if (isTimeout(e)) {
        this.chatStage = '请求已发送，等待回复...';
      } else {
        this.chatStage = userMessage('chat.send', e);
      }
      this.forceRefresh();
      this.scrollToBottom();
    }
  }

  private async interruptChat(): Promise<void> {
    try {
      await apiClient.post('/chat/interrupt', null);
    } catch (e) {
      // ignore
    }
  }

  private toggleReasoning(msg: ChatMessage): void {
    // 在 animateTo 里翻转：展开/收起时 chevron 走已有 .animation，
    // 面板节点在 animateTo 帧内获得默认过渡，不会再硬切。
    this.getUIContext().animateTo({ duration: ANIM_NORMAL, curve: Curve.EaseOut }, () => {
      msg.reasoningOpen = !(msg.reasoningOpen === true);
      this.forceRefresh();
    });
  }

  /**
   * 滚动回调：只设置普通标志位，仅在状态翻转时通知 navBar，
   * 不在回调里做任何耗时操作。navBar.setVisible 内部已去重，
   * 而布局（padding）不再依赖 navVisible，故翻转只触发 GPU 变换，
   * 不会引起布局回流——这是滑动流畅的关键。
   */
  private handleScrollDirection(yOffset: number, state: ScrollState): void {
    if (this.autoScrolling) {
      return;
    }
    // 触顶（近顶部 60vp）且服务端还有更早历史 → 向上懒加载下一页
    if (yOffset < 60 && this.chatHasMore) {
      this.loadOlderChat();
    }
    if (state === ScrollState.Idle) {
      if (this.navHidden) {
        this.navHidden = false;
        navBar.setVisible(true);
      }
    } else {
      // Scroll / Fling：向下/惯性滚动时隐藏导航与输入栏
      if (!this.navHidden) {
        this.navHidden = true;
        navBar.setVisible(false);
      }
    }
  }

  /** 判断消息是否处于入场动画阶段 */
  private isNewMsg(msgId: number): boolean {
    return this.newMsgIds.indexOf(msgId) >= 0;
  }

  // ===================== 二级页面：附件详情 =====================

  /**
   * 打开附件详情。
   * WebGUI 点图片是新开浏览器标签、点文件是直接下载；
   * 移动端没有"标签页"，改成一个二级页面：大图预览 + 元信息 + 保存。
   */
  private openAttachment(att: ChatAttachment): void {
    this.activeAtt = att;
    this.activeSub = SUB_ATTACHMENT;
    if (this.isWide && this.navStack.size() > 0) {
      this.navStack.replacePathByName(SUB_ATTACHMENT, subPageParam(SUB_ATTACHMENT), false);
    } else {
      this.navStack.pushPathByName(SUB_ATTACHMENT, subPageParam(SUB_ATTACHMENT), true);
    }
  }

  private closeAttachment(): void {
    this.navStack.pop();
    this.activeSub = SUB_NONE;
    this.activeAtt = undefined;
  }

  /** 聊天流里最后一个带附件的消息：宽屏进入 Split 时用它填充右栏 */
  private latestAttachment(): ChatAttachment | undefined {
    for (let i = this.messages.length - 1; i >= 0; i--) {
      const a: ChatAttachment | undefined = this.messages[i].attachment;
      if (a !== undefined) {
        return a;
      }
    }
    return undefined;
  }

  build() {
    // 宽屏：左栏聊天流（含底部导航与输入区），右栏附件详情。
    // 窄屏：附件详情整屏覆盖，系统返回手势直接作用于 navStack。
    Navigation(this.navStack) {
      this.ChatBody()
    }
    .navDestination(this.SubDestination)
    .mode(this.isWide ? NavigationMode.Split : NavigationMode.Stack)
    .navBarPosition(NavBarPosition.Start)
    .navBarWidth(WIDE_NAV_BAR_WIDTH)
    .minContentWidth(WIDE_MIN_CONTENT)
    .hideTitleBar(true)
    .hideToolBar(true)
    .hideBackButton(true)
    .width('100%')
    .height('100%')
    .onNavBarStateChange((isVisible: boolean) => {
      markSubPageOpen(this.isWide || !isVisible);
    })
    .onNavigationModeChange((mode: NavigationMode) => {
      if (mode === NavigationMode.Split) {
        markSubPageOpen(true);
        // 右栏不能空白：有附件就展示最近一个，没有则由 SubDestination 显示空态
        if (this.navStack.size() === 0) {
          const a: ChatAttachment | undefined = this.latestAttachment();
          this.activeAtt = a;
          this.activeSub = SUB_ATTACHMENT;
          this.navStack.pushPathByName(SUB_ATTACHMENT, subPageParam(SUB_ATTACHMENT), false);
        }
      } else {
        this.navStack.clear(false);
        this.activeSub = SUB_NONE;
        markSubPageOpen(false);
      }
    })
  }

  /** 附件详情二级页面 */
  @Builder
  SubDestination(name: string, param: object) {
    NavDestination() {
      if (name === SUB_ATTACHMENT) {
        SubPageLayer({
          title: '附件详情',
          tab: 0,
          onBack: () => {
            this.closeAttachment();
          },
        }) {
          this.AttachmentPane()
        }
      }
    }
    .hideTitleBar(true)
    .backgroundColor(Color.Transparent)
  }

  @Builder
  AttachmentPane() {
    if (this.activeAtt !== undefined) {
      AttachmentDetailContent({ att: this.activeAtt })
    } else {
      // 宽屏右栏空态：聊天里还没有任何图片/文件
      Column({ space: 8 }) {
        Image($r('app.media.ic_file'))
          .width(38)
          .height(38)
          .fillColor(this.palette().textMuted)
          .draggable(false)
        Text('选择聊天中的图片或文件查看详情')
          .fontSize(13)
          .fontColor(this.palette().textMuted)
      }
      .width('100%')
      .padding({ top: 60 })
      .alignItems(HorizontalAlign.Center)
    }
  }

  @Builder
  ChatBody() {
    Stack({ alignContent: Alignment.Bottom }) {
      // 层1：消息列表（铺满全屏，内容从顶栏遮罩下方穿过时逐渐淡出）
      Column() {
        // Messages
        Scroll(this.scroller) {
          Column() {
            ForEach(this.messages, (msg: ChatMessage, idx: number) => {
              this.MessageBubble(msg)
            }, (msg: ChatMessage, idx: number) => this.structSig(msg))

            if (this.chatLoading) {
              Row({ space: 8 }) {
                LoadingProgress()
                  .width(16)
                  .height(16)
                  .color(this.palette().accent)
                Text(this.chatStage.length > 0 ? this.chatStage : '处理中...')
                  .fontSize(12)
                  .fontColor(this.palette().textMuted)
              }
              .width('100%')
              .justifyContent(FlexAlign.Start)
              .padding({ left: 52, top: 6, bottom: 6 })
              // if 控制的节点无法用 .animation() 做进出场（那只驱动自身属性的增量更新）；
              // 用 transition 才能让"处理中"这条在出现和消失时都淡入淡出。
              .transition(TransitionEffect.OPACITY.animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
            }
          }
          .width('100%')
          .padding({
            left: 14,
            right: 14,
            top: 76,
            bottom: this.listBottomPad()
          })
        }
        .width('100%')
        .height('100%')
        .scrollBar(BarState.Off)
        .edgeEffect(EdgeEffect.Spring)
        .align(Alignment.Top)
        .onDidScroll((xOffset: number, yOffset: number, state: ScrollState) => {
          this.handleScrollDirection(yOffset, state);
        })
      }
      .width('100%')
      .height('100%')

      // 层1.5：顶栏遮罩（自身撑满并顶部对齐，全链路 hitTest None，触摸完全穿透）
      PageTopBar({ title: '聊天' })

      // 层1.6：底部淡出遮罩 —— 滚动内容接近悬浮输入区/底部导航时逐渐隐入背景，
      // 而不是在玻璃后面清晰可见（PageTopBar 顶部淡出手法的镜像，方向相反）。
      Column()
        .width('100%')
        .height(170)
        .linearGradient({
          direction: GradientDirection.Bottom,
          colors: [
            [this.transparentBottomBg(), 0.0],
            [this.opaqueBottomBg(), 0.6],
            [this.opaqueBottomBg(), 1.0],
          ],
        })
        .hitTestBehavior(HitTestMode.None)

      // 层2：悬浮输入区（导航栏上方，与导航栏左右平齐，各组件独立不共框）
      NavFloatOverlay({ tab: 0 }) {
        // 加号展开的两个选项（图片 / 文件），点一次收起
        if (this.attachMenuOpen) {
          this.AttachMenu()
        }

        // 待发送附件预览（选好图片/文件、还没点发送时显示）
        if (this.pendingName.length > 0) {
          this.PendingAttachmentChip()
        }

        // Stack 而不是 Column：加号与发送按钮钉死在底部这一行不动，
        // 输入框是浮在它们上面的独立层，超过一行就往上长并展开到整行宽度。
        Stack({ alignContent: Alignment.Bottom }) {
          // 底层：固定不动的一行 —— 左加号（图片/文件）、右发送/中断按钮
          Row() {
            FloatIconButton({
              icon: $r('app.media.ic_plus'),
              onTap: () => {
                // 菜单展开会同时改变 listBottomPad，用 animateTo 把
                // 列表内边距和菜单进出场拉到同一个时钟上。
                this.getUIContext().animateTo({ duration: ANIM_NORMAL, curve: Curve.EaseOut }, () => {
                  this.attachMenuOpen = !this.attachMenuOpen;
                });
              },
            })
            Blank()
            if (this.chatLoading) {
              Button() {
                Image($r('app.media.ic_stop'))
                  .width(16)
                  .height(16)
                  .fillColor(Color.White)
              }
                .width(44)
                .height(44)
                .type(ButtonType.Circle)
                .backgroundColor('#77809A')
                // 发送/中断切换是 if 分支整体替换，用 transition 淡入淡出
                .transition(TransitionEffect.OPACITY.combine(TransitionEffect.scale({ x: 0.9, y: 0.9 })).animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
                .onClick(() => {
                  this.interruptChat();
                })
            } else {
              Button() {
                Image($r('app.media.ic_send'))
                  .width(18)
                  .height(18)
                  .fillColor(Color.White)
              }
                .width(44)
                .height(44)
                .type(ButtonType.Circle)
                .backgroundColor(this.palette().accent)
                .enabled(this.inputText.trim().length > 0 || this.pendingPath.length > 0)
                .transition(TransitionEffect.OPACITY.combine(TransitionEffect.scale({ x: 0.9, y: 0.9 })).animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
                .onClick(() => {
                  this.sendChat();
                })
            }
          }
          .width('100%')
          .height(44)
          .alignItems(VerticalAlign.Center)
          .onAreaChange((_o: Area, n: Area) => {
            // 这一行高度恒为 44、宽度恒为 100%，测量它不会形成反馈环。
            const w: number = n.width as number;
            if (Math.abs(w - this.inputRowWidth) > 0.5) {
              this.inputRowWidth = w;
              this.recomputeMultiLine(this.inputText);
            }
          })

          // 上层：输入框。
          // 单行时左右让出加号(42+8)与发送键(44+8)的位置，与它们同处一行；
          // 多行时整体上移 52 抬到那一行之上，并铺满整行宽度。
          //
          // 之前"只上移不变宽"是因为宽度被钉死了：让宽度跟着实测高度变会形成
          // 布局反馈环（变宽→文字回落成一行→变窄→又折行），卡在半弹出态抖动。
          // 现在改用 MeasureUtils 直接量文字：始终按【窄宽度】测量是否需要换行，
          // 判定输入只依赖文本内容，与控件实际宽度无关，所以变宽也不会自激。
          Row() {
            TextArea({
              placeholder: '输入消息...',
              text: this.inputText,
            })
              .layoutWeight(1)
              // 不写死高度：单行 44，随文字换行自动增高，最多约 5 行后内部滚动
              .constraintSize({ minHeight: 44, maxHeight: 168 })
              .fontSize(INPUT_FONT_SIZE)
              .fontColor(this.palette().textPrimary)
              .placeholderFont({ size: 13 })
              .placeholderColor(this.palette().textMuted)
              .backgroundColor(this.inputSolidBg())
              .backdropBlur(24)
              .borderRadius(22)
              .border({ width: 1, color: this.palette().glassBorder })
              .padding({
                left: INPUT_INNER_PAD,
                right: INPUT_INNER_PAD,
                top: 11,
                bottom: 11,
              })
              .enterKeyType(EnterKeyType.Send)
              .onChange((value: string) => {
                this.inputText = value;
                this.recomputeMultiLine(value);
              })
              .onSubmit(() => {
                this.sendChat();
              })
          }
          .width('100%')
          // 多行时必须显式写 0：给 .padding() 传 undefined 在增量更新时会被当作
          // "不修改该属性"，旧的左右 50/52 留在原地 —— 这就是"只上移不变宽"。
          .padding(this.inputMultiLine
            ? { left: 0, right: 0 }
            : { left: INPUT_LEFT_GAP, right: INPUT_RIGHT_GAP })
          .margin({ bottom: this.inputMultiLine ? 52 : 0 })
          // 关键：这层 Row 铺满整宽，它的左右 padding 正好压在加号与发送键上方。
          // 不设 None 的话 padding 区域仍属于 Row，会把点击吞掉 —— 发送键点不动。
          // None = 自身不响应、子节点（TextArea）照常响应，触摸落到下层那一行。
          .hitTestBehavior(HitTestMode.None)
          .animation({ duration: 260, curve: Curve.Friction })
        }
        .width('100%')
      }
    }
    .width('100%')
    .height('100%')
  }

  /** 加号菜单：两枚独立的玻璃胶囊，和输入区其他组件同一套视觉语言 */
  @Builder
  AttachMenu() {
    // 外层撑满并左对齐：菜单要出现在加号正上方，而不是跟着悬浮区右对齐
    Row() {
      Row({ space: 8 }) {
        this.AttachOption($r('app.media.ic_image'), '图片', () => {
          this.pickImage();
        })
        this.AttachOption($r('app.media.ic_file'), '文件', () => {
          this.pickFile();
        })
      }
    }
    .width('100%')
    .justifyContent(FlexAlign.Start)
    .margin({ bottom: 8 })
    .hitTestBehavior(HitTestMode.Transparent)
    // 加号菜单由 if 控制，进出场只能靠 transition；配合下面 toggle 处的
    // animateTo，展开时两枚胶囊从加号上方浮起而不是硬闪出来。
    .transition(TransitionEffect.OPACITY.combine(TransitionEffect.translate({ y: 12 })).animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut }))
  }

  @Builder
  AttachOption(icon: Resource, label: string, tap: () => void) {
    // 按压反馈交给 MotionBase：每个实例自带独立按压态，
    // 修掉了原先两枚胶囊共用一个 @State、按一枚两枚同时缩放的问题。
    MotionBase({ pressEnabled: true, fillWidth: false }) {
      Row({ space: 6 }) {
        Image(icon)
          .width(15)
          .height(15)
          .fillColor(this.palette().textSecondary)
          .draggable(false)
        Text(label)
          .fontSize(12)
          .fontColor(this.palette().textPrimary)
      }
      .padding({ left: 12, right: 14, top: 8, bottom: 8 })
      .backgroundColor(this.palette().navBarBg)
      .borderRadius(RADIUS_PILL)
      .border({ width: 1, color: this.palette().navBarBorder })
      .shadow({ radius: 20, color: this.palette().shadow, offsetY: 6 })
      .onClick(tap)
    }
  }

  /** 待发送附件预览条：缩略信息 + 一个移除按钮 */
  @Builder
  PendingAttachmentChip() {
    // 按压反馈交给 MotionBase（全宽预览条）
    MotionBase({ pressEnabled: true }) {
      Row({ space: 8 }) {
        Image(this.pendingIsImage ? $r('app.media.ic_image') : $r('app.media.ic_file'))
          .width(15)
          .height(15)
          .fillColor(this.palette().accent)
          .draggable(false)
        Column({ space: 1 }) {
          Text(this.pendingName)
            .fontSize(12)
            .fontColor(this.palette().textPrimary)
            .maxLines(1)
            .textOverflow({ overflow: TextOverflow.Ellipsis })
          Text(this.uploading ? '上传中...' : formatBytes(this.pendingSize))
            .fontSize(10)
            .fontColor(this.palette().textMuted)
        }
        .alignItems(HorizontalAlign.Start)
        .layoutWeight(1)
        if (this.uploading) {
          LoadingProgress()
            .width(14)
            .height(14)
            .color(this.palette().accent)
        } else {
          Image($r('app.media.ic_close'))
            .width(13)
            .height(13)
            .fillColor(this.palette().textMuted)
            .draggable(false)
            .onClick(() => {
              this.clearPendingFile();
              this.forceRefresh();
            })
        }
      }
      .width('100%')
      .padding({ left: 12, right: 12, top: 8, bottom: 8 })
      .margin({ bottom: 8 })
      .backgroundColor(this.palette().navBarBg)
      .borderRadius(RADIUS_MD)
      .border({ width: 1, color: this.palette().navBarBorder })
      .shadow({ radius: 20, color: this.palette().shadow, offsetY: 6 })
    }
  }

  @Builder
  Avatar(role: string) {    Text(role === 'user' ? '我' : 'AI')
      .fontSize(11)
      .fontWeight(FontWeight.Bold)
      .fontColor(role === 'user' ? this.palette().msgUserText : this.palette().accent)
      .textAlign(TextAlign.Center)
      .width(28)
      .height(28)
      .borderRadius(RADIUS_PILL)
      .backgroundColor(role === 'user' ? this.palette().msgUserBg : this.palette().accentBg)
      .margin({ top: 2 })
  }

  @Builder
  ReasoningCard(msg: ChatMessage) {
    // 两层结构，原因见 BubbleBody 的布局说明：
    // 外层 Row 是"外观壳"（虚线边框 / 底色 / 圆角），不设百分比宽度，
    // 靠内层 layoutWeight(1) 把气泡内容框的剩余宽度吃满；
    // 内层 holder Column 自身无 padding，所以它的子节点写 width('100%')
    // 才有正确的解析基准，不会再溢出到气泡外被 clip 切掉。
    Row() {
      Column() {
        Row({ space: 7 }) {
        Image($r('app.media.ic_sparkle'))
          .width(12)
          .height(12)
          .fillColor(this.palette().accent)
        Text('思考过程')
          .fontSize(11)
          .fontWeight(FontWeight.Medium)
          .fontColor(this.palette().textPrimary)
        // 流式思考时给出明确进度指示，而不是一张看不出在动的折叠卡
        if (msg.isFinal !== true && msg.isStreaming === true) {
          LoadingProgress()
            .width(11)
            .height(11)
            .color(this.palette().accent)
        }
        Blank()
        Text(this.reasoningLenLabel(msg))
          .fontSize(9.5)
          .fontColor(this.palette().textMuted)
        Image($r('app.media.ic_chevron_down'))
          .width(14)
          .height(14)
          .fillColor(this.palette().textMuted)
          .rotate({ angle: msg.reasoningOpen === true ? 180 : 0 })
          .animation({ duration: 200, curve: Curve.EaseOut })
      }
      .width('100%')
      .padding({ left: 10, right: 10, top: 6, bottom: 6 })
      .onClick(() => {
        this.toggleReasoning(msg);
      })

      if (msg.reasoningOpen === true) {
        Text(msg.reasoningContent)
          .fontSize(11.5)
          .lineHeight(17)
          .fontColor(this.palette().textTertiary)
          .width('100%')
          .padding({ left: 10, right: 10, bottom: 8 })
          .maxLines(24)
          .textOverflow({ overflow: TextOverflow.Ellipsis })
          .wordBreak(WordBreak.BREAK_ALL)
          // 面板内容靠 if 挂载：用 transition 在展开/收起时淡入淡出
          .transition(TransitionEffect.OPACITY.combine(TransitionEffect.translate({ y: -6 })).animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut }))
      }

      // 流式思考中：扫光动画条（对齐 WebGUI reasoningSweep）
      if (msg.isFinal !== true && msg.isStreaming === true) {
        Stack() {
          Row()
            .height(2)
            .borderRadius(2)
            .width('200%')
            .linearGradient({
              angle: 90,
              colors: [
                ['rgba(255,255,255,0.01)', 0],
                [this.palette().accent, 0.35],
                ['rgba(255,255,255,0.01)', 0.5],
                [this.palette().accent, 0.65],
                ['rgba(255,255,255,0.01)', 1],
              ],
            })
            .opacity(0.7)
            .translate({ x: this.isNewMsg(msg.id) ? '0%' : '-50%' })
            .animation({ duration: 1200, curve: Curve.Linear })
        }
        .width('100%')
        .clip(true)
        .height(2)
        .margin({ top: 6 })
      }
      }
      .layoutWeight(1)
      .alignItems(HorizontalAlign.Start)
    }
    .alignItems(VerticalAlign.Top)
    .margin({ bottom: 6 })
    .borderRadius(RADIUS_SM)
    .backgroundColor(this.palette().bgHover)
    .border({ width: 1, color: this.palette().kvBorder, style: BorderStyle.Dashed })
  }

  /** 折叠时也要能看出思考在增长：显示字数 */
  private reasoningLenLabel(msg: ChatMessage): string {
    const rc: string | undefined = msg.reasoningContent;
    if (rc === undefined || rc.length === 0) {
      return '';
    }
    return rc.length.toString() + ' 字';
  }

  /**
   * 是否仍在执行。
   * 判据是 status 而不是 result：后端 status=ok 的工具也可能返回空串，
   * 用 result 判断会让这类调用永远显示"调用中"。
   */
  private tcRunning(tc: ToolCallInfo): boolean {
    const s: string | undefined = tc.status;
    return s === undefined || s.length === 0 || s === 'running';
  }

  private tcError(tc: ToolCallInfo): boolean {
    return tc.status === 'denied' || tc.status === 'error';
  }

  private tcLeftColor(tc: ToolCallInfo): string {
    if (this.tcError(tc)) {
      return '#DB3694';
    }
    if (this.tcRunning(tc)) {
      return this.palette().accent;
    }
    return 'rgba(23, 169, 100, 0.8)';
  }

  @Builder
  ToolCard(tc: ToolCallInfo) {
    // 同 ReasoningCard：外层 Row 只做外观，内层 layoutWeight(1) 取真实内容宽
    Row() {
      Column() {
        Row({ space: 6 }) {
        if (this.tcError(tc)) {
          Image($r('app.media.ic_error'))
            .width(13).height(13)
            .fillColor(this.tcIcoColor(tc))
        } else if (this.tcRunning(tc)) {
          LoadingProgress()
            .width(12).height(12)
            .color(this.tcIcoColor(tc))
        } else {
          Image($r('app.media.ic_check'))
            .width(13).height(13)
            .fillColor(this.tcIcoColor(tc))
        }
        Text(tc.name)
          .fontSize(11)
          .fontWeight(FontWeight.Medium)
          .fontColor(this.palette().textPrimary)
          .maxLines(1)
          .textOverflow({ overflow: TextOverflow.Ellipsis })
          // 名字可长可短，必须让它占剩余宽度并可收缩，
          // 否则右侧状态文字会被挤出气泡（78% 宽度 + clip 直接切掉）
          .layoutWeight(1)
        if (tc.plugin !== undefined && tc.plugin.length > 0) {
          Text(tc.plugin)
            .fontSize(9.5)
            .fontColor(this.palette().textMuted)
            .maxLines(1)
            .flexShrink(0)
        }
        // 状态徽标去掉，只留一行小字（用户要求"去掉所有状态徽标"）
        Text(this.tcStateLabel(tc))
          .fontSize(9.5)
          .fontWeight(FontWeight.Medium)
          .fontColor(this.tcStateColor(tc))
          .flexShrink(0)
        Image($r('app.media.ic_chevron_down'))
          .width(13).height(13)
          .fillColor(this.palette().textMuted)
          .flexShrink(0)
          .rotate({ angle: tc.open === true ? 180 : 0 })
          .animation({ duration: 150, curve: Curve.EaseOut })
      }
      .width('100%')
      .alignItems(VerticalAlign.Center)
      .onClick(() => {
        this.toggleToolCard(tc);
      })

      if (tc.open === true) {
        Column() {
          if (tc.args.length > 0 && tc.args !== '{}') {
            Text('参数')
              .fontSize(9.5).fontWeight(FontWeight.Medium)
              .fontColor(this.palette().textMuted)
              .margin({ top: 6, bottom: 2 })
            Text(tc.args)
              .fontSize(11)
              .fontColor(this.palette().preText)
              .backgroundColor(this.palette().preBg)
              .borderRadius(4)
              .padding({ left: 7, right: 7, top: 5, bottom: 5 })
              .width('100%')
              .textAlign(TextAlign.Start)
              .wordBreak(WordBreak.BREAK_ALL)
          }
          if (tc.result !== undefined && tc.result.length > 0) {
            Text('结果')
              .fontSize(9.5).fontWeight(FontWeight.Medium)
              .fontColor(this.palette().textMuted)
              .margin({ top: 6, bottom: 2 })
            Text(tc.result)
              .fontSize(11)
              .fontColor(this.palette().preText)
              .backgroundColor(this.palette().preBg)
              .borderRadius(4)
              .padding({ left: 7, right: 7, top: 5, bottom: 5 })
              .width('100%')
              .textAlign(TextAlign.Start)
              .wordBreak(WordBreak.BREAK_ALL)
              .maxLines(8)
              .textOverflow({ overflow: TextOverflow.Ellipsis })
          }
        }
        .width('100%')
        .alignItems(HorizontalAlign.Start)
        // 展开内容整体用 if 挂载：transition 让参数/结果随 chevron 一起淡入
        .transition(TransitionEffect.OPACITY.combine(TransitionEffect.translate({ y: -6 })).animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut }))
      }
      }
      .layoutWeight(1)
      .alignItems(HorizontalAlign.Start)
    }
    .alignItems(VerticalAlign.Top)
    .padding({ left: 10, right: 10, top: 7, bottom: 7 })
    .margin({ bottom: 4 })
    .borderRadius(RADIUS_SM)
    .backgroundColor(this.palette().bgHover)
    .border({
      width: { left: 3, top: 1, right: 1, bottom: 1 },
      color: {
        left: this.tcLeftColor(tc),
        top: this.palette().kvBorder,
        right: this.palette().kvBorder,
        bottom: this.palette().kvBorder,
      },
    })
  }

  private tcIcoColor(tc: ToolCallInfo): string {
    if (this.tcError(tc)) {
      return '#DB3694';
    }
    if (this.tcRunning(tc)) {
      return this.palette().accent;
    }
    return 'rgba(23, 169, 100, 0.9)';
  }

  private tcStateLabel(tc: ToolCallInfo): string {
    if (tc.status === 'denied') {
      return '已拒绝';
    }
    if (this.tcRunning(tc)) {
      return '调用中';
    }
    return '完成';
  }

  private tcStateColor(tc: ToolCallInfo): string {
    if (tc.status === 'denied') {
      return '#FF9EC6';
    }
    if (this.tcRunning(tc)) {
      return '#A3B8FF';
    }
    return '#6EE7A8';
  }

  private tcStateBg(tc: ToolCallInfo): string {
    if (tc.status === 'denied') {
      return 'rgba(219, 54, 148, 0.18)';
    }
    if (this.tcRunning(tc)) {
      return 'rgba(63, 110, 245, 0.18)';
    }
    return 'rgba(23, 169, 100, 0.18)';
  }

  /** 工具卡/思考卡在气泡内是独占一行的块，气泡 78% 宽度对它们太窄 —— 见 BubbleBody */

  private toggleToolCard(tc: ToolCallInfo): void {
    this.getUIContext().animateTo({ duration: ANIM_NORMAL, curve: Curve.EaseOut }, () => {
      tc.open = !(tc.open === true);
      this.forceRefresh();
    });
  }

  /**
   * 气泡最大宽度（相对 BubbleSlot 的宽度，即扣掉头像与间距后的真实可用宽）。
   * 纯文本 78% 好看；但工具卡/思考卡是"面板"，78% 会把里面的状态文字和
   * 参数/结果压成一团（还会被 clip 切掉），所以带卡片时放宽到 92%。
   */
  private bubbleMaxWidth(msg: ChatMessage): string {
    const hasPanels: boolean =
      (msg.toolCalls !== undefined && msg.toolCalls.length > 0) ||
      (msg.reasoningContent !== undefined && msg.reasoningContent.length > 0);
    return hasPanels ? '92%' : '78%';
  }

  /**
   * 是否"别处来的"消息。对齐 GUI 的 source !== 'webui' 判定，但多减一项：
   * 本机自己发的消息在后端会被写成 webui/<device_id>，那仍然是"我发的"，
   * 不能当成渠道消息挂上别人的头像。
   */
  private isChannelMsg(msg: ChatMessage): boolean {
    const src: string = msg.source ?? '';
    if (src.length === 0 || src === 'webui') {
      return false;
    }
    return src !== 'webui/' + connStore.ensureDeviceId();
  }

  /** 渠道名展示：webui/<id> 只显示 <id>，其余原样。 */
  private chanLabel(src: string): string {
    if (src.startsWith('webui/')) {
      return src.substring(6);
    }
    return src;
  }

  /** 渠道首字母（大写），用作头像文字。 */
  private chanLetter(src: string): string {
    const label: string = this.chanLabel(src);
    if (label.length === 0) {
      return '?';
    }
    return label.substring(0, 1).toUpperCase();
  }

  /** 由渠道名散列出稳定色，避免每次渲染换色。 */
  private chanColor(src: string): string {
    const label: string = this.chanLabel(src);
    let h: number = 0;
    for (let i = 0; i < label.length; i++) {
      h = (h * 31 + label.charCodeAt(i)) % 360;
    }
    return 'hsl(' + h.toString() + ', 52%, 46%)';
  }

  @Builder
  ChanAvatar(src: string) {
    Text(this.chanLetter(src))
      .fontSize(12)
      .fontWeight(FontWeight.Bold)
      .fontColor(Color.White)
      .textAlign(TextAlign.Center)
      .width(28)
      .height(28)
      .borderRadius(RADIUS_PILL)
      .backgroundColor(this.chanColor(src))
      .margin({ top: 2 })
  }

  /** 自己发的消息（右对齐、"我"头像）：渠道消息即使 role=user 也不算 */
  private isSelfMsg(msg: ChatMessage): boolean {
    return msg.role === 'user' && !this.isChannelMsg(msg);
  }

  /**
   * 气泡占位槽 —— 分栏右侧被切掉的根因就在这里。
   *
   * 原来 BubbleBody 直接放进 Row，它的 constraintSize maxWidth 是百分比
   * （78% / 92%）。百分比是相对【父节点外框】解析的，而这个 Row 自带
   * 左右 8 的 padding、外层列表 Column 又有左右 14 的 padding，
   * 于是 92% 算出来的宽度里包含了这些 padding，再加上 28 的头像和 8 的
   * 间距，一行的总宽就超过了可用内容宽。窄屏因为整体够宽看不出来，
   * 分栏后左栏只有 420vp，溢出的十几 vp 直接被栏宽裁掉 —— 表现为
   * 消息右侧被切了一条（这与 MarkdownView 里 width('100%') 溢出 12vp
   * 被 clip 的问题是同一个成因）。
   *
   * 修法同 MarkdownView：用 layoutWeight(1) 拿"剩余空间"而不是百分比。
   * 槽自身无 padding，外框宽 == 内容宽 == 头像与间距之外的真实可用宽度，
   * 气泡的百分比再相对它解析，无论栏宽多少都不可能溢出。
   */
  @Builder
  BubbleSlot(msg: ChatMessage) {
    Column() {
      this.BubbleBody(msg)
    }
    .layoutWeight(1)
    .alignItems(this.isSelfMsg(msg) ? HorizontalAlign.End : HorizontalAlign.Start)
  }

  @Builder
  MessageBubble(msg: ChatMessage) {
    Row({ space: 8 }) {
      if (this.isChannelMsg(msg)) {
        this.ChanAvatar(msg.source ?? '')
        this.BubbleSlot(msg)
      } else if (msg.role === 'user') {
        this.BubbleSlot(msg)
        this.Avatar(msg.role)
      } else {
        this.Avatar(msg.role)
        this.BubbleSlot(msg)
      }
    }
    .width('100%')
    .alignItems(VerticalAlign.Top)
    // 槽已经用 layoutWeight 吃掉了剩余宽度，这里的对齐实际不再参与分配，
    // 保留是为了兜底：若某处布局退化成非加权分配，方向也仍然正确。
    .justifyContent(this.isSelfMsg(msg) ? FlexAlign.End : FlexAlign.Start)
    .padding({ left: 8, right: 8, top: 3, bottom: 3 })
    // 入场动画：对齐 WebGUI viewIn (opacity 0 -> 1, translateY 6 -> 0)
    .opacity(this.isNewMsg(msg.id) ? 0 : 1)
    .translate({ y: this.isNewMsg(msg.id) ? 8 : 0 })
    .animation({ duration: 180, curve: Curve.EaseOut })
  }

  @Builder
  BubbleBody(msg: ChatMessage) {
    Column() {
      // 渠道来源名（对齐 GUI 的 msg-chan-name）：只有别处来的消息才显示
      if (this.isChannelMsg(msg)) {
        Text(this.chanLabel(msg.source ?? ''))
          .fontSize(10)
          .fontWeight(FontWeight.Medium)
          .fontColor(this.palette().textMuted)
          .margin({ bottom: 4 })
      }

      // Reasoning card (assistant only)
      if (msg.role === 'assistant' && msg.reasoningContent !== undefined && msg.reasoningContent.length > 0) {
        this.ReasoningCard(msg)
      }

      // 附件卡（图片缩略图 / 文件条），点击进入附件详情二级页
      if (msg.attachment !== undefined) {
        AttachmentCard({
          att: msg.attachment,
          mine: msg.role === 'user',
          onTap: () => {
            const a: ChatAttachment | undefined = msg.attachment;
            if (a !== undefined) {
              this.openAttachment(a);
            }
          },
        })
      }

      // Content bubble — 对齐 WebGUI bubbleGrow + textFadeIn
      if (msg.content.length > 0) {
        if (msg.role === 'assistant') {
          MarkdownView({
            content: msg.content,
            isStreaming: msg.isStreaming === true,
            isDark: this.isDark,
          })
        } else {
          Text(msg.content)
            .fontSize(15)
            .lineHeight(24)
            .fontColor(this.palette().msgBubbleText)
            .textAlign(TextAlign.Start)
            .wordBreak(WordBreak.BREAK_ALL)
            .constraintSize({ maxWidth: '100%' })
            .margin({ top: msg.attachment !== undefined ? 8 : 0 })
        }
      }

      // Tool cards
      if (msg.toolCalls !== undefined && msg.toolCalls.length > 0) {
        Column() {
          ForEach(msg.toolCalls, (tc: ToolCallInfo, tci: number) => {
            this.ToolCard(tc)
          }, (tc: ToolCallInfo, tci: number) => tci.toString() + tc.name)
        }
        // 不写 width('100%')：百分比会按气泡外框解析而溢出 12vp 被 clip。
        // 让它自适应，最大宽约束由气泡内容框向下传递，ToolCard 内部用 layoutWeight 取满。
        .alignItems(HorizontalAlign.Start)
        .margin({ top: msg.content.length > 0 ? 6 : 0 })
      }
    }
    .constraintSize({ maxWidth: this.bubbleMaxWidth(msg) })
    .clip(true)
    .padding({ left: 12, right: 12, top: 9, bottom: 9 })
    .backgroundColor(msg.role === 'user' ? this.palette().msgUserBubbleBg : this.palette().msgAssistantBubbleBg)
    .borderRadius({
      topLeft: RADIUS_MD,
      topRight: RADIUS_MD,
      bottomLeft: msg.role === 'assistant' ? 4 : RADIUS_MD,
      bottomRight: msg.role === 'user' ? 4 : RADIUS_MD,
    })
    .border({ width: 1, color: msg.role === 'user' ? this.palette().msgUserBubbleBorder : this.palette().msgAssistantBubbleBorder })
    .shadow({ radius: 8, color: this.palette().shadow, offsetY: 2 })
    .alignItems(HorizontalAlign.Start)
    // bubbleGrow: 气泡入场缩放效果
    .scale({
      x: this.isNewMsg(msg.id) ? 0.95 : 1,
      y: this.isNewMsg(msg.id) ? 0.95 : 1,
    })
    .animation({ duration: 200, curve: Curve.EaseOut })
  }
}
