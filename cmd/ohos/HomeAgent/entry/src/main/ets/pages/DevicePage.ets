import { deviceBridge } from '../common/DeviceBridge';
import { connStore } from '../common/ConnStore';
import { apiClient } from '../common/ApiClient';
import { handleNavOnScroll } from '../common/NavBarController';
import { registerNavStack, unregisterNavStack } from '../common/NavStackRegistry';
import { DeviceInfo } from '../model/Model';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, WIDE_NAV_BAR_WIDTH, WIDE_MIN_CONTENT, ANIM_FAST, ANIM_NORMAL, ANIM_ENTER } from '../common/Constants';
import { MotionBase } from '../components/MotionBase';
import { PageTopBar, NavFloatOverlay, NavFloatRow, FloatIconButton } from '../components/PageTopBar';
import { SubPageLayer, NavGroup, NavRow, PlainCard, markSubPageOpen, subPageParam } from '../components/SubPage';
import { LOCAL_DEVICE_CAPS, deviceGatewayUrl } from '../common/DeviceBridgeSession';

/** 二级页面标识 */
const SUB_NONE: string = '';
const SUB_LOCAL: string = 'local';
const SUB_CAPS: string = 'caps';
const SUB_GATEWAY: string = 'gateway';
const SUB_LIST: string = 'list';

@Component
export struct DevicePage {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @StorageProp('navVisible') private navVisible: boolean = true;
  @StorageProp('currentTab') private currentTab: number = 0;
  /** 宽屏：左边一级界面（含底部导航栏），右边二级界面 */
  @StorageProp('isWideScreen') private isWide: boolean = false;
  /** 当前右栏展示的二级页面 id，用于宽屏下高亮左侧入口行 */
  @State activeSub: string = SUB_NONE;
  @StorageProp('deviceBridgeConnected') private bridgeConnected: boolean = false;
  @State bridgeUrl: string = '';
  @State bridgeToken: string = '';
  @State deviceId: string = '';
  @State authorized: boolean = false;
  @State devices: DeviceInfo[] = [];
  @State loadingDevices: boolean = false;
  @State toastMsg: string = '';
  @State toastIsError: boolean = false;
  /** 二级页面导航栈：系统返回手势/三键返回直接作用于它 */
  private navStack: NavPathStack = new NavPathStack();

  aboutToAppear(): void {
    this.deviceId = deviceBridge.getDeviceId();
    if (this.deviceId.length === 0) {
      this.deviceId = connStore.getDeviceId();
    }
    if (this.deviceId.length === 0) {
      this.deviceId = 'ohos-' + Date.now().toString(36);
      try {
        connStore.saveDeviceId(this.deviceId);
      } catch (e) {
        // ignore
      }
    }
    this.authorized = connStore.getDeviceAuth();
    // Gateway URL derives from current connection
    const cur = connStore.getCurrentConnection();
    if (cur !== null) {
      this.bridgeUrl = deviceGatewayUrl(cur.url);
      this.bridgeToken = cur.apiKey;
    }
    this.refreshDevices();
    // 登记导航栈：返回手势由 Index.onBackPress 按当前 Tab 精确派发过来
    registerNavStack(2, this.navStack, () => {
      this.activeSub = SUB_NONE;
    });
  }

  aboutToDisappear(): void {
    unregisterNavStack(2);
  }

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  /**
   * 打开二级页面。
   *
   * 窄屏（Stack 模式）：push 一层，整屏覆盖，系统侧滑返回可退。
   * 宽屏（Split 模式）：左栏一级列表常驻，右栏只应有一页，
   *   所以用 replace 换页而不是叠栈 —— 否则返回手势要一层层退回去。
   */
  private openSub(id: string): void {
    this.getUIContext().animateTo({ duration: ANIM_NORMAL, curve: Curve.EaseOut }, () => {
      this.activeSub = id;
    });
    if (this.isWide && this.navStack.size() > 0) {
      this.navStack.replacePathByName(id, subPageParam(id), false);
    } else {
      this.navStack.pushPathByName(id, subPageParam(id), true);
    }
  }

  /** 二级页面返回键：宽屏下左栏常驻可见，SubPageLayer 已隐藏返回键 */
  private closeSub(): void {
    this.navStack.pop();
    this.getUIContext().animateTo({ duration: ANIM_NORMAL, curve: Curve.EaseOut }, () => {
      this.activeSub = SUB_NONE;
    });
  }

  /** Toggle local authorization flag; hello 同步到网关。 */
  private toggleAuthorized(on: boolean): void {
    this.authorized = on;
    try {
      connStore.saveDeviceAuth(on);
    } catch (e) {
      // ignore persist failure
    }
    deviceBridge.updateAuthorized(on);
    this.showToast(on ? '已授权，agent 可下发能力命令' : '已取消授权', false);
  }

  private showToast(msg: string, isError: boolean): void {
    // 颜色标记必须在动画闭包外先落定，否则第一帧用的还是上一条 toast 的配色
    this.toastIsError = isError;
    this.getUIContext().animateTo({ duration: ANIM_NORMAL, curve: Curve.EaseOut }, () => {
      this.toastMsg = msg;
    });
    setTimeout(() => {
      this.getUIContext().animateTo({ duration: ANIM_NORMAL, curve: Curve.EaseOut }, () => {
        this.toastMsg = '';
      });
    }, 2500);
  }

  /** 拉取网关在线设备列表（经 webui 反代）。 */
  private async refreshDevices(): Promise<void> {
    this.loadingDevices = true;
    try {
      // apiClient 已自动前置 /api/v1，这里只写其后的部分，
      // 否则会拼成 /api/v1/api/v1/device/online 并 404。
      const resp = await apiClient.get('/device/online');
      if (resp.status >= 200 && resp.status < 300) {
        const parsed: Record<string, Object> = JSON.parse(resp.body) as Record<string, Object>;
        const devs: Object = parsed['devices'];
        const list: DeviceInfo[] = [];
        if (devs !== undefined && devs !== null) {
          const arr: Object[] = devs as Object[];
          for (let i = 0; i < arr.length; i++) {
            const d: Record<string, Object> = arr[i] as Record<string, Object>;
            const capsArr: Object = d['caps'];
            const caps: string[] = [];
            if (capsArr !== undefined && capsArr !== null) {
              const cArr: Object[] = capsArr as Object[];
              for (let j = 0; j < cArr.length; j++) {
                caps.push(cArr[j] as string);
              }
            }
            const info: DeviceInfo = {
              deviceId: d['device_id'] as string ?? '',
              name: d['name'] as string ?? '',
              kind: d['kind'] as string ?? '',
              online: true,
              authorized: d['authorized'] as boolean ?? false,
              caps: caps,
            };
            list.push(info);
          }
        }
        this.getUIContext().animateTo({ duration: ANIM_ENTER, curve: Curve.EaseOut }, () => {
          this.devices = list;
        });
      } else {
        this.devices = [];
      }
    } catch (e) {
      // 网络失败保持静默，不打扰用户
    }
    this.loadingDevices = false;
  }

  build() {
    // Navigation 提供真实导航栈：系统侧滑返回 / 三键返回都会自动 pop
    Navigation(this.navStack) {
      Stack({ alignContent: Alignment.Bottom }) {
        Column() {
          Scroll() {
            Column() {
              this.RootEntries()
            }
            .width('100%')
            .padding({ left: 16, right: 16, top: 76, bottom: 174 })
          }
          .width('100%')
          .height('100%')
          .scrollBar(BarState.Off)
          .align(Alignment.Top)
          .onDidScroll((xOffset: number, yOffset: number, state: ScrollState) => {
            handleNavOnScroll(state);
          })
        }
        .width('100%')
        .height('100%')

        PageTopBar({ title: '设备' })

        // 一级悬浮区：只留刷新（网关状态徽标按用户要求去掉，
        // 状态已经在页面内的网关卡片里如实展示）
        NavFloatOverlay({ tab: 2 }) {
          NavFloatRow() {
            FloatIconButton({
              icon: $r('app.media.ic_refresh'),
              onTap: () => {
                this.refreshDevices();
              },
            })
          }
        }

        this.Toast()
      }
      .width('100%')
      .height('100%')
      .backgroundColor(Color.Transparent)
    }
    .navDestination(this.SubDestination)
    // 宽屏（>=600vp）用 Split：navBar（一级列表 + 底部悬浮导航栏）常驻左栏，
    // NavDestination（二级页面）渲染在右栏，两栏同时可见。
    .mode(this.isWide ? NavigationMode.Split : NavigationMode.Stack)
    .navBarPosition(NavBarPosition.Start)
    .navBarWidth(WIDE_NAV_BAR_WIDTH)
    .minContentWidth(WIDE_MIN_CONTENT)
    .hideTitleBar(true)
    .hideToolBar(true)
    .hideBackButton(true)
    .width('100%')
    .height('100%')
    // Split 模式下 navBar 常驻，此回调不再触发；宽屏一律锁住主 Tab 横滑
    .onNavBarStateChange((isVisible: boolean) => {
      markSubPageOpen(this.isWide || !isVisible);
    })
    // 模式初始化与切换：Split 右栏不能空白，自动填入默认二级页面；
    // 退回 Stack 时清栈，否则会残留一个整屏覆盖的二级页面。
    .onNavigationModeChange((mode: NavigationMode) => {
      if (mode === NavigationMode.Split) {
        markSubPageOpen(true);
        if (this.navStack.size() === 0) {
          this.openSub(SUB_LOCAL);
        }
      } else {
        this.navStack.clear(false);
        this.getUIContext().animateTo({ duration: ANIM_NORMAL, curve: Curve.EaseOut }, () => {
          this.activeSub = SUB_NONE;
        });
        markSubPageOpen(false);
      }
    })
  }

  /** 二级页面路由表 */
  @Builder
  SubDestination(name: string, param: object) {
    NavDestination() {
      if (name === SUB_LOCAL) {
        SubPageLayer({
          title: '本机设备',
          tab: 2,
          onBack: () => {
            this.closeSub();
          },
        }) {
          this.LocalDeviceContent()
        }
      } else if (name === SUB_CAPS) {
        SubPageLayer({
          title: '设备能力',
          tab: 2,
          onBack: () => {
            this.closeSub();
          },
        }) {
          this.CapsContent()
        }
      } else if (name === SUB_GATEWAY) {
        SubPageLayer({
          title: '设备通道',
          tab: 2,
          onBack: () => {
            this.closeSub();
          },
        }) {
          this.GatewayContent()
        }
      } else if (name === SUB_LIST) {
        SubPageLayer({
          title: '接入的设备',
          tab: 2,
          onBack: () => {
            this.closeSub();
          },
          showRefresh: true,
          onRefresh: () => {
            this.refreshDevices();
          },
        }) {
          this.OnlineDevicesContent()
        }
      }
    }
    .hideTitleBar(true)
    .backgroundColor(Color.Transparent)
  }


  @Builder
  Toast() {
    if (this.toastMsg.length > 0) {
      Row() {
        Text(this.toastMsg)
          .fontSize(13)
          .fontColor(this.toastIsError ? this.palette().toastErrorText : this.palette().toastText)
          .padding({ left: 20, right: 20, top: 10, bottom: 10 })
          .borderRadius(RADIUS_MD)
          .backgroundColor(this.toastIsError ? this.palette().toastErrorBg : this.palette().toastBg)
      }
      .width('100%')
      .justifyContent(FlexAlign.End)
      .padding({ right: 20 })
      .margin({ bottom: 166 })
      .transition(TransitionEffect.OPACITY
        .combine(TransitionEffect.translate({ y: 12 }))
        .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut }))
    }
  }

  // ===================== 一级入口列表 =====================

  @Builder
  RootEntries() {
    NavGroup({ caption: '本机' }) {
      NavRow({
        icon: $r('app.media.ic_phone'),
        title: '本机设备',
        subtitle: this.deviceId.length > 0 ? this.deviceId : '未注册',
        value: this.bridgeConnected ? '在线' : '离线',
        selected: this.isWide && this.activeSub === SUB_LOCAL,
        onTap: () => {
          this.openSub(SUB_LOCAL);
        },
      })
      NavRow({
        icon: $r('app.media.ic_bolt'),
        title: '设备能力',
        subtitle: this.capsCount().toString() + ' 项能力',
        value: '',
        showDivider: false,
        selected: this.isWide && this.activeSub === SUB_CAPS,
        onTap: () => {
          this.openSub(SUB_CAPS);
        },
      })
    }

    NavGroup({ caption: '通道' }) {
      NavRow({
        icon: $r('app.media.ic_gateway'),
        title: '设备通道',
        subtitle: this.bridgeUrl.length > 0 ? '网关已配置' : '未配置',
        value: this.bridgeConnected ? '已连接' : '未连接',
        selected: this.isWide && this.activeSub === SUB_GATEWAY,
        onTap: () => {
          this.openSub(SUB_GATEWAY);
        },
      })
      NavRow({
        icon: $r('app.media.ic_devices_multi'),
        title: '接入的设备',
        subtitle: this.loadingDevices ? '加载中...' : '当前在线',
        value: this.devices.length.toString() + ' 台',
        showDivider: false,
        selected: this.isWide && this.activeSub === SUB_LIST,
        onTap: () => {
          this.openSub(SUB_LIST);
        },
      })
    }
  }

  private capsCount(): number {
    return LOCAL_DEVICE_CAPS.length;
  }

  // ===================== 二级：本机设备 =====================

  @Builder
  LocalDeviceContent() {
    PlainCard({ caption: '基本信息' }) {
      this.KvRow('设备 ID', this.deviceId.length > 0 ? this.deviceId : '未注册')
      this.KvRow('名称', 'HomeAgent OHOS')
      this.KvRow('类型', 'phone')
    }

    PlainCard({ caption: '权限控制' }) {
      Row() {
        Column({ space: 2 }) {
          Text('允许 agent 控制本机')
            .fontSize(14)
            .fontColor(this.palette().textPrimary)
          Text('授权后 agent 可调用下方能力；截屏仅捕获本应用画面，剪贴板读取需系统弹窗确认。')
            .fontSize(11)
            .fontColor(this.palette().textMuted)
            .margin({ top: 4 })
        }
        .layoutWeight(1)
        .alignItems(HorizontalAlign.Start)

        Toggle({ type: ToggleType.Switch, isOn: this.authorized })
          .selectedColor(this.palette().accent)
          .onChange((on: boolean) => {
            this.toggleAuthorized(on);
          })
      }
      .width('100%')
      .alignItems(VerticalAlign.Center)

      Row() {
        Circle({ width: 8, height: 8 })
          .fill(this.authorized ? '#17A964' : '#E84026')
          .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
          .margin({ right: 8 })
        Text(this.authorized ? '已授权 — agent 可远程调用能力' : '未授权 — agent 将拒绝远程命令')
          .fontSize(12)
          .fontColor(this.authorized ? '#17A964' : this.palette().textMuted)
          .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
      }
      .width('100%')
      .margin({ top: 12 })
      .padding({ left: 4 })
    }
  }

  // ===================== 二级：设备能力 =====================

  @Builder
  CapsContent() {
    PlainCard({ caption: '能力清单' }) {
      Text('agent 通过设备桥可调用的本机能力：')
        .fontSize(12)
        .fontColor(this.palette().textMuted)
        .margin({ bottom: 10 })

      Flex({ wrap: FlexWrap.Wrap }) {
        ForEach(LOCAL_DEVICE_CAPS, (cap: string) => {
          Text(cap)
            .fontSize(11)
            .fontColor(this.palette().accent)
            .padding({ left: 8, right: 8, top: 4, bottom: 4 })
            .borderRadius(999)
            .backgroundColor(this.palette().bgHover)
            .border({ width: 1, color: this.palette().glassBorder })
            .margin({ right: 6, bottom: 6 })
        }, (cap: string) => cap)
      }
      .width('100%')
    }
  }

  // ===================== 二级：设备通道 =====================

  @Builder
  GatewayContent() {
    PlainCard({ caption: '连接信息' }) {
      this.KvRow('网关地址', this.bridgeUrl.length > 0 ? this.bridgeUrl : '-')
      this.KvRow('Token', this.bridgeToken.length > 0 ? '已从连接继承' : '未配置')
      this.KvRow('状态', this.bridgeConnected ? '已连接' : '未连接')
    }

    PlainCard({ caption: '操作' }) {
      Row() {
        Blank()
        MotionBase({ pressEnabled: true, fillWidth: false }) {
          Button('刷新设备')
            .height(34)
            .fontSize(12)
            .backgroundColor(Color.Transparent)
            .border({ width: 1, color: this.palette().btnGhostBorder })
            .fontColor(this.palette().textSecondary)
            .onClick(() => {
              this.refreshDevices();
            })
        }
      }
      .width('100%')

      Text('设备通道由应用前台生命周期统一管理；切换连接配置后会自动使用新地址和 Token。')
        .fontSize(11)
        .fontColor(this.palette().textMuted)
        .margin({ top: 10 })
    }
  }

  // ===================== 二级：接入的设备 =====================

  @Builder
  OnlineDevicesContent() {
    if (this.devices.length === 0) {
      Text('暂无其他设备。电脑 GUI 或 CLI 连接同一网关后会出现在这里。')
        .fontSize(12)
        .fontColor(this.palette().textMuted)
        .padding({ left: 4 })
        .transition(TransitionEffect.OPACITY.animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
    }

    ForEach(this.devices, (dev: DeviceInfo) => {
      // PlainCard 是自定义组件，transition 不能直接挂在它上面（会生成 __Common__ 包装），
      // 所以用一个无 padding、满宽的 Column 承载入场动画，布局不受影响。
      Column() {
        PlainCard({ caption: '' }) {
          Row() {
            Circle({ width: 8, height: 8 })
              .fill(dev.online ? '#17A964' : '#77809A')
              .margin({ right: 10 })
            Column() {
              Text(dev.name.length > 0 ? dev.name : dev.deviceId)
                .fontSize(14)
                .fontColor(this.palette().textPrimary)
              Text(dev.kind + (dev.authorized ? ' · 已授权' : ' · 未授权'))
                .fontSize(11)
                .fontColor(dev.authorized ? '#17A964' : this.palette().textMuted)
            }
            .layoutWeight(1)
            .alignItems(HorizontalAlign.Start)

            Text(dev.caps.length.toString() + ' 能力')
              .fontSize(10)
              .fontColor(this.palette().textMuted)
          }
          .width('100%')
        }
      }
      .width('100%')
      .transition(TransitionEffect.OPACITY
        .combine(TransitionEffect.translate({ y: 12 }))
        .animation({ duration: ANIM_ENTER, curve: Curve.EaseOut }))
    }, (dev: DeviceInfo) => dev.deviceId + dev.online.toString())
  }

  // ===================== 通用 KV 行 =====================

  @Builder
  KvRow(k: string, v: string) {
    Row() {
      Text(k)
        .fontSize(13)
        .fontColor(this.palette().textSecondary)
      Blank()
      Text(v)
        .fontSize(13)
        .fontColor(this.palette().textPrimary)
        .maxLines(1)
        .textOverflow({ overflow: TextOverflow.Ellipsis })
    }
    .width('100%')
    .padding({ top: 8, bottom: 8 })
    .border({ width: { bottom: 1 }, color: this.palette().kvBorder })
  }
}

const RADIUS_MD: number = 10;
