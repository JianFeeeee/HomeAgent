import { deviceBridge, CmdReply } from './DeviceBridge';
import {
  CapResult,
  capScreensee,
  capClipboardSee,
  capClipboardsue,
  capSpeakerUse,
  capDeviceInfo,
  parseScreensue,
  ScreensuePayload,
} from './BridgeCaps';
import { common } from '@kit.AbilityKit';

// screensue 展示回调由 UI 层注册（Index 挂全局悬浮层）
export type ScreensueHandler = (payload: ScreensuePayload) => void;

let screensueHandler: ScreensueHandler | null = null;
let appContext: common.UIAbilityContext | null = null;

export function registerScreensueHandler(handler: ScreensueHandler): void {
  screensueHandler = handler;
}

export function setBridgeAppContext(ctx: common.UIAbilityContext): void {
  appContext = ctx;
}

/** 解析 homeagent-* 命令：返回能力名与参数串。 */
function splitCapability(command: string): string[] {
  const cmd: string = command.trim();
  const idx: number = cmd.indexOf(' ');
  if (idx < 0) {
    return [cmd];
  }
  const out: string[] = [cmd.substring(0, idx), cmd.substring(idx + 1)];
  return out;
}

async function executeCommand(reqId: string, command: string): Promise<CapResult> {
  const parts: string[] = splitCapability(command);
  const name: string = parts[0];
  const args: string = parts.length > 1 ? parts[1] : '';

  // screensee 截屏回传（data URL 走文本结果，服务端兼容）
  if (name === 'screensee') {
    return capScreensee();
  }
  if (name === 'screensue') {
    const payload: ScreensuePayload = parseScreensue(args);
    if (screensueHandler !== null) {
      screensueHandler(payload);
      return okRes('shown');
    }
    return errRes('screensue: display layer not ready');
  }
  if (name === 'clipboardsee') {
    if (appContext === null) {
      return errRes('clipboardsee: app context missing');
    }
    return capClipboardSee(appContext);
  }
  if (name === 'clipboardsue') {
    if (args.length === 0) {
      return errRes('clipboardsue: empty text');
    }
    return capClipboardsue(args);
  }
  if (name === 'speakeruse') {
    if (args.length === 0) {
      return errRes('speakeruse: empty text');
    }
    return capSpeakerUse(args);
  }
  if (name === 'deviceinfo' || name === 'status') {
    return capDeviceInfo();
  }
  if (name === 'camerasue') {
    return errRes('camerasue: camera capture not supported on this build');
  }
  if (name === 'computeruse') {
    return errRes('computeruse: not applicable to touch-only device');
  }
  return errRes('unsupported homeagent capability: ' + name);
}

function okRes(output: string): CapResult {
  const r: CapResult = { status: 'ok', output: output, error: '' };
  return r;
}

function errRes(errMsg: string): CapResult {
  const r: CapResult = { status: 'error', output: '', error: errMsg };
  return r;
}

/** 安装命令处理器到 bridge 单例。 */
export function installCmdRouter(): void {
  deviceBridge.setCmdHandler(executeCommand);
}
