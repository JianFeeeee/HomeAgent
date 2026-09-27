package pluginmgr

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// platformBinary 按当前 OS/ARCH 选择正确的插件二进制文件名。
// 返回 (zip内文件名, 安装后重命名).
//
// 子进程模式下各平台产物统一叫 plugin.bin（进程边界即 ABI 边界，
// 不存在 .so/.dylib/.dll 的区分），故 zip 内按平台加后缀区分，
// 解包时挑当前平台那一份重命名为 plugin.bin。
func platformBinary() (zipName, canonicalName string) {
	switch runtime.GOOS {
	case "linux", "darwin", "windows", "freebsd":
		return fmt.Sprintf("plugin.bin.%s.%s", runtime.GOOS, runtime.GOARCH), "plugin.bin"
	default:
		return "", ""
	}
}

// validBinaries 是 .hmap 中所有可识别的文件入口（平台二进制或脚本）。
var validBinaries = map[string]bool{
	"plugin.bin": true,
	"main.lua":   true,
	"SKILL.md":   true,
}

// isPlatformBinary 判断 zip 条目是否为平台特定二进制（bundle 模式下仅当前平台的被解压）。
//
// 形式：plugin.bin.<goos>.<goarch>。不用固定表是因为平台组合会增长
// （linux/arm64、darwin/arm64 等），按前缀判断无需维护清单。
func isPlatformBinary(name string) bool {
	return strings.HasPrefix(name, "plugin.bin.")
}

var downloadClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return fmt.Errorf("redirect to disallowed scheme: %s", req.URL.Scheme)
		}
		return nil
	},
}

// defaultHTTPAddr 是 HTTP API 的**内置默认**监听地址。
//
// ★ 曾经这里是一个**包级可变全局** `var HTTPAddr`，且 Start() 会把 settings 读到的值
// **反写**回该全局。两个真实后果：
//  1. 多实例互相污染——测试并行起两个 Registry，后启动的实例会把地址写进全局，
//     先启动那个的 startHTTPServer 读到的是别人的地址（实测与生产 homed 抢 9876）；
//  2. 全局读写在并发下没有同步，属数据竞态。
//
// 现在改为实例字段 p.httpAddr（默认值走本常量），不再有可被任意代码改写的包级状态。
const defaultHTTPAddr = "127.0.0.1:9876"

func init() {
	plugin.RegisterPluginMeta("pluginmgr", "插件管理", "Plugin Manager")
	plugin.RegisterFactory("pluginmgr", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

type Plugin struct {
	name      string
	mu        sync.Mutex
	server    *http.Server
	mux       *http.ServeMux
	listen    net.Listener
	httpURL   string
	httpAddr  string // 本实例的监听地址（默认 defaultHTTPAddr；来自 settings）
	sdk       *sdk.PluginSDK
	pluginDir string
}

func New(name string) *Plugin {
	return &Plugin{name: name, mux: http.NewServeMux(), httpAddr: defaultHTTPAddr}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key:         "http_addr",
		Default:     defaultHTTPAddr,
		Type:        "string",
		DisplayName: "HTTP 监听地址",
		Description: "插件管理 API 的监听地址，设为空可禁用 HTTP 服务；" +
			"填 127.0.0.1:0 让系统分配空闲端口（测试/多实例推荐）",
		Category: "pluginmgr",
	})

	// 只写本实例字段，**不写任何包级状态**（见 defaultHTTPAddr 注释）。
	p.httpAddr = defaultHTTPAddr
	if v, _ := s.Settings().Get("http_addr"); v != nil {
		if addr, ok := v.(string); ok {
			p.httpAddr = addr // 允许空串 = 显式禁用 HTTP 服务
		}
	}

	if v, _ := s.Settings().GetCore("plugin.dir"); v != nil {
		if dir, ok := v.(string); ok && dir != "" {
			p.pluginDir = dir
		}
	}

	p.registerTools(s)

	if p.httpAddr != "" {
		p.startHTTPServer()
	}

	return nil
}

func (p *Plugin) Stop() error {
	if p.server != nil {
		return p.server.Close()
	}
	return nil
}

// ======== Tools ========

func (p *Plugin) registerTools(s *sdk.PluginSDK) {
	s.RegisterTool("plugin_install", sdk.ToolDef{
		Name:        "plugin_install",
		Description: "安装 HomeAgent 插件包（.hmap）。两种来源：url（http/https 下载）或 path（本机路径，配合 plugindev_build 的产物用这个）。插件已存在时传 overwrite=true 原地更新（升级/降级/重装，保留配置表，无需卸载重装）。更新后需调用 plgreload 或重启生效。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"url": map[string]interface{}{
					"type":        "string",
					"description": "插件包的下载 URL（http/https）",
				},
				"path": map[string]interface{}{
					"type":        "string",
					"description": "插件包在**本机**的路径（.hmap）。与 url 二选一；同时给出时以 path 为准",
				},
				"overwrite": map[string]interface{}{
					"type":        "boolean",
					"description": "已存在时原地更新（保留配置）。默认 false",
				},
			},
		},
		// 装插件：改磁盘与运行态，可能半安装
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		overwrite, _ := args["overwrite"].(bool)
		// path 优先：它对应"agent 自己构建出产物再装"的场景（plugindev_build → plugin_install）。
		if path, _ := args["path"].(string); strings.TrimSpace(path) != "" {
			pth := strings.TrimSpace(path)
			if st, err := os.Stat(pth); err != nil || st.IsDir() {
				return map[string]interface{}{"error": fmt.Sprintf("path 无效（必须是存在的 .hmap 文件）: %s", pth)}, nil
			}
			return p.installFromPath(pth, overwrite)
		}
		url, _ := args["url"].(string)
		if strings.TrimSpace(url) == "" {
			return map[string]interface{}{"error": "需要 url 或 path（二选一）"}, nil
		}
		return p.installFromURL(strings.TrimSpace(url), overwrite)
	})

	s.RegisterTool("plugin_list", sdk.ToolDef{
		Name:        "plugin_list",
		Description: "列出已安装的所有外部插件及其版本。同时返回运行状态（loaded/alive/pid/崩溃次数），子进程插件死了在此体现为 alive=false。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		// 已核实只读：只列举已装插件
		ParallelSafe: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.listPlugins()
	})

	s.RegisterTool("plugin_status", sdk.ToolDef{
		Name: "plugin_status",
		Description: "查看插件运行状态：进程是否存活、PID、加载通道、最近崩溃次数、当前注册的工具。" +
			"不传 name 则返回全部插件概览。工具调不通时先用它确认插件是否还活着。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "插件名称；缺省返回全部",
				},
			},
		},
		// 已核实只读：查运行状态
		ParallelSafe: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		name, _ := args["name"].(string)
		return p.pluginStatus(name)
	})

	s.RegisterTool("plugin_restart", sdk.ToolDef{
		Name: "plugin_restart",
		Description: "重启单个插件（停止后重新加载，保留配置）。适用于：插件进程已死但自动重启被用尽" +
			"（plugin_status 的 crash_count 达上限），或换了 plugin.bin 需立即生效。不需重启 homed。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "插件名称",
				},
			},
			"required": []string{"name"},
		},
		// 重启插件：改运行态
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		name, _ := args["name"].(string)
		if name == "" {
			return map[string]interface{}{"error": "name is required"}, nil
		}
		return p.restartPlugin(name)
	})

	s.RegisterTool("plugin_remove", sdk.ToolDef{
		Name:        "plugin_remove",
		Description: "卸载一个已安装的外部插件",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "插件名称",
				},
			},
			"required": []string{"name"},
		},
		// 卸插件：改磁盘与运行态
		Serial: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		name, _ := args["name"].(string)
		if name == "" {
			return map[string]interface{}{"error": "name is required"}, nil
		}
		return p.removePlugin(name)
	})

	s.RegisterTool("plugin_info", sdk.ToolDef{
		Name:        "plugin_info",
		Description: "查看指定已安装插件的详细信息（版本、作者、描述等）",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "插件名称",
				},
			},
			"required": []string{"name"},
		},
		// 已核实只读：查单个插件详情
		ParallelSafe: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		name, _ := args["name"].(string)
		if name == "" {
			return map[string]interface{}{"error": "name is required"}, nil
		}
		return p.pluginInfo(name)
	})
}

// ======== HTTP API ========

func (p *Plugin) startHTTPServer() {
	p.mux.HandleFunc("/plugins", p.handlePlugins)
	p.mux.HandleFunc("/plugins/", p.handlePluginByID)

	listen, err := net.Listen("tcp", p.httpAddr)
	if err != nil {
		log.Printf("[pluginmgr] HTTP listen: %v", err)
		return
	}
	// 用**实际绑定**的地址而非配置值：配 :0 时只有 net.Listener 知道真实端口。
	// 这也让 httpURL 在多实例/测试下始终指向本实例真正监听的端点。
	url := "http://" + listen.Addr().String()
	p.mu.Lock()
	p.listen = listen
	p.httpURL = url
	p.mu.Unlock()

	p.server = &http.Server{Handler: p.mux}
	go func() {
		log.Printf("[pluginmgr] HTTP API on %s", url)
		if err := p.server.Serve(listen); err != nil && err != http.ErrServerClosed {
			log.Printf("[pluginmgr] HTTP serve: %v", err)
		}
	}()
}

// HTTPURL 返回本实例实际监听的基地址（形如 http://127.0.0.1:9876）；
// 未启动或禁用时返回空串。供诊断与需要知道“到底在哪个端口”的调用方使用。
func (p *Plugin) HTTPURL() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.httpURL
}

func (p *Plugin) handlePlugins(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := p.listPlugins()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, list)

	case http.MethodPost:
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "application/json") {
			var body struct {
				URL       string `json:"url"`
				Path      string `json:"path"`
				Overwrite bool   `json:"overwrite"` // 已存在时原地更新（保留配置）
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid json", http.StatusBadRequest)
				return
			}
			switch {
			case body.URL != "":
				result, err := p.installFromURL(body.URL, body.Overwrite)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
					return
				}
				writeJSON(w, http.StatusOK, result)
			case body.Path != "":
				result, err := p.installFromPath(body.Path, body.Overwrite)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
					return
				}
				writeJSON(w, http.StatusOK, result)
			default:
				http.Error(w, "url or path is required", http.StatusBadRequest)
				return
			}
		} else {
			data, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
				return
			}
			result, err := p.installFromData(data, false)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, result)
		}

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (p *Plugin) handlePluginByID(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/plugins/")
	name = strings.TrimSuffix(name, "/")
	if name == "" {
		http.Error(w, "plugin name required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		info, err := p.pluginInfo(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, info)

	case http.MethodDelete:
		result, err := p.removePlugin(name)
		if err != nil {
			// 内置插件禁卸 → 409 Conflict；不存在 → 404；其余删除失败 → 500
			msg := err.Error()
			switch {
			case strings.Contains(msg, "built-in plugin"):
				writeJSON(w, http.StatusConflict, map[string]interface{}{"error": msg, "name": name, "plugin_type": "builtin"})
			case strings.Contains(msg, "not found"):
				writeJSON(w, http.StatusNotFound, map[string]interface{}{"error": msg, "name": name})
			default:
				writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": msg, "name": name})
			}
			return
		}
		writeJSON(w, http.StatusOK, result)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ======== Core Logic ========

func (p *Plugin) installFromPath(path string, overwrite bool) (interface{}, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	return p.installFromData(data, overwrite)
}

func (p *Plugin) installFromURL(rawURL string, overwrite bool) (interface{}, error) {
	log.Printf("[pluginmgr] downloading: %s", rawURL)

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme: %s (only http/https allowed)", parsed.Scheme)
	}

	resp, err := downloadClient.Get(rawURL)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned %s", resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	result, err := p.installFromData(data, overwrite)
	if err != nil {
		return nil, err
	}

	if m, ok := result.(map[string]interface{}); ok {
		m["source"] = "url"
		result = m
	}
	return result, nil
}

// installFromData 安装（或 overwrite=true 时原地更新）插件包。
// 更新语义：StopAndUnload 停止旧实例但保留配置表，备份旧目录→解压新包→失败回滚；
// 更新后配置原样生效，无需用户手动卸载重装。
func (p *Plugin) installFromData(data []byte, overwrite bool) (interface{}, error) {
	pkg, err := validatePackage(data)
	if err != nil {
		return map[string]interface{}{
			"error":   "invalid package",
			"details": err.Error(),
		}, nil
	}

	dir := p.pluginDir
	if dir == "" {
		return map[string]interface{}{"error": "plugin dir not configured"}, nil
	}

	target := filepath.Join(dir, pkg.Name)
	var oldVersion string
	existing := false
	if m, err := plugin.ReadManifest(target); err == nil && m != nil {
		existing = true
		oldVersion = m.Version
	} else if _, statErr := os.Stat(target); statErr == nil {
		existing = true // 目录存在但 manifest 不可读：视为已安装、版本未知
	}

	if existing && !overwrite {
		return map[string]interface{}{
			"error":   "plugin already exists",
			"name":    pkg.Name,
			"version": pkg.Version,
			"current": oldVersion,
			"action":  "remove_first",
			"hint":    `传 "overwrite": true 可原地更新（保留配置）`,
		}, nil
	}

	if existing && overwrite {
		// 原地更新：停旧实例（保留配置表），备份旧目录，解压新包，失败回滚。
		if p.sdk != nil && p.sdk.PluginMgr() != nil {
			if err := p.sdk.PluginMgr().StopAndUnload(pkg.Name); err != nil {
				log.Printf("[pluginmgr] StopAndUnload %s: %v", pkg.Name, err)
			}
		}
		backup := target + ".bak"
		os.RemoveAll(backup)
		if err := os.Rename(target, backup); err != nil {
			return map[string]interface{}{
				"error":   "backup old plugin dir failed",
				"details": err.Error(),
			}, nil
		}
		if err := extractPackage(data, dir); err != nil {
			// 回滚：恢复旧目录并重新加载旧版
			os.RemoveAll(target)
			if rbErr := os.Rename(backup, target); rbErr != nil {
				return map[string]interface{}{
					"error":    "extract failed AND rollback failed",
					"details":  err.Error(),
					"rollback": rbErr.Error(),
				}, nil
			}
			if p.sdk != nil && p.sdk.PluginMgr() != nil {
				_ = p.sdk.PluginMgr().ReloadOne(pkg.Name)
			}
			return map[string]interface{}{
				"error":   "extract failed (rolled back to " + oldVersion + ")",
				"details": err.Error(),
			}, nil
		}
		os.RemoveAll(backup)

		checksum := fmt.Sprintf("%x", sha256.Sum256(data))
		action := "upgraded"
		if cmpVersion(pkg.Version, oldVersion) < 0 {
			action = "downgraded"
		} else if cmpVersion(pkg.Version, oldVersion) == 0 {
			action = "reinstalled"
		}
		return map[string]interface{}{
			"status":           "installed",
			"name":             pkg.Name,
			"version":          pkg.Version,
			"previous_version": oldVersion,
			"entry":            pkg.Entry,
			"checksum":         checksum,
			"action":           action,
			"reload_required":  true,
			"config_kept":      true,
		}, nil
	}

	if err := extractPackage(data, dir); err != nil {
		return map[string]interface{}{
			"error":   "extract failed",
			"details": err.Error(),
		}, nil
	}

	checksum := fmt.Sprintf("%x", sha256.Sum256(data))

	return map[string]interface{}{
		"status":   "installed",
		"name":     pkg.Name,
		"version":  pkg.Version,
		"entry":    pkg.Entry,
		"checksum": checksum,
		"action":   "reload_required",
	}, nil
}

// cmpVersion 比较点分版本号：a<b 返回 -1，a>b 返回 1，相等返回 0。
// 非数字段按字符串比较；长度不齐缺段视作 0。
func cmpVersion(a, b string) int {
	parse := func(s string) []int {
		parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".", 4)
		out := make([]int, 0, len(parts))
		for _, p := range parts {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				n = 0
			}
			out = append(out, n)
		}
		for len(out) < 3 {
			out = append(out, 0)
		}
		return out
	}
	a1, b1 := parse(a), parse(b)
	for i := range a1 {
		if a1[i] < b1[i] {
			return -1
		}
		if a1[i] > b1[i] {
			return 1
		}
	}
	return 0
}

func (p *Plugin) listPlugins() (interface{}, error) {
	dir := p.pluginDir
	if dir == "" {
		return []map[string]interface{}{}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []map[string]interface{}{}, nil
		}
		return nil, err
	}

	// 运行期状态一次取齐，避免逐个插件回内核查。
	//
	// 为何要带运行期：只读 plugin.json 的旧实现无法区分「已安装」与「正在跑」。
	// 生产上 editdoc 子进程被 kill 后，plugin_list 依旧把它列为正常插件，
	// 模型与 WebUI 都看不出异常，只能在调工具时吃一个“进程已退出”。
	runtimes := map[string]sdk.PluginRuntimeInfo{}
	if mgr := p.pluginMgr(); mgr != nil {
		for _, rt := range mgr.ListPluginRuntimes() {
			runtimes[rt.Name] = rt
		}
	}

	var plugins []map[string]interface{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		m, err := plugin.ReadManifest(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		item := map[string]interface{}{
			"name":        m.Name,
			"version":     m.Version,
			"description": m.Description,
			"author":      m.Author,
			"entry":       m.Entry,
			"deprecated":  m.Deprecated,
		}
		if rt, ok := runtimes[m.Name]; ok {
			item["loaded"] = rt.Loaded
			item["alive"] = rt.Alive
			item["disabled"] = rt.Disabled
			item["channel"] = rt.Channel
			if rt.PID > 0 {
				item["pid"] = rt.PID
			}
			if rt.CrashCount > 0 {
				item["crash_count"] = rt.CrashCount
			}
		}
		plugins = append(plugins, item)
	}
	if plugins == nil {
		plugins = []map[string]interface{}{}
	}
	return plugins, nil
}

// pluginMgr 取内核插件管理面（可能为 nil：单测/未注入）。
func (p *Plugin) pluginMgr() sdk.PluginManager {
	if p.sdk == nil {
		return nil
	}
	return p.sdk.PluginMgr()
}

// pluginStatus 返回插件运行期状态（进程存活/PID/崩溃计数/工具清单）。
//
// 这是子进程化后插件管理器必须补上的一块：以前插件与内核同进程，
// “加载了”就等于“能用”；现在插件是独立进程，两者不再等价。
func (p *Plugin) pluginStatus(name string) (interface{}, error) {
	mgr := p.pluginMgr()
	if mgr == nil {
		return nil, fmt.Errorf("内核插件管理面不可用")
	}
	if name != "" {
		info, ok := mgr.PluginRuntime(name)
		if !ok {
			return nil, fmt.Errorf("plugin %q not found", name)
		}
		return info, nil
	}

	all := mgr.ListPluginRuntimes()
	// 汇总一行：让模型不用自己数就能看出“有东西挂了”。
	var loaded, dead int
	var unhealthy []string
	for _, rt := range all {
		if rt.Loaded {
			loaded++
		}
		if rt.Loaded && !rt.Alive {
			dead++
			unhealthy = append(unhealthy, rt.Name)
			continue
		}
		if rt.CrashCount > 0 {
			unhealthy = append(unhealthy, fmt.Sprintf("%s(崩溃%d次)", rt.Name, rt.CrashCount))
		}
	}
	sort.Strings(unhealthy)
	return map[string]interface{}{
		"total":     len(all),
		"loaded":    loaded,
		"dead":      dead,
		"unhealthy": unhealthy,
		"plugins":   all,
	}, nil
}

// restartPlugin 重启单个插件（保留配置）。
//
// 与 plgreload 的区别：后者按入口文件 hash 增量重载，二进制没改就不动；
// 而进程被 kill 时二进制正是没改的，所以一定要有一个无条件重启的入口。
func (p *Plugin) restartPlugin(name string) (interface{}, error) {
	mgr := p.pluginMgr()
	if mgr == nil {
		return nil, fmt.Errorf("内核插件管理面不可用")
	}
	if _, ok := mgr.PluginRuntime(name); !ok {
		return nil, fmt.Errorf("plugin %q not found", name)
	}
	if mgr.IsPluginDisabled(name) {
		return nil, fmt.Errorf("plugin %s 已被禁用，请先启用再重启", name)
	}
	if err := mgr.ReloadOne(name); err != nil {
		return nil, fmt.Errorf("restart %s: %w", name, err)
	}
	info, _ := mgr.PluginRuntime(name)
	return map[string]interface{}{
		"status":  "restarted",
		"name":    name,
		"runtime": info,
	}, nil
}

func (p *Plugin) removePlugin(name string) (interface{}, error) {
	// 内置插件只能禁用不能卸载：目录下无产物，且从注册表删除会破坏内核依赖。
	if p.sdk != nil && p.sdk.PluginMgr() != nil && p.sdk.PluginMgr().IsBuiltinPlugin(name) {
		return nil, fmt.Errorf("plugin %s is a built-in plugin and cannot be unloaded", name)
	}

	dir := filepath.Join(p.pluginDir, name)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil, fmt.Errorf("plugin %s not found", name)
	}

	// 先经内核卸载：停止插件（stop handlers + Stop）并执行插件注册的 onRemove 回调
	if p.sdk != nil && p.sdk.PluginMgr() != nil {
		if err := p.sdk.PluginMgr().RemovePlugin(name); err != nil {
			log.Printf("[pluginmgr] RemovePlugin %s: %v", name, err)
		}
	}

	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("remove plugin dir: %w", err)
	}

	// 同步清理禁用表
	if p.sdk != nil && p.sdk.PluginMgr() != nil {
		if err := p.sdk.PluginMgr().EnablePlugin(name); err != nil {
			log.Printf("[pluginmgr] enable %s after remove: %v", name, err)
		}
	}

	// 卸载已即时生效（停止+注册表移除+目录删除），无需 reload
	return map[string]interface{}{
		"status":          "removed",
		"name":            name,
		"reload_required": false,
	}, nil
}

func (p *Plugin) pluginInfo(name string) (interface{}, error) {
	dir := filepath.Join(p.pluginDir, name)
	m, err := plugin.ReadManifest(dir)
	if err != nil {
		return nil, fmt.Errorf("plugin %q not found", name)
	}
	info := map[string]interface{}{
		"name":        m.Name,
		"version":     m.Version,
		"description": m.Description,
		"author":      m.Author,
		"license":     m.License,
		"homepage":    m.Homepage,
		"repository":  m.Repository,
		"entry":       m.Entry,
		"min_version": m.MinVersion,
		"tags":        m.Tags,
		"deprecated":  m.Deprecated,
	}

	entries, _ := os.ReadDir(dir)
	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	info["files"] = files

	return info, nil
}

// ======== Package Validation ========

type pluginPackage struct {
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	Entry     string   `json:"entry"`
	Platforms []string `json:"platforms,omitempty"`
}

func validatePackage(data []byte) (*pluginPackage, error) {
	if len(data) > 100<<20 {
		return nil, fmt.Errorf("package too large (>100MB)")
	}

	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("invalid zip: %w", err)
	}

	var pkg pluginPackage
	hasManifest := false

	for _, f := range reader.File {
		if strings.Contains(f.Name, "..") || strings.HasPrefix(f.Name, "/") {
			return nil, fmt.Errorf("invalid path: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			continue
		}
		if f.Name != "plugin.json" {
			continue
		}
		hasManifest = true
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("read manifest: %w", err)
		}
		mData, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read manifest: %w", err)
		}
		if err := json.Unmarshal(mData, &pkg); err != nil {
			return nil, fmt.Errorf("parse manifest: %w", err)
		}
		break
	}

	if !hasManifest {
		return nil, fmt.Errorf("missing plugin.json")
	}
	if pkg.Name == "" {
		return nil, fmt.Errorf("manifest: name required")
	}
	if pkg.Version == "" {
		return nil, fmt.Errorf("manifest: version required")
	}
	if pkg.Entry == "" {
		return nil, fmt.Errorf("manifest: entry required")
	}

	// verify at least one valid binary exists for any platform
	hasBinary := false
	zipEntries := map[string]bool{}
	for _, f := range reader.File {
		if !f.FileInfo().IsDir() {
			zipEntries[f.Name] = true
		}
	}

	if len(pkg.Platforms) > 0 {
		// bundle mode：每个声明的平台都要有对应二进制。
		// 子进程模式下条目形式为 plugin.bin.<goos>.<goarch>，
		// 故按前缀匹配而不枚举架构（同一 OS 可能有 amd64/arm64 两份）。
		for _, plat := range pkg.Platforms {
			prefix := "plugin.bin." + plat + "."
			found := false
			for name := range zipEntries {
				if strings.HasPrefix(name, prefix) {
					found = true
					break
				}
			}
			if found {
				hasBinary = true
			}
		}
	} else {
		// legacy mode: check entry exists and is a known binary
		if zipEntries[pkg.Entry] && validBinaries[pkg.Entry] {
			hasBinary = true
		}
	}

	if !hasBinary {
		return nil, fmt.Errorf("no valid binary found in package (entry=%q, platforms=%v)", pkg.Entry, pkg.Platforms)
	}

	return &pkg, nil
}

// copyZipEntry 解压 zip 中的单个文件到目标路径。
func copyZipEntry(f *zip.File, dest string) error {
	os.MkdirAll(filepath.Dir(dest), 0755)
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("open %s: %w", f.Name, err)
	}
	defer rc.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	defer out.Close()
	_, err = io.Copy(out, rc)
	return err
}

func extractPackage(data []byte, pluginDir string) error {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}

	// 先读 manifest 确定插件名和平台信息
	var pkgName string
	var declaredPlatforms []string
	for _, f := range reader.File {
		if f.Name == "plugin.json" && !f.FileInfo().IsDir() {
			rc, _ := f.Open()
			mData, _ := io.ReadAll(rc)
			rc.Close()
			var m struct {
				Name      string   `json:"name"`
				Platforms []string `json:"platforms"`
			}
			json.Unmarshal(mData, &m)
			pkgName = m.Name
			declaredPlatforms = m.Platforms
			break
		}
	}
	if pkgName == "" {
		return fmt.Errorf("cannot determine plugin name")
	}

	target := filepath.Join(pluginDir, pkgName)
	os.MkdirAll(target, 0755)

	zipBin, canonicalName := platformBinary()
	isBundle := declaredPlatforms != nil

	for _, f := range reader.File {
		fpath := filepath.Join(target, f.Name)
		if !strings.HasPrefix(filepath.Clean(fpath), filepath.Clean(target)+string(os.PathSeparator)) {
			return fmt.Errorf("path traversal: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, 0755)
			continue
		}

		// bundle mode: skip other platforms' platform-specific binaries
		if isBundle && isPlatformBinary(f.Name) && f.Name != zipBin {
			continue
		}

		// 平台二进制重命名为规范名（plugin.bin.linux.amd64 → plugin.bin）
		dest := fpath
		if isBundle && f.Name == zipBin && canonicalName != zipBin {
			dest = filepath.Join(target, canonicalName)
		}

		if err := copyZipEntry(f, dest); err != nil {
			return err
		}

		// 子进程插件必须可执行。
		// zip 保留了原文件权限位，但经某些工具链/传输后可能丢失；
		// 内核加载时会因缺执行位报错（带 chmod +x 提示），在此提前补上。
		if filepath.Base(dest) == "plugin.bin" {
			if err := os.Chmod(dest, 0o755); err != nil {
				return fmt.Errorf("chmod %s: %w", dest, err)
			}
		}
	}

	return nil
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
