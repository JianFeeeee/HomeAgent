import { webSocket } from '@kit.NetworkKit';
import { CapResult } from './BridgeCaps';

// ===== 协议消息（与 remotedevice 插件对齐）=====

interface HelloDeviceInfo {
  hostname: string;
  platform: string;
  arch: string;
  os_release: string;
  version: string;
  cpus: number;
}

interface HelloDevice {
  device_id: string;
  name: string;
  kind: string;
  authorized: boolean;
  caps: string[];
  info: HelloDeviceInfo;
}

interface HelloMessage {
  op: string;
  device: HelloDevice;
}

interface BindMessage {
  op: string;
  device_id: string;
  token: string;
}

export interface CmdReply {
  op: string; // 'cmd_result'
  req_id: string;
  status: string;
  output: string;
  error: string;
}

interface DataStartMessage {
  op: string;
  req_id: string;
  kind: string;
  mime: string;
  total: number;
  chunk_size: number;
}

interface DataEndMessage {
  op: string;
  req_id: string;
  status: string;
  total?: number;
  error?: string;
}

const CHUNK_SIZE: number = 8192;

// ===== 命令处理器回调 =====
// 返回 CapResult；二进制大结果通过 dataHandler 分块回传。
export type BridgeCmdHandler = (reqId: string, command: string) => Promise<CapResult>;

export class DeviceBridgeClient {
  private ws: webSocket.WebSocket = webSocket.createWebSocket();
  private url: string = '';
  private token: string = '';
  private deviceId: string = '';
  private name: string = 'HomeAgent OHOS';
  private kind: string = 'phone';
  private caps: string[] = [];
  private hostname: string = 'ohos';
  private connected: boolean = false;
  private bound: boolean = false;
  private manualClose: boolean = false;
  private reconnectTimer: number = -1;
  private connectionGeneration: number = 0;
  private cmdHandler: BridgeCmdHandler | null = null;
  private onStateChange: ((open: boolean) => void) | null = null;

  isConnected(): boolean {
    return this.connected && this.bound;
  }

  getDeviceId(): string {
    return this.deviceId;
  }

  setCmdHandler(handler: BridgeCmdHandler): void {
    this.cmdHandler = handler;
  }

  setStateListener(listener: (open: boolean) => void): void {
    this.onStateChange = listener;
  }

  async connect(url: string, token: string, deviceId: string,
                caps: string[], hostname: string,
                authorized: boolean, name: string): Promise<void> {
    this.url = url;
    this.token = token;
    this.deviceId = deviceId;
    this.caps = caps;
    this.hostname = hostname;
    this.name = name;
    this.manualClose = false;
    await this.openAndRegister(authorized);
  }

  private async openAndRegister(authorized: boolean): Promise<void> {
    try {
      this.ws.off('open');
      this.ws.off('message');
      this.ws.off('close');
      this.ws.off('error');
      this.ws.close().catch(() => {
        // ignore stale socket close failure
      });
    } catch (e) {
      // ignore stale socket cleanup failure
    }
    this.connectionGeneration = this.connectionGeneration + 1;
    const generation: number = this.connectionGeneration;
    const socket: webSocket.WebSocket = webSocket.createWebSocket();
    this.ws = socket;
    this.connected = false;
    this.bound = false;
    this.bindWsEvents(socket, authorized, generation);
    const opts: webSocket.WebSocketRequestOptions = {
      header: this.authHeader(),
    };
    try {
      await socket.connect(this.url, opts);
    } catch (e) {
      if (generation === this.connectionGeneration && !this.manualClose) {
        this.connected = false;
        this.bound = false;
        this.notifyState(false);
        this.scheduleReconnect();
      }
    }
  }

  /** WebUI 用 API key 验证外层连接，并由反代向设备网关注入其内部 token。 */
  private authHeader(): Record<string, string> {
    const h: Record<string, string> = {};
    if (this.token.length > 0) {
      h['X-API-Key'] = this.token;
      h['Authorization'] = 'Bearer ' + this.token;
    }
    return h;
  }

  private bindWsEvents(socket: webSocket.WebSocket, authorized: boolean, generation: number): void {
    socket.on('open', (err: Error, value: Object) => {
      if (generation !== this.connectionGeneration || this.manualClose) {
        socket.close().catch(() => {
          // ignore stale socket close failure
        });
        return;
      }
      this.connected = true;
      this.bound = false;
      this.sendHello(authorized);
      this.sendBind();
    });
    socket.on('message', (err: Error, value: string | ArrayBuffer) => {
      if (generation === this.connectionGeneration && typeof value === 'string') {
        this.handleTextFrame(value);
      }
    });
    socket.on('close', (err: Error, value: webSocket.CloseResult) => {
      this.handleSocketEnd(generation);
    });
    socket.on('error', (err: Error) => {
      this.handleSocketEnd(generation);
    });
  }

  private handleSocketEnd(generation: number): void {
    if (generation !== this.connectionGeneration) {
      return;
    }
    this.connected = false;
    this.bound = false;
    this.notifyState(false);
    this.scheduleReconnect();
  }

  private notifyState(open: boolean): void {
    if (this.onStateChange !== null) {
      this.onStateChange(open);
    }
  }

  private scheduleReconnect(): void {
    if (this.manualClose || this.reconnectTimer >= 0) {
      return;
    }
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = -1;
      if (this.manualClose || this.url.length === 0) {
        return;
      }
      this.openAndRegister(this.lastAuthorized);
    }, 5000);
  }

  private lastAuthorized: boolean = false;

  private cancelReconnect(): void {
    if (this.reconnectTimer >= 0) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = -1;
    }
  }

  /** 更新本地授权状态并在已绑定连接上同步到服务端。 */
  updateAuthorized(authorized: boolean): void {
    this.lastAuthorized = authorized;
    if (this.connected && this.bound) {
      this.sendHello(authorized);
    }
  }

  disconnect(): void {
    this.manualClose = true;
    this.connectionGeneration = this.connectionGeneration + 1;
    this.cancelReconnect();
    this.connected = false;
    this.bound = false;
    try {
      this.ws.off('open');
      this.ws.off('message');
      this.ws.off('close');
      this.ws.off('error');
      this.ws.close().catch(() => {
        // ignore
      });
    } catch (e) {
      // ignore
    }
    this.notifyState(false);
  }

  private sendHello(authorized: boolean): void {
    this.lastAuthorized = authorized;
    const info: HelloDeviceInfo = {
      hostname: this.hostname,
      platform: 'OpenHarmony',
      arch: '',
      os_release: '',
      version: '1.1.1',
      cpus: 0,
    };
    const device: HelloDevice = {
      device_id: this.deviceId,
      name: this.name,
      kind: this.kind,
      authorized: authorized,
      caps: this.caps,
      info: info,
    };
    const hello: HelloMessage = { op: 'hello', device: device };
    this.send(JSON.stringify(hello));
  }

  private sendBind(): void {
    const bind: BindMessage = {
      op: 'bind',
      device_id: this.deviceId,
      token: this.token,
    };
    this.send(JSON.stringify(bind));
  }

  // ===== 命令处理 =====

  private handleTextFrame(text: string): void {
    let obj: Record<string, Object>;
    try {
      obj = JSON.parse(text) as Record<string, Object>;
    } catch (e) {
      return;
    }
    const op: string = obj['op'] as string ?? '';
    if (op === 'bind_ack') {
      const accepted: boolean = obj['ok'] === true;
      if (accepted && this.connected && !this.manualClose) {
        this.bound = true;
        this.cancelReconnect();
        this.notifyState(true);
      } else {
        this.bound = false;
        this.notifyState(false);
        try {
          this.ws.close().catch(() => {
            // ignore bind rejection close failure
          });
        } catch (e) {
          this.scheduleReconnect();
        }
      }
      return;
    }
    if (op !== 'cmd' || !this.bound) {
      return;
    }
    const reqId: string = obj['req_id'] as string ?? '';
    const command: string = obj['command'] as string ?? '';
    if (reqId.length === 0 || command.length === 0) {
      return;
    }
    if (!this.lastAuthorized) {
      this.sendResult(reqId, 'error', '', '设备未授权：请在设备页开启远程控制授权');
      return;
    }
    this.dispatchCommand(reqId, command);
  }

  private dispatchCommand(reqId: string, command: string): void {
    if (this.cmdHandler === null) {
      this.sendResult(reqId, 'error', '', '本机能力尚未就绪，请保持应用在前台后重试');
      return;
    }
    const handler: BridgeCmdHandler = this.cmdHandler;
    handler(reqId, command).then((res: CapResult) => {
      this.sendResult(reqId, res.status, res.output, res.error);
    }).catch((e: Object) => {
      this.sendResult(reqId, 'error', '', '本机能力执行失败，请稍后重试');
    });
  }

  sendResult(reqId: string, status: string, output: string, errMsg: string): void {
    const result: CmdReply = {
      op: 'cmd_result',
      req_id: reqId,
      status: status,
      output: output,
      error: errMsg,
    };
    this.send(JSON.stringify(result));
  }

  // ===== 二进制分块回传（协议与 GUI 客户端一致）=====

  sendDataChunked(reqId: string, kind: string, mime: string, bytes: Uint8Array): void {
    const startMsg: DataStartMessage = {
      op: 'cmd_data_start',
      req_id: reqId,
      kind: kind,
      mime: mime,
      total: bytes.byteLength,
      chunk_size: CHUNK_SIZE,
    };
    this.send(JSON.stringify(startMsg));
    for (let off: number = 0; off < bytes.byteLength; off += CHUNK_SIZE) {
      const end: number = Math.min(off + CHUNK_SIZE, bytes.byteLength);
      const view: Uint8Array = bytes.slice(off, end);
      const ab: ArrayBuffer = view.buffer as ArrayBuffer;
      try {
        this.ws.send(ab).catch(() => {
          // ignore per-chunk failure; end frame reports error below
        });
      } catch (e) {
        break;
      }
    }
    const endMsg: DataEndMessage = {
      op: 'cmd_data_end',
      req_id: reqId,
      status: 'ok',
    };
    this.send(JSON.stringify(endMsg));
  }

  sendEvent(eventType: string, detail: string): void {
    const payload: Record<string, string> = { 'detail': detail };
    const msg: Record<string, Object> = {
      'op': 'event',
      'device_id': this.deviceId,
      'type': eventType,
      'payload': payload,
    };
    this.send(JSON.stringify(msg));
  }

  sendStatus(status: string): void {
    const msg: Record<string, Object> = {
      'op': 'status',
      'device_id': this.deviceId,
      'status': status,
    };
    this.send(JSON.stringify(msg));
  }

  send(text: string): void {
    if (!this.connected) {
      return;
    }
    this.ws.send(text).catch(() => {
      // ignore
    });
  }
}

export const deviceBridge: DeviceBridgeClient = new DeviceBridgeClient();
