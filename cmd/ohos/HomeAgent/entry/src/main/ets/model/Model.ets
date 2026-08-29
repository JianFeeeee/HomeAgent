export interface ConnectionConfig {
  id: string;
  name: string;
  url: string;
  apiKey: string;
  type: string; // 'webui' | 'cli'
}

export interface ToolCallInfo {
  name: string;
  args: string;
  result?: string;
  plugin?: string;
  status?: string; // 'ok' | 'error' | 'denied' | 'running'
  open?: boolean;
}

export interface ChatMessage {
  id: number;
  role: string; // 'user' | 'assistant'
  content: string;
  reasoningContent?: string;
  reasoningOpen?: boolean;
  isStreaming?: boolean;
  isFinal?: boolean;
  toolCalls?: ToolCallInfo[];
  /** 消息来源通道：'webui' | 'channel' | 'webui/<device_id>' 等；用于区分设备/渠道消息 */
  source?: string;
  /** 图片/文件附件（后端 ChatMsg.attachment） */
  attachment?: ChatAttachment;
}

export interface HistoryMessage {
  role: string;
  content: string;
  source?: string;
  timestamp?: string;
  attachment?: ChatAttachment;
}

export interface AgentStatus {
  version?: string;
  uptime_ms?: number;
  agent?: string;
  provider?: string;
  memory_events?: number;
}

export interface DeviceInfo {
  deviceId: string;
  name: string;
  kind: string;
  online: boolean;
  authorized: boolean;
  caps: string[];
  hostname?: string;
  platform?: string;
}

export interface PluginInfo {
  name: string;
  nameZh?: string;
  nameEn?: string;
  version?: string;
  description?: string;
  author?: string;
  deprecated?: boolean;
  builtin: boolean;
  loaded: boolean;
  disabled: boolean;
  tools?: string[];
}

/**
 * One row in the merged plugin list:
 * kernel.plugins ∪ installed(/plugins) ∪ disabled(/plugins/disabled)，
 * 与 WebGUI renderPlugins 的合并口径一致。
 */
export interface PluginRow {
  name: string;
  /** /kernel.plugins 里出现且 loaded=true 才算已加载 */
  loaded: boolean;
  /** 出现在 /plugins/disabled 列表里 */
  disabled: boolean;
  /** 已安装的外部插件（/plugins 返回） */
  external: boolean;
  version?: string;
  description?: string;
  tools?: string[];
}

/**
 * GET /plugins/{name} 返回的插件清单明细。
 * 字段与后端 pluginmgr 的 pluginInfo 一一对应；
 * WebGUI 只把它 JSON.stringify 到 <pre> 里，这里改为结构化展示。
 */
export interface PluginDetail {
  name: string;
  version: string;
  description: string;
  author: string;
  license: string;
  homepage: string;
  repository: string;
  entry: string;
  minVersion: string;
  tags: string[];
  deprecated: boolean;
  files: string[];
}

export function emptyPluginDetail(): PluginDetail {
  const d: PluginDetail = {
    name: '',
    version: '',
    description: '',
    author: '',
    license: '',
    homepage: '',
    repository: '',
    entry: '',
    minVersion: '',
    tags: [],
    deprecated: false,
    files: [],
  };
  return d;
}

/**
 * 聊天消息附件。字段与后端 webui 的 Attachment 严格一致，只有四个：
 * type / url / size / name —— 后端不提供 mime、尺寸、本地路径。
 */
export interface ChatAttachment {
  /** 'image' | 'file' */
  type: string;
  /** /files/<name>、/uploads/<name> 或远程 http(s) 地址 */
  url: string;
  /** 字节数；远程 URL 为 0 */
  size: number;
  /** 展示用文件名 */
  name: string;
}

export interface AppSettings {
  lang: string; // 'zh' | 'en'
  theme: string; // 'dark' | 'light' | 'system'
  currentConnId: string;
  /** 自定义背景图路径（应用沙箱内文件路径），空串表示未设置 */
  bgImage: string;
  /** 背景图不透明度 0..1 */
  bgOpacity: number;
}

export function emptySettings(): AppSettings {
  const s: AppSettings = {
    lang: 'zh',
    theme: 'dark',
    currentConnId: '',
    bgImage: '',
    bgOpacity: 0.25,
  };
  return s;
}

export function defaultConnection(): ConnectionConfig {
  const c: ConnectionConfig = {
    id: '',
    name: 'HomeAgent',
    url: 'http://192.168.2.60:8080',
    apiKey: 'jinrui233719',
    type: 'webui',
  };
  return c;
}
