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
	"strings"
	"sync"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// platformBinary 按当前 OS 选择正确的插件二进制文件名。
// 返回 (zip内文件名, 安装后重命名).
func platformBinary() (zipName, canonicalName string) {
	switch runtime.GOOS {
	case "linux":
		return "plugin.so", "plugin.so"
	case "darwin":
		return "plugin.dylib", "plugin.so" // dlopen 兼容 .so 名称
	case "windows":
		return "plugin.dll", "plugin.dll"
	default:
		return "", ""
	}
}

// validBinaries 是 .hmap 中所有可识别的文件入口（平台二进制或脚本）。
var validBinaries = map[string]bool{
	"plugin.so":   true,
	"plugin.dylib": true,
	"plugin.dll":  true,
	"main.lua":    true,
	"SKILL.md":    true,
}

// platformBinaries 是平台特定的二进制，bundle 模式下仅当前平台的被解压。
var platformBinaries = map[string]bool{
	"plugin.so":   true,
	"plugin.dylib": true,
	"plugin.dll":  true,
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

var (
	PluginDir string          // 由 main.go 设置
	Reg       *plugin.Registry // 由 main.go 设置
	HTTPAddr  = "127.0.0.1:9876" // 监听地址，可被 main.go 覆写或 settings 配置
)

func init() {
	plugin.RegisterPluginMeta("pluginmgr", "插件管理", "Plugin Manager")
	plugin.RegisterFactory("pluginmgr", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

type Plugin struct {
	name    string
	mu      sync.Mutex
	server  *http.Server
	mux     *http.ServeMux
	listen  net.Listener
	httpURL string
}

func New(name string) *Plugin {
	return &Plugin{name: name, mux: http.NewServeMux()}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key:         "http_addr",
		Default:     HTTPAddr,
		Type:        "string",
		DisplayName: "HTTP 监听地址",
		Description: "插件管理 API 的监听地址，设为空可禁用 HTTP 服务",
		Category:    "pluginmgr",
	})

	if v, _ := s.Settings().Get("http_addr"); v != nil {
		if addr, ok := v.(string); ok && addr != "" {
			HTTPAddr = addr
		}
	}

	p.registerTools(s)

	if HTTPAddr != "" {
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
		Description: "从 URL 安装 HomeAgent 插件包（.hmap 文件）。安装后需调用 plgreload 或重启生效。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"url": map[string]interface{}{
					"type":        "string",
					"description": "插件包的下载 URL",
				},
			},
			"required": []string{"url"},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		url, _ := args["url"].(string)
		if url == "" {
			return map[string]interface{}{"error": "url is required"}, nil
		}
		return p.installFromURL(url)
	})

	s.RegisterTool("plugin_list", sdk.ToolDef{
		Name:        "plugin_list",
		Description: "列出已安装的所有外部插件及其版本",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.listPlugins()
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

	listen, err := net.Listen("tcp", HTTPAddr)
	if err != nil {
		log.Printf("[pluginmgr] HTTP listen: %v", err)
		return
	}
	p.listen = listen
	p.httpURL = "http://" + listen.Addr().String()

	p.server = &http.Server{Handler: p.mux}
	go func() {
		log.Printf("[pluginmgr] HTTP API on %s", p.httpURL)
		if err := p.server.Serve(listen); err != nil && err != http.ErrServerClosed {
			log.Printf("[pluginmgr] HTTP serve: %v", err)
		}
	}()
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
				URL  string `json:"url"`
				Path string `json:"path"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid json", http.StatusBadRequest)
				return
			}
			switch {
			case body.URL != "":
				result, err := p.installFromURL(body.URL)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
					return
				}
				writeJSON(w, http.StatusOK, result)
			case body.Path != "":
				result, err := p.installFromPath(body.Path)
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
			result, err := p.installFromData(data)
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
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ======== Core Logic ========

func (p *Plugin) installFromPath(path string) (interface{}, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	return p.installFromData(data)
}

func (p *Plugin) installFromURL(rawURL string) (interface{}, error) {
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

	result, err := p.installFromData(data)
	if err != nil {
		return nil, err
	}

	if m, ok := result.(map[string]interface{}); ok {
		m["source"] = "url"
		result = m
	}
	return result, nil
}

func (p *Plugin) installFromData(data []byte) (interface{}, error) {
	pkg, err := validatePackage(data)
	if err != nil {
		return map[string]interface{}{
			"error":   "invalid package",
			"details": err.Error(),
		}, nil
	}

	dir := PluginDir
	if dir == "" {
		return map[string]interface{}{"error": "plugin dir not configured"}, nil
	}

	target := filepath.Join(dir, pkg.Name)
	if _, err := os.Stat(target); err == nil {
		return map[string]interface{}{
			"error":   "plugin already exists",
			"name":    pkg.Name,
			"version": pkg.Version,
			"action":  "remove_first",
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

func (p *Plugin) listPlugins() (interface{}, error) {
	dir := PluginDir
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

	var plugins []map[string]interface{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		m, err := plugin.ReadManifest(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		plugins = append(plugins, map[string]interface{}{
			"name":        m.Name,
			"version":     m.Version,
			"description": m.Description,
			"author":      m.Author,
			"entry":       m.Entry,
			"deprecated":  m.Deprecated,
		})
	}
	if plugins == nil {
		plugins = []map[string]interface{}{}
	}
	return plugins, nil
}

func (p *Plugin) removePlugin(name string) (interface{}, error) {
	dir := filepath.Join(PluginDir, name)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return map[string]interface{}{"error": "plugin not found", "name": name}, nil
	}

	if err := os.RemoveAll(dir); err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}

	return map[string]interface{}{
		"status": "removed",
		"name":   name,
		"action": "reload_required",
	}, nil
}

func (p *Plugin) pluginInfo(name string) (interface{}, error) {
	dir := filepath.Join(PluginDir, name)
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
		// bundle mode: check each declared platform has a matching binary
		for _, plat := range pkg.Platforms {
			bin, ok := map[string]string{
				"linux":   "plugin.so",
				"darwin":  "plugin.dylib",
				"windows": "plugin.dll",
			}[plat]
			if !ok {
				return nil, fmt.Errorf("unsupported platform: %q", plat)
			}
			if zipEntries[bin] {
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
		if isBundle && platformBinaries[f.Name] && f.Name != zipBin {
			continue
		}

		// rename platform binary to canonical name (e.g. plugin.dylib → plugin.so)
		dest := fpath
		if isBundle && f.Name == zipBin && canonicalName != zipBin {
			dest = filepath.Join(target, canonicalName)
		}

		if err := copyZipEntry(f, dest); err != nil {
			return err
		}
	}

	return nil
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
