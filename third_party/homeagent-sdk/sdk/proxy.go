package sdk

import (
	"net"
	"strconv"
	"strings"
)

// 反向代理声明：插件告诉 HomeAgent「我起了个 HTTP 服务，请把它反代出去」。
//
// 为什么需要：插件自带 Web UI / HTTP API 时，监听地址在插件自己的配置里
// （如 127.0.0.1:12100），外部无从得知；而 webui 的对外端口通常只有一个
// （默认 :8080，且常经 frp 单端口隧道穿透）。没有声明机制时，用户只能
// 「知道端口 + 自己配转发」，插件换端口就失效。
//
// 设计取舍——**声明式而非注册式**：声明写在 plugin.json 里，由 HomeAgent
// 在加载插件时读取聚合，而不是让插件在运行期调 API 注册。理由：
//  1. 静态可发现：未启动/已崩溃的插件，其服务声明依然可见（可给出准确报错
//     「插件 X 声明了 ui 但目标 127.0.0.1:12100 不可达」，而不是静默 404）；
//  2. 可版本化：声明随插件包一起分发、可 diff、可审计；
//  3. 旧内核无害：manifest 解析忽略未知字段，未支持该能力的 HomeAgent 读旧
//     插件、或旧 HomeAgent 读新插件都不会报错。
//
// 安全性：**不声明 = 不被反代**。声明本身就是能力声明，因此不需要在
// capabilities 里另外开一个开关——最小权限默认生效。
//
// 反代路径：HomeAgent 按 Host 路由（子域名标签 → Target），而非路径前缀。
// 理由：插件前端普遍使用根绝对路径（`fetch('/api/status')`），放在路径前缀
// 下会被劫持到 HomeAgent 自己的路由上；Host 路由下根路径天然正确，
// 插件前端**零改动**。这也让「只穿透一个端口」成立：同一端口按 Host 分发。
type ProxyDecl struct {
	// Name 是同一插件内多条声明的唯一标识（如 "ui"、"api"）。
	// 省略时由 HomeAgent 按声明顺序补 "default"/"ui"/"api"... 仅用于展示与日志。
	Name string `json:"name,omitempty"`

	// Host 是**子域名标签**（不含基域名），如 "huawei" 对应 huawei.<基域名>。
	//
	// 约束：仅小写字母、数字与连字符，不以连字符开头/结尾，长度 ≤ 63
	// （DNS label 规则）。省略时默认取插件名（下划线转连字符，因为下划线
	// 不是合法 DNS label 字符）。
	//
	// 冲突处理：两个插件声明同一 Host 时，HomeAgent 不做「后者覆盖前者」——
	// 那样会让先声明者静默消失。冲突条目被拒绝并在反代表里记录原因。
	Host string `json:"host,omitempty"`

	// Target 是上游地址，形如 "127.0.0.1:12100" 或 "http://127.0.0.1:12100"。
	// 可带路径前缀（如 "127.0.0.1:3000/base"），HomeAgent 转发时保留该前缀。
	//
	// 端口由插件自己填它**实际监听**的地址，避免「声明与实际漂移」。
	Target string `json:"target"`

	// WebSocket 表示该服务需要 WebSocket 升级透传（默认 false）。
	//
	// 为什么必须显式声明而不是「有 Upgrade 头就转」：WS 是长连接，会占用
	// 反代侧连接与 goroutine，且绕过普通请求的响应缓冲/超时逻辑。默认关闭
	// 让普通 HTTP 服务的失败模式保持简单；未声明时的升级请求会被明确拒绝，
	// 而不是静默降级成普通请求（后者表现为前端一直重连、排查困难）。
	WebSocket bool `json:"websocket,omitempty"`

	// Auth 决定这条反代由谁保护，取值见 ProxyAuthNone / ProxyAuthHomeAgent。
	// 空串等价于 ProxyAuthHomeAgent（默认安全）。
	//
	// 为什么做成可声明项：设备网关（remotedevice）这类服务的调用方是**设备**，
	// 它们不可能持有浏览器会话 cookie，而服务自身已有接入令牌（如 ws_token）。
	// 强制走 HomeAgent 门户鉴权会把这类链路挡死；反过来，插件自带的 UI 若
	// 声明 none，就等于把管理界面裸露给任何能访问该端口的人。
	// 因此必须由插件**逐条**声明，而不是全局一刀切。
	Auth string `json:"auth,omitempty"`
}

// ProxyAuth 取值。空串按 ProxyAuthHomeAgent 处理（安全的默认）。
const (
	// ProxyAuthHomeAgent 表示由 HomeAgent 统一保护：浏览器走门户会话
	// （homeagent_session cookie），非浏览器客户端走 X-API-Key。
	// 两者都没有时返回 401，而不是把请求透传给上游。
	ProxyAuthHomeAgent = "homeagent"

	// ProxyAuthNone 表示不经 HomeAgent 鉴权，直接把请求转发给上游。
	//
	// 适用场景：上游自己有鉴权且调用方不是浏览器（设备/嵌入式客户端），
	// 或上游是刻意公开的服务。选用它意味着**信任上游自身的鉴权**，
	// 且该服务在网络层可达范围内对所有人开放。
	ProxyAuthNone = "none"
)

// ValidProxyAuth 校验 Auth 取值；空串合法（等价 ProxyAuthHomeAgent）。
func ValidProxyAuth(auth string) bool {
	switch auth {
	case "", ProxyAuthHomeAgent, ProxyAuthNone:
		return true
	}
	return false
}

// EffectiveProxyAuth 返回生效的鉴权模式（空串归一化为 ProxyAuthHomeAgent）。
func EffectiveProxyAuth(auth string) string {
	if auth == "" {
		return ProxyAuthHomeAgent
	}
	return auth
}

// ValidProxyHostLabel 校验子域名标签是否合法（DNS label 规则）。
//
// 独立成导出函数：插件作者在写声明时、HomeAgent 在加载时、工具链在打包时
// 都要用同一套规则判定，避免三处各写一份而互相不一致。
func ValidProxyHostLabel(label string) bool {
	if label == "" || len(label) > 63 {
		return false
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

// NormalizeProxyHost 由插件名派生默认 Host 标签。
//
// 下划线转连字符：插件名允许下划线（huawei_smarthome），但 DNS label 不允许，
// 直接用会导致该子域名无法解析——这里统一转换，避免每个插件各自碰运气。
func NormalizeProxyHost(pluginName string) string {
	s := strings.ToLower(strings.TrimSpace(pluginName))
	s = strings.ReplaceAll(s, "_", "-")
	// 去掉其它非法字符，保证结果是合法 label（宁可退化成保守值也不产出非法域名）
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			b.WriteByte(c)
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "plugin"
	}
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	return out
}

// ValidateProxyDecl 校验一条反代声明，返回人类可读的错误说明（合法时为空）。
//
// 为什么要在 SDK 里做校验：HomeAgent 加载插件时必须能明确拒绝坏声明并说明
// 原因（而不是静默忽略导致用户以为配好了）；插件作者也需要在本地就能查出
// 拼错的 Target/Host。同一套规则两端共用。
func ValidateProxyDecl(d ProxyDecl) string {
	if strings.TrimSpace(d.Target) == "" {
		return "target 为空：必须给出上游地址（如 127.0.0.1:12100 或 http://127.0.0.1:12100）"
	}
	if !ValidProxyAuth(d.Auth) {
		return "auth 取值非法：" + d.Auth + "（只允许 \"\" / \"homeagent\" / \"none\"）"
	}
	if d.Host != "" && !ValidProxyHostLabel(d.Host) {
		return "host 不是合法的子域名标签（只允许小写字母/数字/连字符，且不以连字符开头结尾）: " + d.Host
	}
	// Target 的 host:port 部分必须可解析；路径前缀允许保留。
	//
	// 规则（刻意从严，因为地址写错是最常见的声明错误，而错误的反代会把
	// 用户带到别处去）：
	//   - 带 scheme 时（http://…）允许省略端口，由反代层按 scheme 补默认值；
	//   - 不带 scheme 时必须给出 host:port；
	//   - 端口必须是数字（SplitHostPort 本身不校验数字，"host:abc" 会通过）。
	scheme := ""
	raw := d.Target
	if i := strings.Index(raw, "://"); i >= 0 {
		scheme = strings.ToLower(raw[:i])
		if scheme != "http" && scheme != "https" {
			return "target scheme 只支持 http/https（WS 由 websocket 字段声明，不写 ws://）: " + d.Target
		}
		raw = raw[i+3:]
	}
	if i := strings.IndexByte(raw, '/'); i >= 0 {
		raw = raw[:i]
	}
	if raw == "" {
		return "target 缺少主机部分: " + d.Target
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		if scheme == "" {
			return "target 必须给出 host:port（或带 http:// 前缀以便省略端口）: " + d.Target
		}
		// 带 scheme 且解析失败：只剩主机名一种合法情形。
		host, port = raw, ""
	}
	if host == "" {
		return "target 缺少主机部分: " + d.Target
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "target 端口非法（应为 1-65535 的数字）: " + d.Target
		}
	}
	return ""
}

// ProxyDeclarer 是内核注入的「收集反代声明」回调。
//
// 为什么需要运行期通道（明明主要走 plugin.json 自动发现）：**内置插件**
// （编译进内核、没有独立插件目录与 plugin.json，如 remotedevice）无法靠
// 扫目录发现自己的服务；而它们恰恰最需要被反代出去（设备网关就是内置的）。
// 两种来源互补：
//   - 外部插件 → plugin.json 的 proxies（静态、未启动也可见）
//   - 内置插件 → DeclareProxy（运行期，随 Start 注册）
type ProxyDeclarer func(decl ProxyDecl)

// SetProxyDeclarer 由内核注入收集回调。插件不直接调它。
func (s *PluginSDK) SetProxyDeclarer(d ProxyDeclarer) {
	if s == nil {
		return
	}
	s.apiMu.Lock()
	s.proxyDecl = d
	s.apiMu.Unlock()
}

// DeclareProxy 声明本插件的一个服务需要 HomeAgent 反代出去。
//
// 用法（通常在 Start 里调用）：
//
//	s.DeclareProxy(sdk.ProxyDecl{
//	    Name: "ui", Host: "myapp", Target: "127.0.0.1:12100",
//	})
//
// 声明立即生效（反代表会在下一次请求时重建）。声明**不做去重**：同一 Host
// 被两条声明占用时由反代层判定冲突并明确报错，而不是这里静默吞掉——
// 插件作者需要看见冲突。
func (s *PluginSDK) DeclareProxy(decl ProxyDecl) {
	if s == nil {
		return
	}
	s.apiMu.Lock()
	d := s.proxyDecl
	s.apiMu.Unlock()
	if d != nil {
		d(decl)
	}
}
