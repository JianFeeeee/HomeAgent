import { image } from '@kit.ImageKit';
import { util } from '@kit.ArkTS';
import { pasteboard } from '@kit.BasicServicesKit';
import { deviceInfo } from '@kit.BasicServicesKit';
import { textToSpeech } from '@kit.CoreSpeechKit';
import { componentSnapshot } from '@kit.ArkUI';
import { common } from '@kit.AbilityKit';

// ===== 能力结果 =====

export interface CapResult {
  status: string; // 'ok' | 'error'
  output: string;
  error: string;
}

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
  try {
    const full: image.PixelMap = await captureScreenPixelMap();
    const info: image.ImageInfo = await full.getImageInfo();
    const maxW: number = 720;
    let targetW: number = info.size.width;
    let targetH: number = info.size.height;
    if (targetW > maxW) {
      targetH = Math.floor(targetH * maxW / targetW);
      targetW = maxW;
    }
    let packed: ArrayBuffer;
    if (targetW !== info.size.width) {
      await full.scale(targetW / info.size.width, targetH / info.size.height);
    }
    const packer: image.ImagePacker = image.createImagePacker();
    const opt: image.PackingOption = { format: 'image/jpeg', quality: 70 };
    packed = await packer.packing(full, opt);
    packer.release();
    full.release();
    const helper: util.Base64Helper = new util.Base64Helper();
    const b64: string = helper.encodeToStringSync(new Uint8Array(packed));
    return okResult('data:image/jpeg;base64,' + b64);
  } catch (e) {
    const msg: string = e instanceof Error ? e.message : String(e);
    return errResult('screensee failed: ' + msg);
  }
}

// ===== clipboardsee / clipboardsue =====

export async function capClipboardSee(context: common.UIAbilityContext): Promise<CapResult> {
  // 说明：READ_PASTEBOARD 为受限权限，调试签名无法在真机安装时授予，
  // 这里直接尝试读取；系统拒绝时回错误信息。
  try {
    const clip: pasteboard.SystemPasteboard = pasteboard.getSystemPasteboard();
    const has: boolean = await clip.hasData();
    if (!has) {
      const empty: CapResult = { status: 'ok', output: '', error: '' };
      return empty;
    }
    const data: pasteboard.PasteData = await clip.getData();
    const txt: string = data.getPrimaryText();
    const out: CapResult = { status: 'ok', output: txt ?? '', error: '' };
    return out;
  } catch (e) {
    const msg: string = e instanceof Error ? e.message : String(e);
    return errResult('clipboardsee failed (需系统剪贴板授权): ' + msg);
  }
}

export async function capClipboardsue(text: string): Promise<CapResult> {
  try {
    const clip: pasteboard.SystemPasteboard = pasteboard.getSystemPasteboard();
    const data: pasteboard.PasteData = pasteboard.createData(pasteboard.MIMETYPE_TEXT_PLAIN, text);
    await clip.setPasteData(data);
    return okResult('written ' + text.length + ' chars');
  } catch (e) {
    const msg: string = e instanceof Error ? e.message : String(e);
    return errResult('clipboardsue failed: ' + msg);
  }
}

// ===== speakeruse：TTS 朗读 =====

class TtsSession {
  private engine: textToSpeech.TextToSpeechEngine | null = null;

  async speak(text: string): Promise<CapResult> {
    try {
      if (this.engine === null) {
        const extra: Record<string, Object> = {
          'style': 'interaction-broadcast',
          'locate': 'CN',
          'name': 'EngineName',
        };
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
      const msg: string = e instanceof Error ? e.message : String(e);
      return errResult('speakeruse failed: ' + msg);
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

// ===== deviceinfo =====

export function capDeviceInfo(): CapResult {
  const lines: string[] = [];
  lines.push('brand=' + deviceInfo.brand);
  lines.push('manufacturer=' + deviceInfo.manufacture);
  lines.push('model=' + deviceInfo.productModel);
  lines.push('series=' + deviceInfo.productSeries);
  lines.push('osFullName=' + deviceInfo.osFullName);
  lines.push('sdkApiVersion=' + deviceInfo.sdkApiVersion.toString());
  lines.push('securityPatch=' + deviceInfo.securityPatchTag);
  lines.push('abiList=' + deviceInfo.abiList);
  lines.push('deviceType=' + deviceInfo.deviceType);
  return okResult(lines.join('\n'));
}

// ===== screensue 内容解析 =====
// 服务端协议: screensue [秒] <内容>；0=常驻。

export interface ScreensuePayload {
  duration: number; // 秒；0 表示常驻直到用户关闭
  content: string;
}

export function parseScreensue(rawArgs: string): ScreensuePayload {
  const p: ScreensuePayload = { duration: 5, content: '' };
  let rest: string = rawArgs.trim();
  const tokens: string[] = rest.split(/\s+/);
  if (tokens.length > 1 && /^\d+$/.test(tokens[0])) {
    p.duration = parseInt(tokens[0], 10);
    rest = tokens.slice(1).join(' ');
  } else {
    rest = tokens.join(' ');
  }
  p.content = rest.trim();
  return p;
}
