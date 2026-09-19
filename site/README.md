# 官网（静态站）

单文件、零构建、零依赖的落地页。与仓库 WebUI 的做法一致（原生 HTML/CSS/JS，无打包步骤）。

## 文件

```
site/
├── index.html          单文件站点（内联 CSS/JS）
└── assets/
    ├── logo.svg        logo（来自 assets/branding/logo-h-he.svg）
    └── mascot.webp     看板娘立绘（来自 assets/branding/mascot-xiaozhai.webp）
```

资源是**复制**而非引用上级目录，因此 `site/` 可整体拷走独立部署。

## 本地预览

```bash
cd site && python3 -m http.server 8799
# 打开 http://127.0.0.1:8799/
```

## 部署

纯静态，任意 web 服务器指向 `site/` 即可：

```nginx
server {
    listen 80;
    server_name your.domain;
    root /path/to/TrueAgent/site;
    index index.html;
    location / { try_files $uri $uri/ =404; }
}
```

## 设计说明

- **配色取自品牌指南**：蓝 `#3b82f6` / 青 `#06b6d4` / 金 `#f59e0b`，
  正好对应三层记忆 Context / Document / Graph，因此三层记忆那节直接用了这三色做色条。
- **立绘按「有意的圆角卡面」呈现**，不做抠图：原图是白底 + 蓝紫渐变外框，
  抠图会连带毁掉白裙子与发丝高光（实测背景连通域分析：白底与裙子同色，
  flood-fill 会让 42% 的身体变成透明）。改为圆角 + 发光边框 + 底部渐隐，
  让方形图与深色页面自然衔接。
- **淡入动画是渐进增强**：内容默认可见，仅当 JS 真的跑起来才加 `.js-reveal` 接管。
  早期版本把 `opacity:0` 写在默认样式里，结果 JS 未执行时整页永久空白
  （实测 30 个元素里 26 个停在不可见）——现在禁用 JS 也完整可读。
- 尊重 `prefers-reduced-motion`；架构图的 `<pre>` 在窄屏横向滚动，避免整页撑宽。

## 改内容前请核对事实

页面上的数字（外部插件数、工具数、代码行数、适配器数）都是**实测值**，
不是估的。改动前建议重新取一遍：

```bash
ls /home/newqqagent/plugins/ | grep -v hmap | wc -l          # 外部插件
grep -oE '[0-9]+ tools' $(ls -t /home/newqqagent/log/*.log | head -1) | tail -1
ls internal/lua/adapters/*.lua | wc -l                        # LLM 适配器
find . -name '*.go' -not -path './.git/*' -not -path './.go/*' -not -path './third_party/*' | xargs wc -l | tail -1
```
