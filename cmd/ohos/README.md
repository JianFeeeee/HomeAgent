# HomeAgent 鸿蒙客户端

HarmonyOS / OpenHarmony 原生客户端，用 ArkTS + ArkUI 实现（不是 WebView 套壳）。
功能与 WebUI 对齐：SSE 流式对话、工具调用卡片、思考过程折叠、附件上传预览、
设备桥、插件管理、设置编辑、宽屏双栏、深浅色主题。

## 工程结构

```
HomeAgent/
├── AppScope/                     应用级配置与图标
├── entry/src/main/
│   ├── ets/
│   │   ├── common/               通信与全局状态
│   │   │   ├── ApiClient.ets     REST 客户端（X-API-Key 鉴权、超时、二进制附件）
│   │   │   ├── SseClient.ets     SSE 长连接（Last-Event-ID 断线续传）
│   │   │   ├── DeviceBridge.ets  设备桥：把本机能力暴露给 agent
│   │   │   ├── BridgeRouter.ets  桥请求路由
│   │   │   ├── BridgeCaps.ets    能力声明
│   │   │   ├── ConnStore.ets     连接配置持久化
│   │   │   ├── StatusStore.ets   运行状态缓存
│   │   │   ├── NavBarController.ets / NavStackRegistry.ets  导航
│   │   │   ├── Constants.ets     主题色板、圆角、超时、分页大小
│   │   │   └── UserError.ets     错误转人类可读文案
│   │   ├── components/           可复用组件
│   │   │   ├── MarkdownView.ets      流式 Markdown（增量渲染）
│   │   │   ├── StaticMarkdown.ets    静态 Markdown（历史消息，一次成型）
│   │   │   ├── Attachment.ets        附件卡片 + 详情
│   │   │   ├── StatusCards.ets       状态卡片
│   │   │   ├── SettingsEditor.ets    配置编辑器
│   │   │   ├── PageTopBar.ets        顶栏 + 悬浮按钮
│   │   │   ├── SubPage.ets           二级页容器
│   │   │   └── GradientBackground.ets
│   │   ├── model/Model.ets       共享类型定义
│   │   ├── pages/                页面
│   │   │   ├── Index.ets         Tab 容器（入口）
│   │   │   ├── ChatPage.ets      对话
│   │   │   ├── DevicePage.ets    设备
│   │   │   ├── PluginsPage.ets   插件
│   │   │   └── SettingsPage.ets  设置
│   │   └── entryability/EntryAbility.ets
│   ├── module.json5              权限、能力声明
│   └── resources/                字符串、颜色、图标、页面路由表
├── build-profile.json5.example   构建/签名配置模板（复制后填本机签名材料）
└── oh-package.json5              依赖
```

## 编译

需要 DevEco Studio 或 [command-line-tools](https://developer.huawei.com/consumer/cn/deveco-studio/)。
本工程用 `compatibleSdkVersion 6.1.1(24)` / `compileSdkVersion 26.0.0`。

1. **准备签名配置**（`build-profile.json5` 含密码明文，未入库）：

   ```bash
   cd cmd/ohos/HomeAgent
   cp build-profile.json5.example build-profile.json5
   ```

   把 `REPLACE_WITH_YOUR_*` 换成本机 DevEco 生成的调试签名材料，
   默认在 `~/.ohos/config/` 下（`.cer` / `.p7b` / `.p12` 三件套 + 两个密码）。
   用 DevEco Studio 打开工程会自动生成，命令行可参考 `deveco-cli` 生成签名材料。

2. **构建 HAP**：

   ```bash
   # hvigorw 未入库（本机是符号链接），直接用 command-line-tools 里的
   /path/to/command-line-tools/bin/hvigorw \
     --mode module -p module=entry@default assembleHap --no-daemon
   ```

   产物在 `entry/build/default/outputs/default/entry-default-signed.hap`。

3. **安装到设备**：

   ```bash
   hdc install entry/build/default/outputs/default/entry-default-signed.hap
   ```

## 连接 homed

首次启动在「设置」里填：

- **服务地址**：`http://<homed 主机>:8080`（WebUI 插件监听端口）
- **API Key**：homed 的 `plugin.webui.api_key`

客户端所有请求走 `<服务地址>/api/v1/*`，带 `X-API-Key` 头。
附件路径 `/files/` `/uploads/` 不带 `/api/v1` 前缀，同样携带鉴权头。

设备桥需要 homed 启用 `remotedevice` 插件（默认 9890），
在「设备」页填 ws token 后本机能力即可被 agent 调用。

## 注意事项

- **聊天历史分页**：首屏只拉最新 `CHAT_PAGE_SIZE`（40）条，向上滚动触顶自动加载更早的。
  服务端 `/chat/history` 支持 `limit` / `before` 游标；工具调用详情与思考内容完整下发不裁剪。
- **修改主题色**：改 `common/Constants.ets` 的 `DARK_PALETTE` / `LIGHT_PALETTE`，全局生效。
- **新增页面**：同时在 `resources/base/profile/main_pages.json` 注册，且只有入口页带 `@Entry`。
- 项目代码部分由 AI 辅助生成，改动请自行评估。
