package sdk

import (
	"strings"
	"sync"

	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

// 反代声明在 **公开 SDK** 里定义（`pubsdk`），内核侧只是别名转发。
//
// 为什么放公开 SDK 而不是内核实现在：声明是**插件作者直接书写的契约**
// （plugin.json 的 proxies 字段），必须与 SDK 文档、hmapdev 工具链用同一套
// 定义与校验，否则插件作者本地通过、内核拒绝，或反之。
//
// 这与 ConfigDef 等信息完全同构——公开面定义契约，内核面实现行为。

// ProxyDecl 是一条反代声明（见 pubsdk.ProxyDecl 的完整文档）。
type ProxyDecl = pubsdk.ProxyDecl

// 生效的鉴权模式取值。
const (
	// ProxyAuthHomeAgent：由 HomeAgent 统一保护（门户会话或 X-API-Key）。
	ProxyAuthHomeAgent = pubsdk.ProxyAuthHomeAgent
	// ProxyAuthNone：不经 HomeAgent 鉴权，信任上游自身鉴权。
	ProxyAuthNone = pubsdk.ProxyAuthNone
)

// ValidProxyAuth 校验鉴权模式取值（空串合法，等价 ProxyAuthHomeAgent）。
func ValidProxyAuth(auth string) bool { return pubsdk.ValidProxyAuth(auth) }

// EffectiveProxyAuth 返回生效的鉴权模式（空串归一化为 ProxyAuthHomeAgent）。
func EffectiveProxyAuth(auth string) string { return pubsdk.EffectiveProxyAuth(auth) }

// ValidProxyHostLabel 校验子域标签是否合法（DNS label 规则）。
func ValidProxyHostLabel(label string) bool { return pubsdk.ValidProxyHostLabel(label) }

// NormalizeProxyHost 由插件名派生默认的子域标签。
func NormalizeProxyHost(pluginName string) string { return pubsdk.NormalizeProxyHost(pluginName) }

// ValidateProxyDecl 校验一条声明，返回人类可读的错误（合法时为空）。
func ValidateProxyDecl(d ProxyDecl) string { return pubsdk.ValidateProxyDecl(d) }

// ---- 内置插件反代声明的运行期登记表 ----

// 为什么需要它：外部插件的声明在 plugin.json 里，可以扫目录发现；但**内置**
// 插件编译进内核、没有插件目录，靠扫盘永远发现不了自己的服务——而设备网关
// （remotedevice）正是内置的，且最需要被反代出去。两种来源互补。
var (
	builtinProxyMu    sync.RWMutex
	builtinProxyDecls = map[string][]ProxyDecl{}
	builtinProxyVer   int64
)

// DeclareBuiltinProxy 登记一个内置插件的服务声明（由 DeclareProxy 转发）。
func DeclareBuiltinProxy(plugin string, d ProxyDecl) {
	if plugin == "" || strings.TrimSpace(d.Target) == "" {
		return
	}
	builtinProxyMu.Lock()
	defer builtinProxyMu.Unlock()
	// 同一插件同一声明名重复登记（如自动重启后再次 Start）视为刷新，不重复累积。
	name := d.Name
	if name == "" {
		name = "service"
		d.Name = name
	}
	list := builtinProxyDecls[plugin]
	for i := range list {
		if list[i].Name == name {
			list[i] = d
			builtinProxyVer++
			return
		}
	}
	builtinProxyDecls[plugin] = append(list, d)
	builtinProxyVer++
}

// ClearBuiltinProxyDecls 清除某插件的声明（插件停止/卸载时调用）。
func ClearBuiltinProxyDecls(plugin string) {
	builtinProxyMu.Lock()
	defer builtinProxyMu.Unlock()
	if _, ok := builtinProxyDecls[plugin]; ok {
		delete(builtinProxyDecls, plugin)
		builtinProxyVer++
	}
}

// BuiltinProxyDecls 返回内置插件声明的快照（plugin → decls）。
func BuiltinProxyDecls() map[string][]ProxyDecl {
	builtinProxyMu.RLock()
	defer builtinProxyMu.RUnlock()
	out := make(map[string][]ProxyDecl, len(builtinProxyDecls))
	for k, v := range builtinProxyDecls {
		cp := make([]ProxyDecl, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// BuiltinProxyVersion 是声明表的版本号。调用方（webui 反代层）据它判断
// 缓存的路由表是否过期——比每次请求重新聚合一遍便宜得多。
func BuiltinProxyVersion() int64 {
	builtinProxyMu.RLock()
	defer builtinProxyMu.RUnlock()
	return builtinProxyVer
}
