import { apiClient } from '../common/ApiClient';
import { userMessage, noConnectionMessage } from '../common/UserError';
import { handleNavOnScroll } from '../common/NavBarController';
import { registerNavStack, unregisterNavStack } from '../common/NavStackRegistry';
import { PluginRow, PluginDetail, emptyPluginDetail } from '../model/Model';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, WIDE_NAV_BAR_WIDTH, WIDE_MIN_CONTENT } from '../common/Constants';
import { RADIUS_LG, RADIUS_MD, RADIUS_SM, RADIUS_PILL, COLOR_ERROR } from '../common/Constants';
import { PageTopBar, NavFloatOverlay, NavFloatRow, GlassShell, FloatIconButton } from '../components/PageTopBar';
import { SubPageLayer, PlainCard, markSubPageOpen, subPageParam } from '../components/SubPage';
import { SettingsEditor } from '../components/SettingsEditor';

interface InstallBody {
  url: string;
}

/** 二级页面标识：插件详情 */
const SUB_NONE: string = '';
const SUB_DETAIL: string = 'detail';

@Component
export struct PluginsPage {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @StorageProp('navVisible') private navVisible: boolean = true;
  @StorageProp('currentTab') private currentTab: number = 0;
  /** 宽屏：左边插件列表（含底部导航栏），右边插件详情 */
  @StorageProp('isWideScreen') private isWide: boolean = false;
  @State plugins: PluginRow[] = [];
  @State loading: boolean = false;
  @State showInstallForm: boolean = false;
  @State installUrl: string = '';
  @State toastMsg: string = '';
  @State toastIsError: boolean = false;
  /** 当前查看详情的插件名，用于宽屏下高亮左侧列表项 */
  @State activeName: string = '';
  /** 详情页数据（GET /plugins/{name}） */
  @State detail: PluginDetail = emptyPluginDetail();
  @State detailLoading: boolean = false;
  @State detailError: string = '';
  /** 二级页面导航栈：系统返回手势/三键返回直接作用于它 */
  private navStack: NavPathStack = new NavPathStack();

  aboutToAppear(): void {
    // 登记导航栈：返回手势由 Index.onBackPress 按当前 Tab 精确派发过来
    registerNavStack(1, this.navStack, () => {
      this.activeName = '';
    });
    this.loadPlugins();
  }

  aboutToDisappear(): void {
    unregisterNavStack(1);
  }

  private palette(): ThemePalette {
    // 引用 this.isDark 建立响应式依赖：主题切换时整个组件树重渲染
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  /**
   * 数据源对齐 WebGUI renderPlugins：
   *  - GET /kernel          → plugins[{name,loaded}]（含全部内置插件）+ tools（按 plugin 归属）
   *  - GET /plugins         → 已安装外部插件元数据（version/description 等）
   *  - GET /plugins/disabled → {disabled:[{name,...}]}
   * 三方按名称合并去重排序。
   */
  private async loadPlugins(): Promise<void> {
    if (!apiClient.hasConnection()) {
      return;
    }
    this.loading = true;
    try {
      // ---- kernel: loaded plugins + tool ownership ----
      const kResp = await apiClient.getWithTimeout('/kernel', 12000);
      const kernelObj: Record<string, Object> = JSON.parse(kResp.body) as Record<string, Object>;
      const loadedMap: Map<string, boolean> = new Map<string, boolean>();
      const kpRaw: Object | undefined = kernelObj['plugins'];
      if (kpRaw !== undefined && kpRaw !== null) {
        const kpArr: Object[] = kpRaw as Object[];
        for (let i = 0; i < kpArr.length; i++) {
          const item: Record<string, Object> = kpArr[i] as Record<string, Object>;
          const n: string = item['name'] as string ?? '';
          if (n.length === 0) {
            continue;
          }
          loadedMap.set(n, item['loaded'] as boolean ?? true);
        }
      }
      const toolsByPlugin: Map<string, string[]> = new Map<string, string[]>();
      const tRaw: Object | undefined = kernelObj['tools'];
      if (tRaw !== undefined && tRaw !== null) {
        const tArr: Object[] = tRaw as Object[];
        for (let i = 0; i < tArr.length; i++) {
          const item: Record<string, Object> = tArr[i] as Record<string, Object>;
          const tn: string = item['name'] as string ?? '';
          const owner: string = item['plugin'] as string ?? '';
          if (tn.length === 0 || owner.length === 0) {
            continue;
          }
          let list: string[] | undefined = toolsByPlugin.get(owner);
          if (list === undefined) {
            list = [];
            toolsByPlugin.set(owner, list);
          }
          // 每插件最多展示 8 个工具名，避免卡片过长
          if (list.length < 8) {
            list.push(tn);
          }
        }
      }

      // ---- installed external plugins metadata ----
      const externalMeta: Map<string, Record<string, Object>> = new Map<string, Record<string, Object>>();
      try {
        const pResp = await apiClient.getWithTimeout('/plugins', 10000);
        const bodyTrim = pResp.body.trim();
        let arr: Object[] = [];
        if (bodyTrim.length > 0 && bodyTrim.charAt(0) === '[') {
          arr = JSON.parse(pResp.body) as Object[];
        } else {
          const obj: Record<string, Object> = JSON.parse(pResp.body) as Record<string, Object>;
          const rawList: Object = obj['plugins'] ?? obj['data'];
          if (rawList !== undefined && rawList !== null) {
            arr = rawList as Object[];
          }
        }
        for (let i = 0; i < arr.length; i++) {
          const item: Record<string, Object> = arr[i] as Record<string, Object>;
          const n: string = item['name'] as string ?? '';
          if (n.length > 0) {
            externalMeta.set(n, item);
          }
        }
      } catch (e) {
        // 外部列表失败不阻塞内置展示
      }

      // ---- disabled list ----
      const disabledNames: Set<string> = new Set<string>();
      try {
        const dResp = await apiClient.getWithTimeout('/plugins/disabled', 8000);
        const dObj: Record<string, Object> = JSON.parse(dResp.body) as Record<string, Object>;
        const dArr: Object | undefined = dObj['disabled'];
        if (dArr !== undefined && dArr !== null) {
          const items: Object[] = dArr as Object[];
          for (let di = 0; di < items.length; di++) {
            const dItem: Record<string, Object> = items[di] as Record<string, Object>;
            const dn: string = dItem['name'] as string ?? '';
            if (dn.length > 0) {
              disabledNames.add(dn);
            }
          }
        }
      } catch (e) {
        // disabled endpoint may not exist; ignore
      }

      // ---- merge: allNames sorted（与 GUI 一致）----
      const allNames: Set<string> = new Set<string>();
      loadedMap.forEach((v: boolean, k: string) => {
        allNames.add(k);
      });
      externalMeta.forEach((v: Record<string, Object>, k: string) => {
        allNames.add(k);
      });
      disabledNames.forEach((n: string) => {
        allNames.add(n);
      });
      const names: string[] = Array.from(allNames);
      names.sort();

      const rows: PluginRow[] = [];
      for (let i = 0; i < names.length; i++) {
        const name: string = names[i];
        const meta: Record<string, Object> | undefined = externalMeta.get(name);
        const tools: string[] | undefined = toolsByPlugin.get(name);
        const row: PluginRow = {
          name: name,
          loaded: loadedMap.get(name) ?? false,
          disabled: disabledNames.has(name),
          external: externalMeta.has(name),
          version: meta !== undefined ? meta['version'] as string ?? '' : '',
          description: meta !== undefined ? meta['description'] as string ?? '' : '',
          tools: tools,
        };
        rows.push(row);
      }
      this.plugins = rows;
    } catch (e) {
      this.showToast(userMessage('plugins.load', e), true);
    }
    this.loading = false;
  }

  /** 徽标状态：与 GUI 一致 —— 已加载绿 / 禁用待生效黄 / 已禁用红 / 未加载灰。 */
  private statusOf(plugin: PluginRow): string {
    if (plugin.loaded && !plugin.disabled) {
      return 'loaded'; // 已加载
    }
    if (plugin.loaded && plugin.disabled) {
      return 'pending'; // 运行中(禁用待生效)
    }
    if (plugin.disabled) {
      return 'disabled'; // 已禁用
    }
    return 'notloaded'; // 未加载
  }

  private async togglePlugin(plugin: PluginRow): Promise<void> {
    const name: string = plugin.name;
    const action: string = plugin.disabled ? 'enable' : 'disable';
    try {
      await apiClient.post('/plugins/' + name + '/' + action, null);
      this.showToast('已' + (plugin.disabled ? '启用' : '禁用') + '插件: ' + name, false);
      this.loadPlugins();
    } catch (e) {
      this.showToast(userMessage('plugins.toggle', e), true);
    }
  }

  private async removePlugin(plugin: PluginRow): Promise<void> {
    const name: string = plugin.name;
    try {
      await apiClient.request('/plugins/' + name, 'DELETE', '', 8000);
      this.showToast('已卸载插件: ' + name, false);
      this.loadPlugins();
    } catch (e) {
      this.showToast(userMessage('plugins.remove', e), true);
    }
  }

  private async installPlugin(): Promise<void> {
    const url: string = this.installUrl.trim();
    if (url.length === 0) {
      return;
    }
    try {
      const bodyObj: InstallBody = { url: url };
      // 后端没有 /plugins/install 这个路由：安装就是 POST /plugins，body 带 url。
      await apiClient.post('/plugins', bodyObj);
      this.showToast('安装请求已发送', false);
      this.installUrl = '';
      this.showInstallForm = false;
      this.loadPlugins();
    } catch (e) {
      this.showToast(userMessage('plugins.install', e), true);
    }
  }

  // ===================== 二级页面：插件详情 =====================

  /**
   * 打开插件详情。
   *
   * 窄屏（Stack）：push 一层整屏覆盖，系统侧滑返回可退。
   * 宽屏（Split）：左栏列表常驻，右栏换页而不叠栈。
   */
  private openDetail(name: string): void {
    this.activeName = name;
    this.loadDetail(name);
    if (this.isWide && this.navStack.size() > 0) {
      this.navStack.replacePathByName(SUB_DETAIL, subPageParam(SUB_DETAIL), false);
    } else {
      this.navStack.pushPathByName(SUB_DETAIL, subPageParam(SUB_DETAIL), true);
    }
  }

  private closeDetail(): void {
    this.navStack.pop();
    this.activeName = '';
  }

  /**
   * GET /plugins/{name} —— 后端返回插件清单字段。
   * WebGUI 只是把它 JSON.stringify 进 <pre>，这里逐字段结构化展示。
   * 内置插件不在 /plugins 里，取不到详情时退回用列表已有的信息。
   */
  private async loadDetail(name: string): Promise<void> {
    this.detailLoading = true;
    this.detailError = '';
    try {
      const resp = await apiClient.getWithTimeout('/plugins/' + name, 10000);
      const o: Record<string, Object> = JSON.parse(resp.body) as Record<string, Object>;
      const d: PluginDetail = emptyPluginDetail();
      d.name = o['name'] as string ?? name;
      d.version = o['version'] as string ?? '';
      d.description = o['description'] as string ?? '';
      d.author = o['author'] as string ?? '';
      d.license = o['license'] as string ?? '';
      d.homepage = o['homepage'] as string ?? '';
      d.repository = o['repository'] as string ?? '';
      d.entry = o['entry'] as string ?? '';
      d.minVersion = o['min_version'] as string ?? '';
      d.deprecated = o['deprecated'] as boolean ?? false;
      d.tags = this.strArray(o['tags']);
      d.files = this.strArray(o['files']);
      this.detail = d;
    } catch (e) {
      // 内置插件没有清单，属于预期情况，不当成错误刷红
      const row: PluginRow | null = this.findRow(name);
      const d: PluginDetail = emptyPluginDetail();
      d.name = name;
      if (row !== null) {
        d.version = row.version ?? '';
        d.description = row.description ?? '';
      }
      this.detail = d;
      if (row !== null && row.external) {
        this.detailError = userMessage('plugins.detail', e);
      }
    }
    this.detailLoading = false;
  }

  private strArray(raw: Object | undefined): string[] {
    const out: string[] = [];
    if (raw === undefined || raw === null) {
      return out;
    }
    const arr: Object[] = raw as Object[];
    for (let i = 0; i < arr.length; i++) {
      const s: string = arr[i] as string ?? '';
      if (s.length > 0) {
        out.push(s);
      }
    }
    return out;
  }

  private findRow(name: string): PluginRow | null {
    for (let i = 0; i < this.plugins.length; i++) {
      if (this.plugins[i].name === name) {
        return this.plugins[i];
      }
    }
    return null;
  }

  /** 详情页正在展示的那一行（供二级页面里的操作按钮使用） */
  private activeRow(): PluginRow | null {
    return this.findRow(this.activeName);
  }

  private async reloadPlugins(): Promise<void> {
    try {
      await apiClient.post('/plugins/reload', null);
      this.showToast('插件已重载', false);
      this.loadPlugins();
    } catch (e) {
      this.showToast(userMessage('plugins.reload', e), true);
    }
  }

  private showToast(msg: string, isError: boolean): void {
    this.toastMsg = msg;
    this.toastIsError = isError;
    setTimeout(() => {
      this.toastMsg = '';
    }, 2500);
  }

  build() {
    // 宽屏（>=600vp）走 Split：navBar（左栏 = 插件列表 + 底部导航栏）常驻，
    // NavDestination（右栏 = 插件详情）与之并列；窄屏则是整屏覆盖的二级页面。
    Navigation(this.navStack) {
      Stack({ alignContent: Alignment.Bottom }) {
        Column() {
          Scroll() {
            Column() {
              this.ListStates()
              this.PluginList()
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

        // 顶栏遮罩（自身撑满并顶部对齐，全链路 hitTest None，触摸完全穿透到滚动区）
        PageTopBar({ title: '插件' })

        // 悬浮操作区：安装（悬浮主按钮）+ 重载。
        // 安装表单以悬浮卡形式浮在按钮上方，不再占用列表顶部一行。
        NavFloatOverlay({ tab: 1 }) {
          if (this.showInstallForm) {
            this.InstallForm()
          }
          NavFloatRow() {
            FloatIconButton({
              icon: $r('app.media.ic_reload'),
              onTap: () => {
                this.reloadPlugins();
              },
            })
            Button() {
              Image($r('app.media.ic_plus'))
                .width(20)
                .height(20)
                .fillColor(Color.White)
            }
              .width(50)
              .height(50)
              .type(ButtonType.Circle)
              .backgroundColor(this.palette().accent)
              .shadow({ radius: 24, color: this.palette().shadow, offsetY: 8 })
              .onClick(() => {
                this.showInstallForm = !this.showInstallForm;
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
    // 模式初始化与切换：Split 右栏不能空白，自动选中第一个插件；
    // 退回 Stack 时清栈，否则会残留一个整屏覆盖的详情页。
    .onNavigationModeChange((mode: NavigationMode) => {
      if (mode === NavigationMode.Split) {
        markSubPageOpen(true);
        if (this.navStack.size() === 0 && this.plugins.length > 0) {
          this.openDetail(this.plugins[0].name);
        }
      } else {
        this.navStack.clear(false);
        this.activeName = '';
        markSubPageOpen(false);
      }
    })
  }

  /** 二级页面路由表：插件详情 */
  @Builder
  SubDestination(name: string, param: object) {
    NavDestination() {
      if (name === SUB_DETAIL) {
        SubPageLayer({
          title: this.activeName.length > 0 ? this.activeName : '插件详情',
          tab: 1,
          onBack: () => {
            this.closeDetail();
          },
          showRefresh: true,
          onRefresh: () => {
            this.loadDetail(this.activeName);
          },
        }) {
          this.DetailContent()
        }
      }
    }
    .hideTitleBar(true)
    .backgroundColor(Color.Transparent)
  }

  /** 安装表单：悬浮在安装按钮上方的一张玻璃卡（点悬浮按钮开合） */
  @Builder
  InstallForm() {
    Row() {
      TextInput({ placeholder: '.hmap 包下载 URL', text: this.installUrl })
        .layoutWeight(1)
        .height(36)
        .fontSize(14)
        .fontColor(this.palette().textPrimary)
        .placeholderColor(this.palette().textMuted)
        .backgroundColor(this.palette().bgInput)
        .borderRadius(RADIUS_SM)
        .border({ width: 1, color: this.palette().border })
        .onChange((v: string) => {
          this.installUrl = v;
        })
      Button('安装')
        .height(36)
        .fontSize(12)
        .backgroundColor(this.palette().accent)
        .fontColor('#FFFFFF')
        .margin({ left: 6 })
        .onClick(() => {
          this.installPlugin();
        })
    }
    .width('100%')
    .padding(10)
    .margin({ bottom: 10 })
    .backgroundColor(this.palette().navBarBg)
    .borderRadius(RADIUS_MD)
    .border({ width: 1, color: this.palette().navBarBorder })
    .shadow({ radius: 20, color: this.palette().shadow, offsetY: 6 })
    .alignItems(VerticalAlign.Center)
  }

  /** 加载中 / 未配置 / 空列表三种占位态 */
  @Builder
  ListStates() {
    if (this.loading && this.plugins.length === 0) {
      LoadingProgress()
        .width(32)
        .height(32)
        .color(this.palette().accent)
        .margin({ top: 40 })
    }

    if (!apiClient.hasConnection()) {
      Text(noConnectionMessage())
        .fontSize(13)
        .fontColor(this.palette().textMuted)
        .padding(20)
    }

    if (!this.loading && this.plugins.length === 0 && apiClient.hasConnection()) {
      Text('暂无已加载插件')
        .fontSize(13)
        .fontColor(this.palette().textMuted)
        .padding(20)
    }
  }

  /**
   * 一级列表：每个插件一张紧凑卡（名称 + 状态徽标 + 右尖角）。
   * 描述、工具清单、启停/卸载操作全部下沉到详情页 —— 列表只负责选择。
   */
  @Builder
  PluginList() {
    ForEach(this.plugins, (plugin: PluginRow) => {
      Row() {
        Column({ space: 3 }) {
          Row({ space: 6 }) {
            Text(plugin.name)
              .fontSize(15)
              .fontWeight(this.activeName === plugin.name ? FontWeight.Medium : FontWeight.Normal)
              .fontColor(this.activeName === plugin.name
                ? this.palette().accent : this.palette().textPrimary)
              .maxLines(1)
              .textOverflow({ overflow: TextOverflow.Ellipsis })
            if (plugin.version !== undefined && plugin.version.length > 0) {
              Text('v' + plugin.version)
                .fontSize(10)
                .fontColor(this.palette().textMuted)
            }
          }
          // 徽标全部去掉（用户要求）：状态用一个 3vp 圆点表达，
          // 其余信息退化为一行灰字副标题 —— 列表只负责"选谁"，细节看详情页。
          Row({ space: 6 }) {
            Circle({ width: 6, height: 6 })
              .fill(this.statusDotColor(plugin))
            Text(this.rowSubtitle(plugin))
              .fontSize(11)
              .fontColor(this.palette().textMuted)
              .maxLines(1)
              .textOverflow({ overflow: TextOverflow.Ellipsis })
              .layoutWeight(1)
          }
          .width('100%')
          .alignItems(VerticalAlign.Center)
        }
        .layoutWeight(1)
        .alignItems(HorizontalAlign.Start)

        Image($r('app.media.ic_chevron_right'))
          .width(15)
          .height(15)
          .fillColor(this.activeName === plugin.name
            ? this.palette().accent : this.palette().textMuted)
          .draggable(false)
      }
      .width('100%')
      .padding(14)
      .borderRadius(RADIUS_LG)
      .backgroundColor(this.activeName === plugin.name
        ? this.palette().accentBg : this.palette().bgCard)
      .border({
        width: 1,
        color: this.activeName === plugin.name
          ? this.palette().accent : this.palette().glassBorder,
      })
      .margin({ bottom: 10 })
      .alignItems(VerticalAlign.Center)
      .onClick(() => {
        this.openDetail(plugin.name);
      })
    }, (plugin: PluginRow) => plugin.name + (plugin.loaded ? 'L' : '') + (plugin.disabled ? 'D' : ''))
  }

  /**
   * 二级页面：插件详情。
   * WebGUI 这里只有一个 JSON.stringify 的 <pre>，
   * 移植时改成结构化卡片：状态 / 清单字段 / 工具 / 操作。
   */
  @Builder
  DetailContent() {
    if (this.detailLoading) {
      Row() {
        LoadingProgress()
          .width(26)
          .height(26)
          .color(this.palette().accent)
      }
      .width('100%')
      .justifyContent(FlexAlign.Center)
      .padding({ top: 30, bottom: 30 })
    }

    if (this.detailError.length > 0) {
      Text(this.detailError)
        .fontSize(12)
        .fontColor(COLOR_ERROR)
        .padding({ left: 4, bottom: 12 })
    }

    // 概览卡：名称、版本、状态徽标、描述
    PlainCard({ caption: '概览' }) {
      Row({ space: 8 }) {
        Text(this.detail.name.length > 0 ? this.detail.name : this.activeName)
          .fontSize(17)
          .fontWeight(FontWeight.Bold)
          .fontColor(this.palette().textPrimary)
          .layoutWeight(1)
          .maxLines(1)
          .textOverflow({ overflow: TextOverflow.Ellipsis })
        if (this.detail.version.length > 0) {
          Text('v' + this.detail.version)
            .fontSize(12)
            .fontColor(this.palette().textSecondary)
        }
      }
      .width('100%')
      .margin({ bottom: 10 })

      this.DetailBadges()

      if (this.detail.description.length > 0) {
        Text(this.detail.description)
          .fontSize(13)
          .fontColor(this.palette().textSecondary)
          .width('100%')
          .margin({ top: 10 })
      }
    }

    // 清单卡：只有真拿到字段才出卡，否则会留一张空壳（内置插件没有清单文件）
    if (this.hasManifest()) {
      PlainCard({ caption: '清单' }) {
        this.KvRow('作者', this.detail.author)
        this.KvRow('许可证', this.detail.license)
        this.KvRow('主页', this.detail.homepage)
        this.KvRow('仓库', this.detail.repository)
        this.KvRow('入口', this.detail.entry)
        this.KvRow('最低内核版本', this.detail.minVersion)
      }
    }

    if (this.detail.tags.length > 0) {
      PlainCard({ caption: '标签' }) {
        Flex({ wrap: FlexWrap.Wrap }) {
          ForEach(this.detail.tags, (t: string) => {
            Text(t)
              .fontSize(10)
              .fontColor('#4A90D9')
              .padding({ left: 8, right: 8, top: 3, bottom: 3 })
              .borderRadius(RADIUS_SM)
              .backgroundColor(this.palette().frostSoftBg)
              .margin({ right: 5, bottom: 5 })
          }, (t: string) => t)
        }
      }
    }

    this.DetailTools()

    // 插件配置：plugin.<name>.* 从后端 /settings?prefix= 取，就地编辑。
    // 这些 key 属于插件本身，之前被平铺在「设置 → 后端配置」里，
    // 现在归位到插件详情页 —— 「插件的设计页面就是插件的详情页」。
    if (this.activeName.length > 0) {
      PlainCard({ caption: '插件配置' }) {
        SettingsEditor({
          prefix: 'plugin.' + this.activeName + '.',
          emptyHint: '该插件没有暴露可配置项',
        })
      }
    }

    if (this.detail.files.length > 0) {
      PlainCard({ caption: '文件 (' + this.detail.files.length.toString() + ')' }) {
        ForEach(this.detail.files, (f: string) => {
          Text(f)
            .fontSize(12)
            .fontColor(this.palette().textSecondary)
            .width('100%')
            .maxLines(1)
            .textOverflow({ overflow: TextOverflow.Ellipsis })
            .margin({ bottom: 4 })
        }, (f: string) => f)
      }
    }

    this.DetailActions()
  }

  /** 详情页状态行：同样去掉徽标，一个状态点 + 一行纯文字 */
  @Builder
  DetailBadges() {
    Row({ space: 6 }) {
      Circle({ width: 7, height: 7 })
        .fill(this.activeStatusColor())
      Text(this.detailStatusLine())
        .fontSize(12)
        .fontColor(this.palette().textSecondary)
        .layoutWeight(1)
        .maxLines(1)
        .textOverflow({ overflow: TextOverflow.Ellipsis })
    }
    .width('100%')
    .alignItems(VerticalAlign.Center)
  }

  private detailStatusLine(): string {
    const parts: string[] = [];
    parts.push(this.activeStatusText());
    parts.push(this.activeIsBuiltin() ? '内置' : '外部');
    if (this.detail.deprecated) {
      parts.push('已废弃');
    }
    const n: number = this.activeTools().length;
    if (n > 0) {
      parts.push(n.toString() + ' 个工具');
    }
    return parts.join(' · ');
  }

  /** 工具清单来自一级列表已合并的 kernel.tools（按 plugin 归属） */
  @Builder
  DetailTools() {
    if (this.activeTools().length > 0) {
      PlainCard({ caption: '注册的工具 (' + this.activeTools().length.toString() + ')' }) {
        Flex({ wrap: FlexWrap.Wrap }) {
          ForEach(this.activeTools(), (tool: string) => {
            Text(tool)
              .fontSize(11)
              .fontColor('#4A90D9')
              .padding({ left: 8, right: 8, top: 3, bottom: 3 })
              .borderRadius(RADIUS_SM)
              .backgroundColor(this.palette().frostSoftBg)
              .margin({ right: 5, bottom: 5 })
          }, (tool: string) => tool)
        }
      }
    }
  }

  @Builder
  DetailActions() {
    if (this.activeName.length > 0) {
      PlainCard({ caption: '操作' }) {
        Row() {
          Button(this.activeIsDisabled() ? '启用' : '禁用')
            .height(34)
            .fontSize(12)
            .backgroundColor(Color.Transparent)
            .border({
              width: 1,
              color: this.activeIsDisabled()
                ? this.palette().btnGhostBorder : 'rgba(217, 154, 43, 0.5)',
            })
            .fontColor(this.activeIsDisabled()
              ? this.palette().textSecondary : '#D99A2B')
            .onClick(() => {
              const r: PluginRow | null = this.activeRow();
              if (r !== null) {
                this.togglePlugin(r);
              }
            })
          Blank()
          if (!this.activeIsBuiltin()) {
            Button('卸载')
              .height(34)
              .fontSize(12)
              .backgroundColor(Color.Transparent)
              .border({ width: 1, color: 'rgba(232, 64, 38, 0.45)' })
              .fontColor(COLOR_ERROR)
              .onClick(() => {
                const r: PluginRow | null = this.activeRow();
                if (r !== null) {
                  this.removePlugin(r);
                }
              })
          }
        }
        .width('100%')
      }
    }
  }

  // ---- 详情页取值助手：ArkTS 禁止非空断言，统一在这里做 null 收敛 ----

  private hasManifest(): boolean {
    return this.detail.author.length > 0 || this.detail.license.length > 0 ||
      this.detail.homepage.length > 0 || this.detail.repository.length > 0 ||
      this.detail.entry.length > 0 || this.detail.minVersion.length > 0;
  }

  private activeIsBuiltin(): boolean {
    const r: PluginRow | null = this.activeRow();
    return r !== null ? !r.external : false;
  }

  private activeIsDisabled(): boolean {
    const r: PluginRow | null = this.activeRow();
    return r !== null ? r.disabled : false;
  }

  private activeTools(): string[] {
    const r: PluginRow | null = this.activeRow();
    if (r === null) {
      return [];
    }
    return r.tools ?? [];
  }

  private activeStatusText(): string {
    const r: PluginRow | null = this.activeRow();
    return r !== null ? this.statusBadgeText(r) : '未加载';
  }

  private activeStatusColor(): string {
    const r: PluginRow | null = this.activeRow();
    return r !== null ? this.statusBadgeColor(r) : this.palette().textMuted;
  }

  /** 明细行：值为空时整行不渲染，避免详情页出现一排 "-" */
  @Builder
  KvRow(label: string, value: string) {
    if (value.length > 0) {
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
          .constraintSize({ maxWidth: 220 })
          .margin({ left: 16 })
      }
      .width('100%')
      .padding({ top: 8, bottom: 8 })
      .alignItems(VerticalAlign.Top)
    }
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
          .border({
            width: 1,
            color: this.toastIsError ? 'rgba(232, 64, 38, 0.3)' : 'rgba(23, 169, 100, 0.3)',
          })
      }
      .width('100%')
      .justifyContent(FlexAlign.End)
      .padding({ right: 20 })
      .margin({ bottom: 166 })
    }
  }

  /** 状态点颜色：绿=已加载，黄=待生效，红=已禁用，灰=未加载 */
  private statusDotColor(plugin: PluginRow): string {
    return this.statusBadgeColor(plugin);
  }

  /** 列表行副标题：状态 + 内置/外部 + 工具数，一行灰字，不用徽标 */
  private rowSubtitle(plugin: PluginRow): string {
    const parts: string[] = [];
    parts.push(this.statusBadgeText(plugin));
    parts.push(plugin.external ? '外部' : '内置');
    if (plugin.tools !== undefined && plugin.tools.length > 0) {
      parts.push(plugin.tools.length.toString() + ' 工具');
    }
    return parts.join(' · ');
  }

  private statusBadgeText(plugin: PluginRow): string {
    const s: string = this.statusOf(plugin);
    if (s === 'loaded') {
      return '已加载';
    }
    if (s === 'pending') {
      return '待生效';
    }
    if (s === 'disabled') {
      return '已禁用';
    }
    return '未加载';
  }

  private statusBadgeColor(plugin: PluginRow): string {
    const s: string = this.statusOf(plugin);
    if (s === 'loaded') {
      return '#17A964';
    }
    if (s === 'pending') {
      return '#D99A2B';
    }
    if (s === 'disabled') {
      return '#E84026';
    }
    return this.palette().textMuted;
  }
}
