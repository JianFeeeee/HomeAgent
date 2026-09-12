import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, ANIM_NORMAL } from '../common/Constants';
import { GradientBackground } from './GradientBackground';
import { PageTopBar } from './PageTopBar';
import { MotionBase } from './MotionBase';

/**
 * agent 主动推送的前台内容页。
 *
 * 调用方负责决定页面宽度：窄屏占满窗口，宽屏只占右侧内容栏，
 * 从而让左侧一级页面和主导航保持可见、可操作。
 */
@Component
export struct ScreensuePage {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @Prop pushedText: string = '';
  @Prop countdown: number = 0;
  onClose: () => void = () => {
  };

  build() {
    Stack({ alignContent: Alignment.Bottom }) {
      GradientBackground()

      Scroll() {
        Column({ space: 14 }) {
          Row({ space: 8 }) {
            Circle({ width: 8, height: 8 })
              .fill(this.palette().accent)
            Text('agent 推送')
              .fontSize(12)
              .fontWeight(FontWeight.Medium)
              .fontColor(this.palette().textSecondary)
            Blank()
            if (this.countdown > 0) {
              Text(this.countdown.toString() + 's')
                .fontSize(12)
                .fontColor(this.palette().textMuted)
            } else {
              Text('常驻')
                .fontSize(12)
                .fontColor(this.palette().textMuted)
            }
          }
          .width('100%')

          Column() {
            Text(this.pushedText)
              .fontSize(16)
              .lineHeight(25)
              .fontColor(this.palette().textPrimary)
              .width('100%')
              .textAlign(TextAlign.Start)
              .copyOption(CopyOptions.LocalDevice)
          }
          .width('100%')
          .padding(18)
          .borderRadius(18)
          .backgroundColor(this.palette().bgCard)
          .border({ width: 1, color: this.palette().glassBorder })
          .alignItems(HorizontalAlign.Start)
        }
        .width('100%')
        .padding({ left: 18, right: 18, top: 82, bottom: 96 })
        .alignItems(HorizontalAlign.Start)
      }
      .width('100%')
      .height('100%')
      .scrollBar(BarState.Auto)
      .align(Alignment.Top)

      PageTopBar({ title: '推送内容' })

      Row() {
        MotionBase({ pressEnabled: true, fillWidth: false }) {
          Button('关闭')
            .height(42)
            .padding({ left: 22, right: 22 })
            .fontSize(14)
            .fontWeight(FontWeight.Medium)
            .fontColor(Color.White)
            .backgroundColor(this.palette().accent)
            .borderRadius(21)
            .onClick(() => {
              this.onClose();
            })
        }
      }
      .width('100%')
      .padding({ left: 18, right: 18, bottom: 22 })
      .justifyContent(FlexAlign.End)
      .transition(TransitionEffect.OPACITY
        .combine(TransitionEffect.translate({ y: 18 }))
        .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut }))
    }
    .width('100%')
    .height('100%')
    .backgroundColor(this.palette().bgPrimary)
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }
}
