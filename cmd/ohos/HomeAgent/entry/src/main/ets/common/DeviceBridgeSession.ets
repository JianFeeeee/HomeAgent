import { common } from '@kit.AbilityKit';
import { deviceBridge } from './DeviceBridge';
import { installCmdRouter, setBridgeAppContext } from './BridgeRouter';
import { LOCAL_DEVICE_CAPS, shutdownSpeakerUse } from './BridgeCaps';
import { connStore } from './ConnStore';
import { ConnectionConfig } from '../model/Model';

export { LOCAL_DEVICE_CAPS } from './BridgeCaps';

let bridgeStarting: boolean = false;
let foregroundActive: boolean = false;
let rootUIReady: boolean = false;
let bridgeGeneration: number = 0;
let stateTrackingReady: boolean = false;

function ensureBridgeStateTracking(): void {
  if (stateTrackingReady) {
    return;
  }
  stateTrackingReady = true;
  AppStorage.setOrCreate<boolean>('deviceBridgeConnected', false);
  deviceBridge.setStateListener((open: boolean) => {
    AppStorage.set<boolean>('deviceBridgeConnected', open);
  });
}

/** 把当前后端 HTTP 地址转换为同源设备桥 WebSocket 地址。 */
export function deviceGatewayUrl(base: string): string {
  let trimmed: string = base.trim();
  while (trimmed.length > 0 && trimmed.charAt(trimmed.length - 1) === '/') {
    trimmed = trimmed.substring(0, trimmed.length - 1);
  }
  let scheme: string = 'ws://';
  let rest: string = trimmed;
  if (trimmed.startsWith('https://')) {
    scheme = 'wss://';
    rest = trimmed.substring('https://'.length);
  } else if (trimmed.startsWith('http://')) {
    rest = trimmed.substring('http://'.length);
  } else if (trimmed.startsWith('wss://')) {
    scheme = 'wss://';
    rest = trimmed.substring('wss://'.length);
  } else if (trimmed.startsWith('ws://')) {
    rest = trimmed.substring('ws://'.length);
  }
  return scheme + rest + '/api/v1/device/ws';
}

/**
 * 应用进入前台后建立全局设备桥。它不再依赖用户先打开“设备”Tab，
 * 因而 screensue、clipboardsee 等前台能力从主页面加载后即可接收。
 */
export async function startForegroundBridge(context: common.UIAbilityContext): Promise<void> {
  foregroundActive = true;
  setBridgeAppContext(context);
  installCmdRouter();
  ensureBridgeStateTracking();
  if (!rootUIReady || deviceBridge.isConnected() || bridgeStarting) {
    return;
  }
  const cur: ConnectionConfig | null = connStore.getCurrentConnection();
  if (cur === null || cur.url.length === 0 || cur.apiKey.length === 0) {
    return;
  }
  bridgeStarting = true;
  bridgeGeneration = bridgeGeneration + 1;
  const generation: number = bridgeGeneration;
  const deviceId: string = connStore.ensureDeviceId();
  try {
    await deviceBridge.connect(
      deviceGatewayUrl(cur.url), cur.apiKey, deviceId,
      LOCAL_DEVICE_CAPS, 'ohos-phone', connStore.getDeviceAuth(), connStore.getDeviceName());
    if (!foregroundActive || generation !== bridgeGeneration) {
      deviceBridge.disconnect();
    }
  } catch (e) {
    // DeviceBridge 自己会安排重连；前台启动不弹技术错误打扰用户。
  }
  if (generation === bridgeGeneration) {
    bridgeStarting = false;
  }
}

/** 根页面挂载完成后才连接，避免首条 screensue 到达时展示层尚未注册。 */
export function markForegroundBridgeUIReady(context: common.UIAbilityContext): void {
  rootUIReady = true;
  startForegroundBridge(context);
}

/** 后台不接受需要前台 UI/剪贴板授权的命令。 */
export function stopForegroundBridge(): void {
  foregroundActive = false;
  bridgeGeneration = bridgeGeneration + 1;
  bridgeStarting = false;
  shutdownSpeakerUse();
  deviceBridge.disconnect();
}

/** 连接配置切换或修改后立即让设备桥使用新地址和 Token。 */
export async function restartForegroundBridge(context: common.UIAbilityContext): Promise<void> {
  bridgeGeneration = bridgeGeneration + 1;
  bridgeStarting = false;
  deviceBridge.disconnect();
  await startForegroundBridge(context);
}
