import { image } from '@kit.ImageKit';
import { fileIo } from '@kit.CoreFileKit';
import { common } from '@kit.AbilityKit';
import { apiClient } from '../common/ApiClient';
import { userMessage } from '../common/UserError';
import { ChatAttachment } from '../model/Model';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, RADIUS_LG, RADIUS_MD, RADIUS_SM, ANIM_FAST, ANIM_NORMAL, ANIM_ENTER } from '../common/Constants';
import { COLOR_ERROR } from '../common/Constants';
import { MotionBase } from './MotionBase';
import { PlainCard } from './SubPage';

/**
 * 附件解析与展示。
 *
 * 后端 Attachment 只有四个字段：type / url / size / name
 * （internal/plugins/webui/handler.go），没有 mime、没有像素尺寸、没有本地路径。
 * 所以详情页里的"尺寸/格式"必须由客户端自己解码得出，不能假装后端给了。
 *
 * 字节走 GET <base>/files/<name> 或 /uploads/<name>（注意不带 /api/v1 前缀）。
 * 这两条路由在后端是 requireWeb，但对 API Key 客户端同等放行，
 * 所以带上和普通接口一样的鉴权头即可，无需 web 登录态。
 */

/** 从后端 JSON 里解析 attachment 字段；缺字段或类型不对则返回 undefined。 */
export function parseAttachment(raw: Object | undefined): ChatAttachment | undefined {
  if (raw === undefined || raw === null) {
    return undefined;
  }
  const o: Record<string, Object> = raw as Record<string, Object>;
  const url: string = o['url'] as string ?? '';
  if (url.length === 0) {
    return undefined;
  }
  const t: string = o['type'] as string ?? 'file';
  const a: ChatAttachment = {
    type: t === 'image' ? 'image' : 'file',
    url: url,
    size: o['size'] as number ?? 0,
    name: o['name'] as string ?? fileNameOf(url),
  };
  return a;
}

/** 由 SSE channel_output 事件构造附件（字段名与 history 不同）。 */
export function attachmentFromChannelOutput(
  outputType: string, url: string, size: number): ChatAttachment | undefined {
  if (url.length === 0) {
    return undefined;
  }
  if (outputType !== 'image' && outputType !== 'file') {
    return undefined;
  }
  const a: ChatAttachment = {
    type: outputType,
    url: url,
    size: size,
    name: fileNameOf(url),
  };
  return a;
}

/** 取 URL 最后一段作为展示文件名，与后端 handler.go 的取名方式一致。 */
export function fileNameOf(url: string): string {
  let s: string = url;
  const q: number = s.indexOf('?');
  if (q >= 0) {
    s = s.substring(0, q);
  }
  const i: number = s.lastIndexOf('/');
  const name: string = i >= 0 ? s.substring(i + 1) : s;
  return name.length > 0 ? name : '附件';
}

/** 人类可读字节数，口径对齐后端 formatBytes（KB 以上保留一位小数）。 */
export function formatBytes(n: number): string {
  if (n <= 0) {
    return '';
  }
  if (n < 1024) {
    return n.toString() + ' B';
  }
  const kb: number = n / 1024;
  if (kb < 1024) {
    return oneDecimal(kb) + ' KB';
  }
  const mb: number = kb / 1024;
  if (mb < 1024) {
    return oneDecimal(mb) + ' MB';
  }
  return oneDecimal(mb / 1024) + ' GB';
}

function oneDecimal(v: number): string {
  return (Math.round(v * 10) / 10).toString();
}

/** 由文件名后缀猜测类型标签。后端不返回 mime，只能这样标注。 */
export function extLabel(name: string): string {
  const i: number = name.lastIndexOf('.');
  if (i < 0 || i === name.length - 1) {
    return '未知类型';
  }
  return name.substring(i + 1).toUpperCase();
}

/**
 * 气泡内的附件卡：图片显示缩略图，文件显示一枚文件条。
 * 点击进入附件详情二级页面（WebGUI 是新开标签页，移动端改为二级页）。
 */
@Component
export struct AttachmentCard {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop att: ChatAttachment;
  @Prop mine: boolean = false;
  onTap?: () => void;
  @State private pixel: image.PixelMap | undefined = undefined;
  @State private failed: boolean = false;

  aboutToAppear(): void {
    if (this.att.type === 'image') {
      this.loadThumb();
    }
  }

  private async loadThumb(): Promise<void> {
    const pm: image.PixelMap | undefined = await loadPixelMap(this.att.url);
    if (pm === undefined) {
      this.failed = true;
      return;
    }
    this.pixel = pm;
  }

  build() {
    // 整卡按压反馈统一收进 MotionBase（父组件）：scale 回弹 + 透明度压暗。
    // fillWidth: false —— 附件卡在聊天气泡内按内容自适应宽度，不能撑满整行。
    MotionBase({ pressEnabled: true, pressOpacity: 0.88, fillWidth: false }) {
    Column({ space: 6 }) {
      Text(this.mine ? '你发送的' : '小宅发送的')
        .fontSize(11)
        .fontColor(this.palette().textMuted)

      if (this.att.type === 'image') {
        if (this.pixel !== undefined) {
          Image(this.pixel)
            .width('100%')
            .constraintSize({ maxHeight: 220 })
            .objectFit(ImageFit.Cover)
            .borderRadius(RADIUS_MD)
            .draggable(false)
            .transition(TransitionEffect.OPACITY
              .combine(TransitionEffect.scale({ x: 0.98, y: 0.98 }))
              .animation({ duration: ANIM_ENTER, curve: Curve.EaseOut }))
        } else if (this.failed) {
          Row({ space: 6 }) {
            Image($r('app.media.ic_error'))
              .width(14)
              .height(14)
              .fillColor(COLOR_ERROR)
              .draggable(false)
            Text('图片加载失败')
              .fontSize(12)
              .fontColor(COLOR_ERROR)
          }
          .padding({ left: 10, right: 10, top: 8, bottom: 8 })
          .borderRadius(RADIUS_SM)
          .backgroundColor(this.palette().bgHover)
          .transition(TransitionEffect.OPACITY.animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
        } else {
          Row() {
            LoadingProgress()
              .width(20)
              .height(20)
              .color(this.palette().accent)
          }
          .width(120)
          .height(80)
          .justifyContent(FlexAlign.Center)
          .borderRadius(RADIUS_SM)
          .backgroundColor(this.palette().bgHover)
          .transition(TransitionEffect.OPACITY.animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
        }
      } else {
        Row({ space: 8 }) {
          Image($r('app.media.ic_file'))
            .width(18)
            .height(18)
            .fillColor(this.palette().accent)
            .draggable(false)
          Column({ space: 2 }) {
            Text(this.att.name)
              .fontSize(13)
              .fontColor(this.palette().textPrimary)
              .maxLines(1)
              .textOverflow({ overflow: TextOverflow.Ellipsis })
            if (this.att.size > 0) {
              Text(formatBytes(this.att.size))
                .fontSize(10)
                .fontColor(this.palette().textMuted)
            }
          }
          .alignItems(HorizontalAlign.Start)
          .layoutWeight(1)
          Image($r('app.media.ic_chevron_right'))
            .width(14)
            .height(14)
            .fillColor(this.palette().textMuted)
            .draggable(false)
        }
        .constraintSize({ minWidth: 180 })
        .padding({ left: 10, right: 10, top: 8, bottom: 8 })
        .borderRadius(RADIUS_SM)
        .backgroundColor(this.mine ? this.palette().accentBg : this.palette().bgHover)
        .alignItems(VerticalAlign.Center)
      }
    }
    .alignItems(HorizontalAlign.Start)
    .onClick(() => {
      const cb = this.onTap;
      if (cb !== undefined) {
        cb();
      }
    })
    }
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }
}

/**
 * 附件详情二级页面内容：大图预览 / 文件信息 + 保存到本地。
 *
 * 后端不提供尺寸与 mime，图片尺寸由本地解码得到，类型标签由后缀推断，
 * 界面上如实标注它们的来源，不谎称是服务端元数据。
 */
@Component
export struct AttachmentDetailContent {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop att: ChatAttachment;
  @State private pixel: image.PixelMap | undefined = undefined;
  @State private pxWidth: number = 0;
  @State private pxHeight: number = 0;
  @State private loading: boolean = true;
  @State private err: string = '';
  @State private savedPath: string = '';

  aboutToAppear(): void {
    if (this.att.type === 'image') {
      this.loadFull();
    } else {
      this.loading = false;
    }
  }

  private async loadFull(): Promise<void> {
    this.loading = true;
    this.err = '';
    try {
      const abs: string = apiClient.absoluteUrl(this.att.url);
      const resp = await apiClient.getBinary(abs, 20000);
      const src: image.ImageSource = image.createImageSource(resp.data);
      const info: image.ImageInfo = await src.getImageInfo();
      this.pxWidth = info.size.width;
      this.pxHeight = info.size.height;
      this.pixel = await src.createPixelMap();
      await src.release();
    } catch (e) {
      this.err = userMessage('attachment.load', e);
    }
    this.getUIContext().animateTo({ duration: ANIM_ENTER, curve: Curve.EaseOut }, () => {
      this.loading = false;
    });
  }

  /** 保存到应用沙箱 files 目录（不申请媒体库权限，避免为了看一张图要授权相册）。 */
  private async saveToSandbox(): Promise<void> {
    try {
      const abs: string = apiClient.absoluteUrl(this.att.url);
      const resp = await apiClient.getBinary(abs, 30000);
      const ctx = getContext(this) as common.UIAbilityContext;
      const dest: string = ctx.filesDir + '/' + sanitize(this.att.name);
      const f = fileIo.openSync(dest,
        fileIo.OpenMode.READ_WRITE | fileIo.OpenMode.CREATE | fileIo.OpenMode.TRUNC);
      fileIo.writeSync(f.fd, resp.data);
      fileIo.closeSync(f);
      this.savedPath = dest;
    } catch (e) {
      this.err = userMessage('attachment.save', e);
    }
  }

  build() {
    Column() {
      if (this.err.length > 0) {
        Row({ space: 8 }) {
          Image($r('app.media.ic_error'))
            .width(16)
            .height(16)
            .fillColor(COLOR_ERROR)
            .draggable(false)
          Text(this.err)
            .fontSize(13)
            .fontColor(COLOR_ERROR)
            .layoutWeight(1)
        }
        .width('100%')
        .padding(16)
        .borderRadius(RADIUS_LG)
        .backgroundColor(this.palette().bgCard)
        .border({ width: 1, color: this.palette().glassBorder })
        .alignItems(VerticalAlign.Top)
        .margin({ bottom: 14 })
        .transition(TransitionEffect.OPACITY
          .combine(TransitionEffect.translate({ y: -12 }))
          .animation({ duration: ANIM_ENTER, curve: Curve.EaseOut }))
      }

      // 预览：图片铺满宽度可缩放查看；文件给一个大图标占位
      Column() {
        if (this.att.type === 'image') {
          if (this.loading) {
            Row() {
              LoadingProgress()
                .width(28)
                .height(28)
                .color(this.palette().accent)
            }
            .width('100%')
            .height(200)
            .justifyContent(FlexAlign.Center)
            .transition(TransitionEffect.OPACITY.animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
          } else if (this.pixel !== undefined) {
            Image(this.pixel)
              .width('100%')
              .constraintSize({ maxHeight: 420 })
              .objectFit(ImageFit.Contain)
              .borderRadius(RADIUS_MD)
              .draggable(false)
              .transition(TransitionEffect.OPACITY.animation({ duration: ANIM_ENTER, curve: Curve.EaseOut }))
          }
        } else {
          Column({ space: 10 }) {
            Image($r('app.media.ic_file'))
              .width(46)
              .height(46)
              .fillColor(this.palette().accent)
              .draggable(false)
            Text(extLabel(this.att.name))
              .fontSize(12)
              .fontColor(this.palette().textSecondary)
          }
          .width('100%')
          .padding({ top: 26, bottom: 26 })
          .justifyContent(FlexAlign.Center)
          .alignItems(HorizontalAlign.Center)
        }
      }
      .width('100%')
      .padding(12)
      .borderRadius(RADIUS_LG)
      .backgroundColor(this.palette().bgCard)
      .border({ width: 1, color: this.palette().glassBorder })
      .margin({ bottom: 14 })

      PlainCard({ caption: '信息' }) {
        this.kv('文件名', this.att.name)
        this.kv('类型', this.att.type === 'image' ? '图片' : '文件')
        this.kv('格式', extLabel(this.att.name))
        this.kv('大小', this.att.size > 0 ? formatBytes(this.att.size) : '未知（远程链接）')
        if (this.pxWidth > 0 && this.pxHeight > 0) {
          // 后端不返回像素尺寸，这一行来自本地解码
          this.kv('像素', this.pxWidth.toString() + ' × ' + this.pxHeight.toString())
        }
        this.kv('来源', this.att.url.startsWith('http') ? '远程链接' : '服务端中转')
      }

      PlainCard({ caption: '操作' }) {
        Button('保存到应用目录')
          .width('100%')
          .height(38)
          .fontSize(13)
          .backgroundColor(this.palette().accent)
          .fontColor('#FFFFFF')
          .onClick(() => {
            this.saveToSandbox();
          })
        if (this.savedPath.length > 0) {
          Text('已保存：' + this.savedPath)
            .fontSize(11)
            .fontColor(this.palette().textMuted)
            .width('100%')
            .margin({ top: 8 })
            .transition(TransitionEffect.OPACITY
              .combine(TransitionEffect.translate({ y: 6 }))
              .animation({ duration: ANIM_ENTER, curve: Curve.EaseOut }))
        }
      }
    }
    .width('100%')
    .alignItems(HorizontalAlign.Start)
  }

  @Builder
  kv(label: string, value: string) {
    Row() {
      Text(label)
        .fontSize(13)
        .fontColor(this.palette().textSecondary)
        .layoutWeight(1)
      Text(value)
        .fontSize(13)
        .fontColor(this.palette().textPrimary)
        .textAlign(TextAlign.End)
        .maxLines(2)
        .textOverflow({ overflow: TextOverflow.Ellipsis })
        .constraintSize({ maxWidth: 210 })
        .margin({ left: 16 })
    }
    .width('100%')
    .padding({ top: 8, bottom: 8 })
    .alignItems(VerticalAlign.Top)
    .transition(TransitionEffect.OPACITY.animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut }))
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }
}

/** 下载并解码成 PixelMap；任何一步失败都返回 undefined（调用方显示占位）。 */
async function loadPixelMap(url: string): Promise<image.PixelMap | undefined> {
  try {
    // 本地待上传的图片：直接读沙箱文件，不走网络
    if (url.startsWith('file://')) {
      const path: string = url.substring(7);
      const f = fileIo.openSync(path, fileIo.OpenMode.READ_ONLY);
      const localSrc: image.ImageSource = image.createImageSource(f.fd);
      const localPm: image.PixelMap = await localSrc.createPixelMap();
      await localSrc.release();
      fileIo.closeSync(f);
      return localPm;
    }
    const abs: string = apiClient.absoluteUrl(url);
    const resp = await apiClient.getBinary(abs, 15000);
    const src: image.ImageSource = image.createImageSource(resp.data);
    const pm: image.PixelMap = await src.createPixelMap();
    await src.release();
    return pm;
  } catch (e) {
    return undefined;
  }
}

/** 去掉路径分隔符，避免附件名把文件写到 filesDir 之外。 */
function sanitize(name: string): string {
  let s: string = name.replace(/[\/\\:*?"<>|]/g, '_');
  if (s.length === 0) {
    s = 'attachment';
  }
  return s;
}
