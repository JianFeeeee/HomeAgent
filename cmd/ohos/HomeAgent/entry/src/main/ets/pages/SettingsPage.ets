import { apiClient } from '../common/ApiClient';
import { userMessage, noConnectionMessage } from '../common/UserError';
import { connStore } from '../common/ConnStore';
import { handleNavOnScroll } from '../common/NavBarController';
import { registerNavStack, unregisterNavStack } from '../common/NavStackRegistry';
import { ConnectionConfig, AppSettings, emptySettings } from '../model/Model';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, applyThemeMode, WIDE_NAV_BAR_WIDTH, WIDE_MIN_CONTENT } from '../common/Constants';
import { ANIM_FAST, ANIM_NORMAL, ANIM_ENTER } from '../common/Constants';
import { MotionBase } from '../components/MotionBase';
import { picker, fileIo } from '@kit.CoreFileKit';
import { common } from '@kit.AbilityKit';
import { PageTopBar, NavFloatOverlay, NavFloatRow, FloatIconButton } from '../components/PageTopBar';
import { SubPageLayer, NavGroup, NavRow, PlainCard, markSubPageOpen, subPageParam } from '../components/SubPage';
import { StatusSummaryCard, StatusDetailContent } from '../components/StatusCards';
import { statusStore } from '../common/StatusStore';

/** One settings key card rendered in the editor list. */
interface SettingEntry {
  key: string;
  displayName: string;
  description: string;
  type: string; // bool | int | duration | select | password | text | string
  options: string[];
  value: string; // raw string value as stored by backend
  dirty: boolean;
}

interface SettingsSection {
  id: string;
  title: string;
  count: number;
}

interface SaveSettingBody {
  key: string;
  value: string;
}

const PAGE_SIZE: number = 40;

/** 二级页面标识 */
const SUB_NONE: string = '';
const SUB_STATUS: string = 'status';
const SUB_CONNECTIONS: string = 'connections';
const SUB_APPEARANCE: string = 'appearance';
const SUB_BACKEND: string = 'backend';
const SUB_SECTION: string = 'section';

@Component
export struct SettingsPage {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @StorageProp('navVisible') private navVisible: boolean = true;
  @StorageProp('currentTab') private currentTab: number = 0;
  /** 宽屏：左边一级界面（含底部导航栏），右边二级界面 */
  @StorageProp('isWideScreen') private isWide: boolean = false;
  /** 当前右栏展示的二级页面 id，用于宽屏下高亮左侧入口行 */
  @State activeSub: string = SUB_NONE;
  @State connections: ConnectionConfig[] = [];
  @State currentId: string = '';
  @State editUrl: string = '';
  @State editApiKey: string = '';
  @State editName: string = '';
  @State showAddForm: boolean = false;
  @State addFormVisible: boolean = false;
  @State themeMode: string = 'system';
  @State lang: string = 'zh';
  @State bgImage: string = '';
  @State bgOpacity: number = 0.25;
  @State pickingBg: boolean = false;

  /** 二级页面导航栈：系统返回手势/三键返回直接作用于它 */
  private navStack: NavPathStack = new NavPathStack();

  // ===== backend key/value editor state =====
  @State sections: SettingsSection[] = [];
  /** plugin.* 配置项总数与涉及的插件个数（只用于在设置里给出去向提示） */
  @State pluginKeyCount: number = 0;
  @State pluginConfigCount: number = 0;
  @State activeSection: string = 'core';
  @State entries: SettingEntry[] = [];
  @State loadingSettings: boolean = false;
  @State settingsError: string = '';
  @State savingCount: number = 0;
  private metaStore: Record<string, SettingMetaRaw> = {};
  private valuesStore: Record<string, string> = {};

  @State toastMsg: string = '';
  @State toastIsError: boolean = false;

  aboutToAppear(): void {
    // 登记导航栈：返回手势由 Index.onBackPress 按当前 Tab 精确派发过来
    registerNavStack(3, this.navStack, () => {
      this.activeSub = SUB_NONE;
    });
    const s: AppSettings = connStore.getSettings();
    this.themeMode = s.theme.length > 0 ? s.theme : 'system';
    this.lang = s.lang;
    this.bgImage = s.bgImage ?? '';
    this.bgOpacity = s.bgOpacity ?? 0.25;
    this.loadConnections();
    this.loadBackendSettings();
    // 状态 Tab 已删除，运行状态并入本页：顶部摘要卡 + 二级明细页
    statusStore.init();
    statusStore.refresh();
  }

  aboutToDisappear(): void {
    unregisterNavStack(3);
  }

  private loadConnections(): void {
    this.connections = connStore.getConnections();
    const cur = connStore.getCurrentConnection();
    this.currentId = cur !== null ? cur.id : '';
  }

  private palette(): ThemePalette {
    // 引用 this.isDark 建立响应式依赖：主题切换时整个组件树重渲染
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  // ===================== 二级页面导航 =====================

  /**
   * 打开二级页面。
   *
   * 窄屏（Stack 模式）：push 一层，整屏覆盖，系统侧滑返回可退。
   * 宽屏（Split 模式）：左栏一级列表常驻，右栏只应有一页，
   *   所以用 replace 换页而不是叠栈 —— 否则返回手势要一层层退回去。
   */
  private openSub(id: string): void {
    this.activeSub = id;
    if (this.isWide && this.navStack.size() > 0) {
      this.navStack.replacePathByName(id, subPageParam(id), false);
    } else {
      this.navStack.pushPathByName(id, subPageParam(id), true);
    }
  }

  /** 二级页面返回键：宽屏下左栏常驻可见，SubPageLayer 已隐藏返回键 */
  private closeSub(): void {
    this.navStack.pop();
    this.activeSub = SUB_NONE;
  }

  private currentConnName(): string {
    for (let i = 0; i < this.connections.length; i++) {
      if (this.connections[i].id === this.currentId) {
        return this.connections[i].name;
      }
    }
    return '未配置';
  }

  private themeLabel(): string {
    if (this.themeMode === 'dark') {
      return '深色';
    }
    if (this.themeMode === 'light') {
      return '浅色';
    }
    return '跟随系统';
  }

  private totalKeyCount(): number {
    return Object.keys(this.valuesStore).length;
  }

  /** 核心 + 其他（不含 plugin.*，那些在插件详情页里） */
  private coreKeyCount(): number {
    let n: number = 0;
    for (let i = 0; i < this.sections.length; i++) {
      n = n + this.sections[i].count;
    }
    return n;
  }

  private activeSectionTitle(): string {
    for (let i = 0; i < this.sections.length; i++) {
      if (this.sections[i].id === this.activeSection) {
        return this.sections[i].title;
      }
    }
    return '配置项';
  }

  // ===================== connections =====================

  private selectConnection(id: string): void {
    connStore.setCurrent(id).then(() => {
      const cur: ConnectionConfig | null = connStore.getCurrentConnection();
      if (cur !== null) {
        apiClient.setConnection(cur);
      }
      this.loadConnections();
      this.showToast('已切换连接', false);
    });
  }

  private addConnection(): void {
    const name: string = this.editName.trim();
    const url: string = this.editUrl.trim();
    const apiKey: string = this.editApiKey.trim();
    if (name.length === 0 || url.length === 0) {
      this.showToast('名称和地址不能为空', true);
      return;
    }
    connStore.addConnection(name, url, apiKey).then(() => {
      this.editName = '';
      this.editUrl = '';
      this.editApiKey = '';
      this.showAddForm = false;
      const cur = connStore.getCurrentConnection();
      if (cur !== null) {
        apiClient.setConnection(cur);
      }
      this.loadConnections();
      this.showToast('连接已添加', false);
    });
  }

  private deleteConnection(id: string): void {
    connStore.deleteConnection(id).then(() => {
      this.loadConnections();
      const cur = connStore.getCurrentConnection();
      if (cur !== null) {
        apiClient.setConnection(cur);
      }
      this.showToast('连接已删除', false);
    });
  }

  // ===================== appearance =====================

  private applyTheme(mode: string): void {
    this.themeMode = mode;
    this.persistSettings();
    // 立即翻转全局主题标志，整个 UI 随之切换
    applyThemeMode(mode);
  }

  // ===================== custom background =====================

  private async pickBackgroundImage(): Promise<void> {
    if (this.pickingBg) {
      return;
    }
    this.pickingBg = true;
    try {
      const options = new picker.PhotoSelectOptions();
      options.MIMEType = picker.PhotoViewMIMETypes.IMAGE_TYPE;
      options.maxSelectNumber = 1;
      const photoPicker = new picker.PhotoViewPicker();
      const result = await photoPicker.select(options);
      if (result.photoUris.length === 0) {
        return;
      }
      const srcUri: string = result.photoUris[0];
      const ctx = getContext(this) as common.UIAbilityContext;
      // 文件名带时间戳：Image 组件按 src 字符串做内存缓存，
      // 每次都写同一个 bg_image 会让第二次换图看起来"没生效"。
      // 后缀必须保留：Image 组件按扩展名挑选解码器，无后缀的沙箱文件会解码失败，
      // 表现就是"设置了背景图但没生效"（onError 里能看到 decode 失败）。
      const destPath: string = ctx.filesDir + '/bg_' + Date.now().toString(36) + imageExt(srcUri);
      const srcFile = fileIo.openSync(srcUri, fileIo.OpenMode.READ_ONLY);
      const destFile = fileIo.openSync(destPath,
        fileIo.OpenMode.READ_WRITE | fileIo.OpenMode.CREATE | fileIo.OpenMode.TRUNC);
      fileIo.copyFileSync(srcFile.fd, destFile.fd);
      fileIo.closeSync(srcFile);
      fileIo.closeSync(destFile);
      this.removeOldBgFile();
      // Image 只认带协议头的沙箱 URI，裸路径会被当成资源名而静默失败
      this.bgImage = 'file://' + destPath;
      this.persistSettings();
      this.showToast('背景图已设置', false);
    } catch (e) {
      this.showToast(userMessage('settings.pickBg', e), true);
    }
    this.pickingBg = false;
  }

  /** 删除上一张背景图文件，避免沙箱里越攒越多 */
  private removeOldBgFile(): void {
    const old: string = this.bgImage;
    if (old.length === 0) {
      return;
    }
    const path: string = old.startsWith('file://') ? old.substring(7) : old;
    try {
      fileIo.unlinkSync(path);
    } catch (e) {
      // 文件可能已不存在，忽略
    }
  }

  private clearBackgroundImage(): void {
    this.removeOldBgFile();
    this.bgImage = '';
    this.persistSettings();
    this.showToast('已清除背景图', false);
  }

  private onBgOpacityChange(value: number): void {
    this.bgOpacity = value / 100;
    this.persistSettings();
  }

  private persistSettings(): void {
    const s: AppSettings = emptySettings();
    const old: AppSettings = connStore.getSettings();
    s.lang = old.lang;
    s.theme = this.themeMode.length > 0 ? this.themeMode : (old.theme.length > 0 ? old.theme : 'system');
    s.currentConnId = old.currentConnId;
    s.bgImage = this.bgImage;
    s.bgOpacity = this.bgOpacity;
    connStore.saveSettings(s);
    AppStorage.set<string>('bgImage', this.bgImage);
    AppStorage.set<number>('bgOpacity', this.bgOpacity);
  }

  // ===================== backend kv editor =====================

  private async loadBackendSettings(): Promise<void> {
    if (!apiClient.hasConnection()) {
      this.settingsError = noConnectionMessage();
      return;
    }
    this.loadingSettings = true;
    this.settingsError = '';
    try {
      const resp = await apiClient.getWithTimeout('/settings', 15000);
      const obj: Record<string, Object> = JSON.parse(resp.body) as Record<string, Object>;

      // meta definitions
      this.metaStore = {};
      const rawMeta: Object | undefined = obj['meta'];
      if (rawMeta !== undefined && rawMeta !== null) {
        const mObj: Record<string, Object> = rawMeta as Record<string, Object>;
        for (const mk of Object.keys(mObj)) {
          const item: Record<string, Object> = mObj[mk] as Record<string, Object>;
          const optsArr: Object | undefined = item['options'];
          const opts: string[] = [];
          if (optsArr !== undefined && optsArr !== null) {
            const oa: Object[] = optsArr as Object[];
            for (let i = 0; i < oa.length; i++) {
              opts.push(oa[i] as string);
            }
          }
          const meta: SettingMetaRaw = {
            key: item['key'] as string ?? mk,
            type: item['type'] as string ?? 'string',
            displayName: item['display_name'] as string ?? '',
            description: item['description'] as string ?? '',
            category: item['category'] as string ?? '',
            options: opts,
          };
          this.metaStore[mk] = meta;
        }
      }

      // current values
      this.valuesStore = {};
      const rawVals: Object | undefined = obj['settings'];
      if (rawVals !== undefined && rawVals !== null) {
        const vObj: Record<string, Object> = rawVals as Record<string, Object>;
        for (const vk of Object.keys(vObj)) {
          if (vk.length === 0) {
            continue;
          }
          const val: Object = vObj[vk];
          let strVal: string;
          if (typeof val === 'string') {
            strVal = val as string;
          } else if (typeof val === 'boolean' || typeof val === 'number') {
            strVal = String(val);
          } else {
            strVal = JSON.stringify(val);
          }
          this.valuesStore[vk] = strVal;
        }
      }

      this.buildSections();
      this.selectSection(this.pickInitialSection());
    } catch (e) {
      this.settingsError = userMessage('settings.load', e);
    }
    this.loadingSettings = false;
  }

  /**
   * 把 key 归到分区：
   *  - core.*   → 'core/<category>'，展示在「核心」二级页下
   *  - plugin.* → 'plugin/<name>'，仅用于计数；实际编辑在插件详情页里，
   *               不在这里列出（否则同一批 key 会有两个入口）
   *  - 其余     → 'other'
   */
  private buildSections(): void {
    const ids: string[] = [];
    const counts: Record<string, number> = {};
    const titles: Record<string, string> = {};
    let pluginKeys: number = 0;
    const plugNames: string[] = [];
    for (const key of Object.keys(this.valuesStore)) {
      if (key.startsWith('plugin.')) {
        // 插件配置不在这里列：它属于插件本身，入口在「插件 → 详情 → 插件配置」。
        // 这里只统计，用于提示有多少项在那边。
        pluginKeys = pluginKeys + 1;
        const rest: string = key.substring('plugin.'.length);
        const dot: number = rest.indexOf('.');
        const plugName: string = dot > 0 ? rest.substring(0, dot) : rest;
        if (plugName.length > 0 && plugNames.indexOf(plugName) < 0) {
          plugNames.push(plugName);
        }
        continue;
      }
      let secId: string;
      if (key.startsWith('core.')) {
        const cat: string = this.metaCategory(key);
        secId = cat.length > 0 ? 'core/' + cat : 'core/misc';
        if (titles[secId] === undefined) {
          titles[secId] = cat.length > 0 ? this.categoryTitle(cat) : '未分类';
        }
      } else {
        secId = 'other';
        if (titles[secId] === undefined) {
          titles[secId] = '其他';
        }
      }
      if (counts[secId] === undefined) {
        counts[secId] = 0;
        ids.push(secId);
      }
      counts[secId] = counts[secId] + 1;
    }
    ids.sort((a: string, b: string): number => a.localeCompare(b));
    const secs: SettingsSection[] = [];
    for (const id of ids) {
      secs.push({ id: id, title: titles[id] ?? id, count: counts[id] ?? 0 });
    }
    this.sections = secs;
    this.pluginKeyCount = pluginKeys;
    this.pluginConfigCount = plugNames.length;
  }

  private metaCategory(key: string): string {
    const m: SettingMetaRaw | undefined = this.metaStore[key];
    return m !== undefined && m.category.length > 0 ? m.category : '';
  }

  private categoryTitle(cat: string): string {
    const map: Record<string, string> = {
      'agent': '智能体',
      'daemon': '守护进程',
      'llm': '大模型',
      'sources': '数据源',
      'input': '输入',
      'paths': '路径',
      'resources': '资源',
      'defaults': '默认值',
      'snapshot': '快照',
      'rollback': '回滚',
    };
    const t: string | undefined = map[cat];
    return t !== undefined ? t : cat;
  }

  private pickInitialSection(): string {
    for (let i = 0; i < this.sections.length; i++) {
      if (this.sections[i].id === 'core/agent') {
        return 'core/agent';
      }
    }
    return this.sections.length > 0 ? this.sections[0].id : 'core';
  }

  private selectSection(secId: string): void {
    this.activeSection = secId;
    const entries: SettingEntry[] = [];
    // deterministic order: alphabetical by key
    const keys: string[] = Object.keys(this.valuesStore).filter((k: string): boolean => {
      return this.keyInSection(k, secId);
    });
    keys.sort((a: string, b: string): number => a.localeCompare(b));
    const limit: number = Math.min(keys.length, PAGE_SIZE * 4);
    for (let i = 0; i < limit; i++) {
      const key: string = keys[i];
      const meta: SettingMetaRaw | undefined = this.metaStore[key];
      const entry: SettingEntry = {
        key: key,
        displayName: meta !== undefined && meta.displayName.length > 0 ? meta.displayName : key,
        description: meta !== undefined ? meta.description : '',
        type: meta !== undefined ? meta.type : 'string',
        options: meta !== undefined ? meta.options : [],
        value: this.valuesStore[key] ?? '',
        dirty: false,
      };
      entries.push(entry);
    }
    this.entries = entries;
  }

  private keyInSection(key: string, secId: string): boolean {
    if (secId === 'other') {
      return !key.startsWith('core.') && !key.startsWith('plugin.');
    }
    if (secId.startsWith('core/')) {
      if (!key.startsWith('core.')) {
        return false;
      }
      const cat: string = secId.substring('core/'.length);
      return cat === 'misc' ? this.metaCategory(key).length === 0 : this.metaCategory(key) === cat;
    }
    if (secId.startsWith('plugin/')) {
      const p: string = secId.substring('plugin/'.length);
      return key.startsWith('plugin.' + p + '.');
    }
    return false;
  }

  private hasDirty(): boolean {
    for (let i: number = 0; i < this.entries.length; i++) {
      if (this.entries[i].dirty === true) {
        return true;
      }
    }
    return false;
  }

  /** Persist one key via PUT /settings. bool is sent as "true"/"false" strings like the GUI. */
  private async saveSetting(entry: SettingEntry, newValue: string): Promise<void> {
    let payload: string = newValue.trim();
    if (entry.type === 'bool') {
      payload = newValue === 'true' ? 'true' : 'false';
    }
    this.savingCount = this.savingCount + 1;
    try {
      const body: SaveSettingBody = { key: entry.key, value: payload };
      await apiClient.request('/settings', 'PUT', JSON.stringify(body), 10000);
      this.valuesStore[entry.key] = payload;
      this.markEntry(entry.key, payload, false);
      this.showToast('已保存: ' + entry.displayName, false);
    } catch (e) {
      this.showToast(userMessage('settings.save', e), true);
    }
    this.savingCount = this.savingCount - 1;
  }

  private markEntry(key: string, value: string, dirty: boolean): void {
    const next: SettingEntry[] = [];
    for (let i = 0; i < this.entries.length; i++) {
      const e: SettingEntry = this.entries[i];
      if (e.key === key) {
        const copy: SettingEntry = {
          key: e.key,
          displayName: e.displayName,
          description: e.description,
          type: e.type,
          options: e.options,
          value: value,
          dirty: dirty,
        };
        next.push(copy);
      } else {
        next.push(e);
      }
    }
    this.entries = next;
  }

  private updateEntryValue(key: string, value: string): void {
    const original: string = this.valuesStore[key] ?? '';
    this.markEntry(key, value, value !== original);
  }

  private showToast(msg: string, isError: boolean): void {
    // 颜色标记必须在 animateTo 之外先定好，否则第一帧会用上一条 toast 的配色
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

  build() {
    // Navigation 提供真实导航栈：系统侧滑返回 / 三键返回 / 无障碍返回
    // 都会自动 pop 当前 NavDestination，无需自己拦 onBackPress。
    //
    // 宽屏（>=600vp）改用 Split 模式：navBar（这里的一级入口列表，含底部悬浮导航栏）
    // 常驻左栏，NavDestination（二级页面）渲染在右栏，两栏同时可见。
    Navigation(this.navStack) {
      // ===== 一级：入口列表（分组卡 + 右尖角行） =====
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

        PageTopBar({ title: '设置' })

        // 一级悬浮区：仅在保存进行中显示一个转圈；
        // 已保存/未保存的常驻徽标按用户要求去掉（保存本来就是即时的，不需要状态吊牌）
        NavFloatOverlay({ tab: 3 }) {
          NavFloatRow() {
            if (this.savingCount > 0) {
              LoadingProgress()
                .width(14)
                .height(14)
                .color(this.palette().accent)
            }
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
    // Split 模式下 navBar 常驻可见，onNavBarStateChange 不再触发，
    // 所以在这里统一维护"横滑锁"：宽屏一律锁住（靠点击导航栏切换 Tab，
    // 避免在右栏操作时误触发主 Tab 切换）。
    .onNavBarStateChange((isVisible: boolean) => {
      markSubPageOpen(this.isWide || !isVisible);
    })
    // 模式初始化与切换的唯一钩子：Split 右栏不能空白，自动填入默认二级页面；
    // 退回 Stack 时清栈，否则会残留一个整屏覆盖的二级页面。
    .onNavigationModeChange((mode: NavigationMode) => {
      if (mode === NavigationMode.Split) {
        markSubPageOpen(true);
        if (this.navStack.size() === 0) {
          this.openSub(SUB_STATUS);
        }
      } else {
        this.navStack.clear(false);
        this.activeSub = SUB_NONE;
        markSubPageOpen(false);
      }
    })
  }

  /** 二级页面路由表：name 由 pushPathByName 传入 */
  @Builder
  SubDestination(name: string, param: object) {
    NavDestination() {
      if (name === SUB_STATUS) {
        SubPageLayer({
          title: '运行状态',
          tab: 3,
          onBack: () => {
            this.closeSub();
          },
          showRefresh: true,
          onRefresh: () => {
            statusStore.refresh();
          },
        }) {
          StatusDetailContent()
        }
      } else if (name === SUB_CONNECTIONS) {
        SubPageLayer({
          title: '后端连接',
          tab: 3,
          onBack: () => {
            this.closeSub();
          },
        }) {
          this.ConnectionsContent()
        }
      } else if (name === SUB_APPEARANCE) {
        SubPageLayer({
          title: '外观',
          tab: 3,
          onBack: () => {
            this.closeSub();
          },
        }) {
          this.AppearanceContent()
        }
      } else if (name === SUB_BACKEND) {
        SubPageLayer({
          title: '核心配置',
          tab: 3,
          onBack: () => {
            this.closeSub();
          },
          showRefresh: true,
          onRefresh: () => {
            this.loadBackendSettings();
          },
        }) {
          this.BackendSectionsContent()
        }
      } else if (name === SUB_SECTION) {
        SubPageLayer({
          title: this.activeSectionTitle(),
          tab: 3,
          onBack: () => {
            this.closeSub();
          },
          showRefresh: true,
          onRefresh: () => {
            this.loadBackendSettings();
          },
        }) {
          this.SectionEntriesContent()
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
      // if 控制的节点靠 transition 做进出场，配合 showToast 里的 animateTo
      .transition(TransitionEffect.OPACITY.combine(TransitionEffect.translate({ y: 12 })).animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut }))
    }
  }

  // ===================== 一级入口列表 =====================

  @Builder
  RootEntries() {
    // 运行状态摘要（原「状态」Tab）：整卡可点，进入明细二级页
    StatusSummaryCard({
      onTap: () => {
        this.openSub(SUB_STATUS);
      },
    })

    NavGroup({ caption: '连接' }) {
      NavRow({
        icon: $r('app.media.ic_link'),
        title: '后端连接',
        subtitle: this.connections.length.toString() + ' 个连接配置',
        value: this.currentConnName(),
        selected: this.isWide && this.activeSub === SUB_CONNECTIONS,
        onTap: () => {
          this.openSub(SUB_CONNECTIONS);
        },
      })
      NavRow({
        icon: $r('app.media.ic_tune'),
        title: '核心配置',
        subtitle: this.sections.length.toString() + ' 个分类 · ' + this.coreKeyCount().toString() + ' 项',
        value: this.loadingSettings ? '加载中' : (this.settingsError.length > 0 ? '不可用' : ''),
        showDivider: false,
        selected: this.isWide && (this.activeSub === SUB_BACKEND || this.activeSub === SUB_SECTION),
        onTap: () => {
          this.openSub(SUB_BACKEND);
        },
      })
    }

    NavGroup({ caption: '个性化' }) {
      NavRow({
        icon: $r('app.media.ic_theme'),
        title: '外观',
        subtitle: '主题与背景图',
        value: this.themeLabel(),
        showDivider: false,
        selected: this.isWide && this.activeSub === SUB_APPEARANCE,
        onTap: () => {
          this.openSub(SUB_APPEARANCE);
        },
      })
    }

    if (this.settingsError.length > 0) {
      Text(this.settingsError)
        .fontSize(12)
        .fontColor('#E84026')
        .padding({ left: 4, right: 4 })
    }
  }

  // ===================== 二级：后端连接 =====================

  @Builder
  ConnectionsContent() {
    PlainCard({ caption: '当前连接' }) {
      Row() {
        Column({ space: 3 }) {
          Text(this.currentConnName())
            .fontSize(15)
            .fontColor(this.palette().textPrimary)
          Text(this.currentId.length > 0 ? '已激活，用于所有请求' : '尚未选择连接')
            .fontSize(11)
            .fontColor(this.palette().textMuted)
        }
        .layoutWeight(1)
        .alignItems(HorizontalAlign.Start)

        Button('+ 添加')
          .height(30)
          .fontSize(12)
          .backgroundColor(this.palette().accent)
          .fontColor(Color.White)
          .onClick(() => {
            this.showAddForm = true;
            this.addFormVisible = false;
            this.editName = '';
            this.editUrl = '';
            this.editApiKey = '';
            setTimeout(() => {
              this.addFormVisible = true;
            }, 30);
          })
      }
      .width('100%')
      .alignItems(VerticalAlign.Center)

      if (this.showAddForm) {
        Column() {
          TextInput({ placeholder: '名称 (如 HomeAgent)', text: this.editName })
            .height(36).fontSize(13).fontColor(this.palette().textPrimary)
            .placeholderColor(this.palette().textMuted).backgroundColor(this.palette().bgInput)
            .borderRadius(RADIUS_SM).border({ width: 1, color: this.palette().border })
            .margin({ bottom: 10 })
            .onChange((v: string) => {
              this.editName = v;
            })
          TextInput({ placeholder: '地址 (域名或 http://192.168.1.100:8080)', text: this.editUrl })
            .height(36).fontSize(13).fontColor(this.palette().textPrimary)
            .placeholderColor(this.palette().textMuted).backgroundColor(this.palette().bgInput)
            .borderRadius(RADIUS_SM).border({ width: 1, color: this.palette().border })
            .margin({ bottom: 10 })
            .onChange((v: string) => {
              this.editUrl = v;
            })
          TextInput({ placeholder: 'API Key (可选)', text: this.editApiKey })
            .height(36).fontSize(13).fontColor(this.palette().textPrimary)
            .placeholderColor(this.palette().textMuted).backgroundColor(this.palette().bgInput)
            .borderRadius(RADIUS_SM).border({ width: 1, color: this.palette().border })
            .type(InputType.Password).margin({ bottom: 12 })
            .onChange((v: string) => {
              this.editApiKey = v;
            })

          Row() {
            Button('取消')
              .height(30)
              .fontSize(12)
              .backgroundColor(Color.Transparent)
              .border({ width: 1, color: this.palette().btnGhostBorder })
              .fontColor(this.palette().textSecondary)
              .onClick(() => {
                this.showAddForm = false;
              })
            Blank()
            Button('保存')
              .height(30)
              .fontSize(12)
              .backgroundColor(this.palette().accent)
              .fontColor(Color.White)
              .onClick(() => {
                this.addConnection();
              })
          }
          .width('100%')
        }
        .width('100%')
        .padding(12)
        .borderRadius(RADIUS_MD)
        .backgroundColor(this.palette().bgHover)
        .border({ width: 1, color: this.palette().kvBorder })
        .margin({ top: 12 })
        .alignItems(HorizontalAlign.Start)
        .opacity(this.addFormVisible ? 1 : 0)
        .translate({ y: this.addFormVisible ? 0 : 12 })
        .animation({ duration: 220, curve: Curve.EaseOut })
      }
    }

    PlainCard({ caption: '全部连接' }) {
      if (this.connections.length === 0) {
        Text('暂无连接。点击上方“添加”配置后端地址。')
          .fontSize(12)
          .fontColor(this.palette().textMuted)
      }
      ForEach(this.connections, (conn: ConnectionConfig) => {
        Row() {
          Circle({ width: 8, height: 8 })
            .fill(conn.id === this.currentId ? this.palette().accent : '#77809A')
            .margin({ right: 10 })
          Column() {
            Text(conn.name)
              .fontSize(13)
              .fontColor(this.palette().textPrimary)
              .maxLines(1)
              .textOverflow({ overflow: TextOverflow.Ellipsis })
            Text(conn.url)
              .fontSize(11)
              .fontColor(this.palette().textMuted)
              .maxLines(1)
              .textOverflow({ overflow: TextOverflow.Ellipsis })
          }
          .layoutWeight(1)
          .alignItems(HorizontalAlign.Start)

          if (conn.id !== this.currentId) {
            Button('切换')
              .height(26)
              .fontSize(11)
              .backgroundColor(Color.Transparent)
              .border({ width: 1, color: this.palette().btnGhostBorder })
              .fontColor(this.palette().textSecondary)
              .margin({ right: 6 })
              .onClick(() => {
                this.selectConnection(conn.id);
              })
          } else {
            Text('使用中')
              .fontSize(11)
              .fontColor(this.palette().accent)
              .padding({ left: 8, right: 8, top: 2, bottom: 2 })
              .borderRadius(RADIUS_SM)
              .backgroundColor(this.palette().accentBg)
              .margin({ right: 6 })
          }
          Button('删除')
            .height(26)
            .fontSize(11)
            .backgroundColor(Color.Transparent)
            .border({ width: 1, color: 'rgba(232, 64, 38, 0.45)' })
            .fontColor('#E84026')
            .onClick(() => {
              this.deleteConnection(conn.id);
            })
        }
        .width('100%')
        .padding(10)
        .borderRadius(RADIUS_SM)
        .backgroundColor(this.palette().bgHover)
        .border({
          width: { left: 3 },
          color: conn.id === this.currentId ? this.palette().accent : Color.Transparent,
        })
        .margin({ bottom: 6 })
      }, (conn: ConnectionConfig) => conn.id)
    }
  }

  // ===================== 二级：外观 =====================

  @Builder
  AppearanceContent() {
    PlainCard({ caption: '主题' }) {
      Row({ space: 8 }) {
        this.ThemeOption('跟随系统', 'system')
        this.ThemeOption('浅色', 'light')
        this.ThemeOption('深色', 'dark')
      }
      .width('100%')

      Text(this.themeMode === 'system'
        ? '当前跟随系统，系统切换深浅色时自动跟随'
        : (this.themeMode === 'dark' ? '当前强制深色主题' : '当前强制浅色主题'))
        .fontSize(11)
        .fontColor(this.palette().textMuted)
        .margin({ top: 10 })
    }

    PlainCard({ caption: '背景图' }) {
      Row() {
        Column() {
          Text('自定义背景图')
            .fontSize(14)
            .fontColor(this.palette().textPrimary)
          Text(this.bgImage.length > 0 ? '已设置背景图' : '未设置背景图')
            .fontSize(11)
            .fontColor(this.palette().textMuted)
            .margin({ top: 2 })
        }
        .alignItems(HorizontalAlign.Start)
        .layoutWeight(1)

        if (this.bgImage.length > 0) {
          Button('更换')
            .height(28)
            .fontSize(12)
            .backgroundColor(Color.Transparent)
            .border({ width: 1, color: this.palette().btnGhostBorder })
            .fontColor(this.palette().textSecondary)
            .margin({ right: 6 })
            .onClick(() => {
              this.pickBackgroundImage();
            })
          Button('清除')
            .height(28)
            .fontSize(12)
            .backgroundColor(Color.Transparent)
            .border({ width: 1, color: 'rgba(232, 64, 38, 0.45)' })
            .fontColor('#E84026')
            .onClick(() => {
              this.clearBackgroundImage();
            })
        } else {
          Button(this.pickingBg ? '选择中...' : '选择图片')
            .height(28)
            .fontSize(12)
            .backgroundColor(this.palette().accent)
            .fontColor(Color.White)
            .onClick(() => {
              this.pickBackgroundImage();
            })
        }
      }
      .width('100%')

      if (this.bgImage.length > 0) {
        Row() {
          Text('透明度')
            .fontSize(11)
            .fontColor(this.palette().textMuted)
          Slider({
            value: Math.round(this.bgOpacity * 100),
            min: 5,
            max: 60,
            step: 1,
          })
            .layoutWeight(1)
            .selectedColor(this.palette().accent)
            .trackColor(this.palette().bgHover)
            .margin({ left: 8, right: 8 })
            .onChange((v: number, mode: SliderChangeMode) => {
              if (mode === SliderChangeMode.Moving || mode === SliderChangeMode.Click) {
                this.onBgOpacityChange(v);
              }
            })
          Text(Math.round(this.bgOpacity * 100).toString() + '%')
            .fontSize(11)
            .fontColor(this.palette().textSecondary)
            .width(32)
        }
        .width('100%')
        .alignItems(VerticalAlign.Center)
        .margin({ top: 12 })
      }
    }
  }

  // ===================== 二级：核心配置分类列表 =====================

  @Builder
  BackendSectionsContent() {
    if (this.settingsError.length > 0) {
      PlainCard({ caption: '状态' }) {
        Text(this.settingsError)
          .fontSize(12)
          .fontColor('#E84026')
      }
    }

    NavGroup({ caption: '核心' }) {
      ForEach(this.sections, (sec: SettingsSection, idx: number) => {
        NavRow({
          icon: $r('app.media.ic_tune'),
          title: sec.title,
          subtitle: sec.count.toString() + ' 项配置',
          showDivider: idx < this.sections.length - 1,
          selected: this.isWide && this.activeSub === SUB_SECTION
            && this.activeSection === sec.id,
          onTap: () => {
            this.selectSection(sec.id);
            this.openSub(SUB_SECTION);
          },
        })
      }, (sec: SettingsSection) => sec.id + sec.count.toString())
    }

    // 插件配置不在这里编辑：入口在插件页的详情里，这里只指路，避免两处重复入口
    if (this.pluginKeyCount > 0) {
      Text('插件的 ' + this.pluginKeyCount.toString() + ' 项配置（' +
        this.pluginConfigCount.toString() + ' 个插件）在「插件 → 选择插件 → 插件配置」中修改。')
        .fontSize(12)
        .fontColor(this.palette().textMuted)
        .width('100%')
        .padding({ left: 4, right: 4 })
    }

    if (this.sections.length === 0 && !this.loadingSettings) {
      Text('未获取到配置分类。检查后端连接后点击刷新。')
        .fontSize(12)
        .fontColor(this.palette().textMuted)
        .padding({ left: 4 })
    }
  }

  // ===================== 三级：某个分区的配置项 =====================

  @Builder
  SectionEntriesContent() {
    ForEach(this.entries, (entry: SettingEntry) => {
      this.SettingCard(entry)
    }, (entry: SettingEntry) => entry.key + '|' + entry.value + '|' + (entry.dirty ? 'd' : 'c'))

    if (!this.loadingSettings && this.entries.length === 0) {
      Text('该分区暂无配置项')
        .fontSize(12)
        .fontColor(this.palette().textMuted)
        .padding(16)
    }
  }

  @Builder
  ThemeOption(label: string, mode: string) {
    // 按压缩放由 MotionBase 统一；flexWeight: 1 让三枚选项在 Row 里继续等分。
    MotionBase({ pressEnabled: true, flexWeight: 1 }) {
      Column() {
        Text(label)
          .fontSize(12)
          .fontColor(this.themeMode === mode ? Color.White : this.palette().textSecondary)
          // 子节点的颜色迁移要自己声明：父容器的 .animation() 不下传
          .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
      }
      .justifyContent(FlexAlign.Center)
      .width('100%')
      .height(34)
      .borderRadius(RADIUS_MD)
      .backgroundColor(this.themeMode === mode ? this.palette().accent : this.palette().bgHover)
      // 三选一的选中态迁移：底色与描边一起过渡。写在 .border 之后、
      // 覆盖它上面的所有状态驱动属性。
      .border({
        width: 1,
        color: this.themeMode === mode ? this.palette().accent : this.palette().btnGhostBorder,
      })
      .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
      .onClick(() => {
        this.applyTheme(mode);
      })
    }
  }

  @Builder
  SettingCard(entry: SettingEntry) {
    Column() {
      Row() {
        Text(entry.displayName)
          .fontSize(13)
          .fontWeight(FontWeight.Medium)
          .fontColor(this.palette().textPrimary)
          .maxLines(2)
          .textOverflow({ overflow: TextOverflow.Ellipsis })
          .layoutWeight(1)
        Text(entry.dirty ? '未保存' : entry.type)
          .fontSize(10)
          .fontColor(entry.dirty ? '#D99A2B' : this.palette().textMuted)
          .padding({ left: 6, right: 6, top: 1, bottom: 1 })
          .borderRadius(RADIUS_SM)
          .backgroundColor(entry.dirty ? 'rgba(217, 154, 43, 0.16)' : this.palette().bgHover)
      }
      .width('100%')

      Text(entry.key)
        .fontSize(10)
        .fontColor(this.palette().textMuted)
        .maxLines(1)
        .textOverflow({ overflow: TextOverflow.Ellipsis })
        .margin({ top: 1 })

      if (entry.description.length > 0) {
        Text(entry.description)
          .fontSize(11)
          .fontColor(this.palette().textSecondary)
          .maxLines(3)
          .textOverflow({ overflow: TextOverflow.Ellipsis })
          .margin({ top: 3 })
      }

      // control row per type
      if (entry.type === 'bool') {
        Row() {
          Text(entry.value === 'true' ? 'true' : 'false')
            .fontSize(12)
            .fontColor(entry.value === 'true' ? '#17A964' : this.palette().textMuted)
            .animation({ duration: ANIM_NORMAL, curve: Curve.EaseOut })
          Blank()
          Toggle({ type: ToggleType.Switch, isOn: entry.value === 'true' })
            .selectedColor(this.palette().accent)
            .onChange((on: boolean) => {
              this.saveSetting(entry, on ? 'true' : 'false');
            })
        }
        .width('100%')
        .margin({ top: 8 })
      } else if (entry.type === 'select' && entry.options.length > 0) {
        Flex({
          direction: FlexDirection.Row,
          justifyContent: FlexAlign.Start,
          alignItems: ItemAlign.Center,
          wrap: FlexWrap.Wrap,
        }) {
          ForEach(entry.options, (opt: string) => {
            Button(opt)
              .height(26)
              .fontSize(11)
              .margin({ right: 6, bottom: 6 })
              .backgroundColor(entry.value === opt ? this.palette().accent : this.palette().bgHover)
              .fontColor(entry.value === opt ? Color.White : this.palette().textSecondary)
              // 选中项迁移：底色与字色一起过渡，避免整排选项同时硬切
              .animation({ duration: ANIM_FAST, curve: Curve.EaseOut })
              .onClick(() => {
                this.saveSetting(entry, opt);
              })
          }, (opt: string) => opt)
        }
        .width('100%')
        .margin({ top: 8 })
      } else if (entry.type === 'password') {
        Row() {
          TextInput({ text: entry.value })
            .height(36)
            .fontSize(13)
            .fontColor(this.palette().textPrimary)
            .placeholderColor(this.palette().textMuted)
            .backgroundColor(this.palette().bgInput)
            .borderRadius(RADIUS_SM)
            .border({ width: 1, color: this.palette().border })
            .type(InputType.Password)
            .layoutWeight(1)
            .onChange((v: string) => {
              this.updateEntryValue(entry.key, v);
            })
          Button('保存')
            .height(30)
            .fontSize(12)
            .backgroundColor(entry.dirty ? '#D99A2B' : this.palette().accent)
            .fontColor(Color.White)
            .margin({ left: 8 })
            .onClick(() => {
              this.saveSetting(entry, this.currentValueOf(entry));
            })
        }
        .width('100%')
        .margin({ top: 8 })
      } else if (entry.type === 'text') {
        TextArea({ text: entry.value })
          .width('100%')
          .fontSize(13)
          .fontColor(this.palette().textPrimary)
          .backgroundColor(this.palette().bgInput)
          .borderRadius(RADIUS_SM)
          .border({ width: 1, color: this.palette().border })
          .constraintSize({ minHeight: 60, maxHeight: 200 })
          .margin({ top: 8 })
          .onChange((v: string) => {
            this.updateEntryValue(entry.key, v);
          })
        Row() {
          Blank()
          Button('保存')
            .height(30)
            .fontSize(12)
            .backgroundColor(entry.dirty ? '#D99A2B' : this.palette().accent)
            .fontColor(Color.White)
            .onClick(() => {
              this.saveSetting(entry, this.currentValueOf(entry));
            })
        }
        .width('100%')
        .margin({ top: 6 })
      } else {
        // string / int / duration
        Row() {
          TextInput({ text: entry.value })
            .height(36)
            .fontSize(13)
            .fontColor(this.palette().textPrimary)
            .placeholderColor(this.palette().textMuted)
            .backgroundColor(this.palette().bgInput)
            .borderRadius(RADIUS_SM)
            .border({ width: 1, color: this.palette().border })
            .layoutWeight(1)
            .onChange((v: string) => {
              this.updateEntryValue(entry.key, v);
            })
          Button('保存')
            .height(30)
            .fontSize(12)
            .backgroundColor(entry.dirty ? '#D99A2B' : this.palette().accent)
            .fontColor(Color.White)
            .margin({ left: 8 })
            .onClick(() => {
              this.saveSetting(entry, this.currentValueOf(entry));
            })
        }
        .width('100%')
        .margin({ top: 8 })
      }
    }
    .width('100%')
    .padding(14)
    .borderRadius(RADIUS_MD)
    .backgroundColor(this.palette().bgCard)
    .border({
      width: 1,
      color: entry.dirty ? '#D99A2B' : this.palette().glassBorder,
    })
    .margin({ bottom: 10 })
    .alignItems(HorizontalAlign.Start)
  }

  private currentValueOf(entry: SettingEntry): string {
    // read latest edited value from entries store (markEntry keeps copies)
    for (let i = 0; i < this.entries.length; i++) {
      if (this.entries[i].key === entry.key) {
        return this.entries[i].value;
      }
    }
    return entry.value;
  }
}

const RADIUS_SM: number = 6;
const RADIUS_MD: number = 10;

interface SettingMetaRaw {
  key: string;
  type: string;
  displayName: string;
  description: string;
  category: string;
  options: string[];
}

/**
 * 从 picker 返回的 URI 里取图片后缀（带点）。
 * Image 组件依赖后缀选择解码器；沙箱里存成无后缀文件会静默解码失败，
 * 用户看到的就是"背景图设置了却不生效"。取不到后缀时兜底 .jpg。
 */
function imageExt(uri: string): string {
  let s: string = uri;
  const q: number = s.indexOf('?');
  if (q >= 0) {
    s = s.substring(0, q);
  }
  const dot: number = s.lastIndexOf('.');
  const slash: number = s.lastIndexOf('/');
  if (dot > slash && dot < s.length - 1) {
    const ext: string = s.substring(dot).toLowerCase();
    if (ext.length <= 5) {
      return ext;
    }
  }
  return '.jpg';
}
