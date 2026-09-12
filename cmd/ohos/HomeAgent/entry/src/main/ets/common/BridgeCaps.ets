import { image } from '@kit.ImageKit';
import { util } from '@kit.ArkTS';
import { pasteboard } from '@kit.BasicServicesKit';
import { deviceInfo } from '@kit.BasicServicesKit';
import { textToSpeech } from '@kit.CoreSpeechKit';
import { componentSnapshot } from '@kit.ArkUI';
import { abilityAccessCtrl, common, PermissionRequestResult, Permissions } from '@kit.AbilityKit';

// ===== 能力结果 =====

/** 与 BridgeRouter 实际支持的本机命令保持一一对应。 */
export const LOCAL_DEVICE_CAPS: string[] = [
  'status',
  'deviceinfo',
  'screensee',
  'screensue',
  'clipboardsee',
  'clipboardsue',
  'speakeruse',
];

export interface CapResult {
  status: string; // 'ok' | 'error'
  output: string;
  error: string;
}

interface DeviceStatusPayload {
  device_id: string;
  status: string;
  hostname: string;
  platform: string;
  arch: string;
  uptime: number;
}

interface DeviceDetails {
  hostname: string;
  platform: string;
  arch: string;
  os_release: string;
  version: string;
  cpus: number;
  brand: string;
  manufacturer: string;
  model: string;
  series: string;
  sdk_api_version: number;
  security_patch: string;
  abi_list: string;
  device_type: string;
}

interface DeviceInfoPayload {
  device_id: string;
  name: string;
  kind: string;
  caps: string[];
  info: DeviceDetails;
}

const APP_STARTED_AT: number = Date.now();

function okResult(output: string): CapResult {
  const r: CapResult = { status: 'ok', output: output, error: '' };
  return r;
}

function errResult(errMsg: string): CapResult {
  const r: CapResult = { status: 'error', output: '', error: errMsg };
  return r;
}

// ===== screensee：截取本应用当前画面（前台时为整屏可见内容）=====

const SNAPSHOT_COMPONENT_ID: string = 'homeagent-root';

/** 根组件 id（Index 的根 Stack 设置同名 .id()），截屏时按此定位。 */
export function snapshotComponentId(): string {
  return SNAPSHOT_COMPONENT_ID;
}

async function captureScreenPixelMap(): Promise<image.PixelMap> {
  const pm: image.PixelMap = await componentSnapshot.get(SNAPSHOT_COMPONENT_ID);
  return pm;
}

/**
 * screensee 实现：截本应用画面，缩放到最大宽度 720px 后压成 jpeg base64 data URL。
 * 说明：鸿蒙三方应用无法无弹窗截取整个系统屏幕（CUSTOM_SCREEN_CAPTURE 为系统权限），
 * 此处回传应用自身前台画面；应用在前台运行时即为用户正在看到的界面。
 */
export async function capScreensee(): Promise<CapResult> {
  let full: image.PixelMap | null = null;
  let packer: image.ImagePacker | null = null;
  try {
    full = await captureScreenPixelMap();
    const info: image.ImageInfo = await full.getImageInfo();
    const maxW: number = 420;
    const maxH: number = 640;
    let scale: number = 1;
    if (info.size.width > maxW) {
      scale = maxW / info.size.width;
    }
    if (info.size.height * scale > maxH) {
      scale = maxH / info.size.height;
    }
    if (scale < 1) {
      await full.scale(scale, scale);
    }
    packer = image.createImagePacker();
    const opt: image.PackingOption = { format: 'image/jpeg', quality: 55 };
    const packed: ArrayBuffer = await packer.packing(full, opt);
    const helper: util.Base64Helper = new util.Base64Helper();
    const b64: string = helper.encodeToStringSync(new Uint8Array(packed));
    if (b64.length > 950000) {
      return errResult('当前画面数据过大，请稍后重试');
    }
    return okResult('data:image/jpeg;base64,' + b64);
  } catch (e) {
    return errResult('无法读取当前应用画面，请保持应用在前台后重试');
  } finally {
    if (packer !== null) {
      packer.release();
    }
    if (full !== null) {
      full.release();
    }
  }
}

// ===== clipboardsee / clipboardsue =====

const CLIPBOARD_PERMISSIONS: Array<Permissions> = ['ohos.permission.READ_PASTEBOARD'];

/**
 * READ_PASTEBOARD 是 user_grant 权限：仅在 agent 真正请求 clipboardsee 时弹出系统授权，
 * 不在应用启动时抢先索权。已授权时系统会直接返回，不会重复打扰用户。
 */
async function ensureClipboardPermission(context: common.UIAbilityContext): Promise<boolean> {
  try {
    const atManager = abilityAccessCtrl.createAtManager();
    const result: PermissionRequestResult =
      await atManager.requestPermissionsFromUser(context, CLIPBOARD_PERMISSIONS);
    return result.authResults.length > 0 && result.authResults[0] === 0;
  } catch (e) {
    return false;
  }
}

export async function capClipboardSee(context: common.UIAbilityContext): Promise<CapResult> {
  const granted: boolean = await ensureClipboardPermission(context);
  if (!granted) {
    return errResult('剪贴板读取权限未授予，请在系统设置中允许后重试');
  }
  try {
    const clip: pasteboard.SystemPasteboard = pasteboard.getSystemPasteboard();
    const has: boolean = await clip.hasData();
    if (!has) {
      return okResult('');
    }
    const data: pasteboard.PasteData = await clip.getData();
    const txt: string = data.getPrimaryText();
    return okResult(txt ?? '');
  } catch (e) {
    return errResult('剪贴板读取失败，请确认应用在前台并已获得系统授权');
  }
}

export async function capClipboardsue(text: string): Promise<CapResult> {
  try {
    const clip: pasteboard.SystemPasteboard = pasteboard.getSystemPasteboard();
    const data: pasteboard.PasteData = pasteboard.createData(pasteboard.MIMETYPE_TEXT_PLAIN, text);
    await clip.setPasteData(data);
    return okResult('clipboard written');
  } catch (e) {
    return errResult('剪贴板写入失败，请保持应用在前台后重试');
  }
}

// ===== speakeruse：TTS 朗读 =====

class TtsSession {
  private engine: textToSpeech.TextToSpeechEngine | null = null;

  async speak(text: string): Promise<CapResult> {
    if (text.length > 4000) {
      return errResult('朗读内容过长，请缩短到 4000 字以内');
    }
    try {
      if (this.engine === null) {
        const params: textToSpeech.CreateEngineParams = {
          language: 'zh-CN',
          person: 0,
          online: 1,
        };
        const eng: textToSpeech.TextToSpeechEngine = await textToSpeech.createEngine(params);
        this.engine = eng;
      }
      const sp: textToSpeech.SpeakParams = {
        requestId: 'spk-' + Date.now().toString(),
      };
      this.engine.speak(text, sp);
      return okResult('speaking');
    } catch (e) {
      return errResult('语音服务暂时不可用，请稍后重试');
    }
  }

  shutdown(): void {
    if (this.engine !== null) {
      try {
        this.engine.shutdown();
      } catch (e) {
        // ignore
      }
      this.engine = null;
    }
  }
}

const ttsSession: TtsSession = new TtsSession();

export async function capSpeakerUse(text: string): Promise<CapResult> {
  return ttsSession.speak(text);
}

export function shutdownSpeakerUse(): void {
  ttsSession.shutdown();
}

// ===== status / deviceinfo =====

export function capStatus(deviceId: string): CapResult {
  const payload: DeviceStatusPayload = {
    device_id: deviceId,
    status: 'online',
    hostname: 'ohos-phone',
    platform: 'OpenHarmony',
    arch: deviceInfo.abiList,
    uptime: Math.floor((Date.now() - APP_STARTED_AT) / 1000),
  };
  return okResult(JSON.stringify(payload));
}

export function capDeviceInfo(deviceId: string, deviceName: string): CapResult {
  const details: DeviceDetails = {
    hostname: 'ohos-phone',
    platform: 'OpenHarmony',
    arch: deviceInfo.abiList,
    os_release: deviceInfo.osFullName,
    version: '1.1.1',
    cpus: 0,
    brand: deviceInfo.brand,
    manufacturer: deviceInfo.manufacture,
    model: deviceInfo.productModel,
    series: deviceInfo.productSeries,
    sdk_api_version: deviceInfo.sdkApiVersion,
    security_patch: deviceInfo.securityPatchTag,
    abi_list: deviceInfo.abiList,
    device_type: deviceInfo.deviceType,
  };
  const payload: DeviceInfoPayload = {
    device_id: deviceId,
    name: deviceName,
    kind: 'ohos-phone',
    caps: LOCAL_DEVICE_CAPS,
    info: details,
  };
  return okResult(JSON.stringify(payload));
}

// ===== screensue 内容解析 =====
// 服务端协议: screensue [秒] <内容>；0=常驻。

export interface ScreensuePayload {
  duration: number; // 秒；0 表示常驻直到用户关闭
  content: string;
}

export function parseScreensue(rawArgs: string): ScreensuePayload {
  const p: ScreensuePayload = { duration: 5, content: '' };
  const leadingSpaces: RegExp = new RegExp('^\\s+');
  const firstSpace: RegExp = new RegExp('\\s');
  let rest: string = rawArgs.replace(leadingSpaces, '');
  const splitAt: number = rest.search(firstSpace);
  if (splitAt > 0) {
    const first: string = rest.substring(0, splitAt);
    const digits: RegExp = new RegExp('^\\d+$');
    if (digits.test(first)) {
      p.duration = Math.min(parseInt(first, 10), 86400);
      rest = rest.substring(splitAt).replace(leadingSpaces, '');
    }
  }
  p.content = rest;
  return p;
}
