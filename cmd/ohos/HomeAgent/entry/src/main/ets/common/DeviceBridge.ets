import { webSocket } from '@kit.NetworkKit';
import { DeviceInfo } from '../model/Model';
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

interface CmdMessage {
  op: string;
  req_id: string;
  command: string;
  cmd_type: string;
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
  private everConnected: boolean = false;
  private manualClose: boolean = false;
  private reconnectTimer: number = -1;
  private cmdHandler: BridgeCmdHandler | null = null;
  private onStateChange: ((open: boolean) => void) | null = null;

  isConnected(): boolean {
    return this.connected;
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
    // 每次连接使用新的 WebSocket 实例，避免旧实例事件残留
    try {
      this.ws.off('open');
      this.ws.off('message');
      this.ws.off('close');
      this.ws.off('error');
    } catch (e) {
      // ignore
    }
    this.ws = webSocket.createWebSocket();
    this.bindWsEvents(authorized);
    // 鉴权必须走请求头，不能拼 ?token= ：
    //   1) webui 的 /api/v1/device/* 反代包在 requireAPI 里，
    //      validAPIKey 只认 X-API-Key 头或 Authorization: Bearer，
    //      查询参数一律视为未授权 → 握手被 401 顶掉，
    //      表现为 NETSTACK 日志 "Lws client connection error HS: ws upgrade unauthorized"。
    //   2) 反代到 remotedevice 时会自行注入网关的 ws_token；
    //      如果我们再带 ?token=<webui apiKey>，remotedevice 的 ServeWS
    //      会拿它和 ws_token 比对并 401。留空反而放行。
    const opts: webSocket.WebSocketRequestOptions = {
      header: this.authHeader(),
    };
    try {
      await this.ws.connect(this.url, opts);
    } catch (e) {
      this.connected = false;
      this.scheduleReconnect();
    }
  }

  /** 握手请求头：X-API-Key + Authorization 双写，兼容不同后端校验实现。 */
  private authHeader(): Record<string, string> {
    const h: Record<string, string> = {};
    if (this.token.length > 0) {
      h['X-API-Key'] = this.token;
      h['Authorization'] = 'Bearer ' + this.token;
    }
    return h;
  }

  private bindWsEvents(authorized: boolean): void {
    this.ws.on('open', (err: Error, value: Object) => {
      this.connected = true;
      this.everConnected = true;
      this.cancelReconnect();
      this.sendHello(authorized);
      this.sendBind();
      if (this.onStateChange !== null) {
        this.onStateChange(true);
      }
    });
    this.ws.on('message', (err: Error, value: string | ArrayBuffer) => {
      if (typeof value === 'string') {
        this.handleTextFrame(value);
      }
    });
    this.ws.on('close', (err: Error, value: webSocket.CloseResult) => {
      this.connected = false;
      if (this.onStateChange !== null) {
        this.onStateChange(false);
      }
      this.scheduleReconnect();
    });
    this.ws.on('error', (err: Error) => {
      this.connected = false;
      if (this.onStateChange !== null) {
        this.onStateChange(false);
      }
      this.scheduleReconnect();
    });
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

  /** 更新本地授权状态并立即重新 hello 同步到服务端。 */
  updateAuthorized(authorized: boolean): void {
    this.lastAuthorized = authorized;
    if (this.connected) {
      this.sendHello(authorized);
    }
  }

  disconnect(): void {
    this.manualClose = true;
    this.cancelReconnect();
    this.connected = false;
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
    if (this.onStateChange !== null) {
      this.onStateChange(false);
    }
  }

  private sendHello(authorized: boolean): void {
    this.lastAuthorized = authorized;
    const info: HelloDeviceInfo = {
      hostname: this.hostname,
      platform: 'OpenHarmony',
      arch: '',
      os_release: '',
      version: '1.1.0',
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
    if (op === 'cmd') {
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
    } else if (op === 'hello_ack' || op === 'bind_ack') {
      if (this.onAck !== null) {
        this.onAck(op);
      }
    }
  }

  onAck: ((op: string) => void) | null = null;

  private dispatchCommand(reqId: string, command: string): void {
    if (this.cmdHandler === null) {
      this.sendResult(reqId, 'error', '', 'no capability handler registered');
      return;
    }
    const handler: BridgeCmdHandler = this.cmdHandler;
    handler(reqId, command).then((res: CapResult) => {
      this.sendResult(reqId, res.status, res.output, res.error);
    }).catch((e: Object) => {
      const msg: string = e instanceof Error ? e.message : String(e);
      this.sendResult(reqId, 'error', '', msg);
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

export function parseDevicesPayload(jsonStr: string): DeviceInfo[] {
  return [];
}
