package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	sdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"

	"github.com/chromedp/chromedp"
)

type Plugin struct {
	name    string
	sdk     *sdk.PluginSDK
	mu      sync.RWMutex
	timeout int
	proxy   string
	client  *http.Client

	sessions    map[string]*BrowserSession
	nextID      int
	wg          sync.WaitGroup
	stopCh      chan struct{}
	stopOnce    sync.Once
	profilesDir string // 持久化 profile 根目录（<data>/browser_profiles），空则禁用

	// 共享浏览器单例：所有 agent 共用一个 Chromium 进程（全局 UserDataDir，
	// 登录态/cookies 跨 agent、跨会话、跨插件重启保留），每个 start 创建一个
	// 新标签页（CDP Target）。同 source 复用自己的标签页。浏览器进程在
	// 最后一个标签页关闭后保留（避免反复冷启动），仅插件 Stop 时回收。
	sharedAllocCtx    context.Context
	sharedAllocCancel context.CancelFunc
	sharedMu          sync.Mutex
}

type BrowserSession struct {
	id         string
	allocCtx   context.Context // 共享浏览器进程上下文（shared=true 时指向全局单例）
	cancel     context.CancelFunc
	ctx        context.Context // 本会话的 Target 上下文（一个标签页）
	createdAt  time.Time
	timeout    time.Duration
	closed     bool
	mu         sync.Mutex
	currentURL string
	shared     bool   // true=共享浏览器的一个标签页；false=独占浏览器实例
	profileDir string // 非空表示使用持久化 profile（关闭时不删目录）
	sessionKey string // 共享模式下的复用键（agent 来源标识，同 key 复用同一标签页）
}

// sanitizeProfileName 消毒 profile 名：仅保留字母数字-_，防路径穿越。
func sanitizeProfileName(name string) string {
	var b []byte
	for _, c := range []byte(name) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b = append(b, c)
		}
	}
	if len(b) == 0 || string(b) == "." || string(b) == ".." {
		return ""
	}
	return string(b)
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name, stopCh: make(chan struct{}), sessions: make(map[string]*BrowserSession)}, nil
}

func (p *Plugin) Name() string { return p.name }

func readCfg[T string | int64 | float64](s sdk.SettingsAPI, key string, fallback T) T {
	v, err := s.Get(key)
	if err == nil && v != nil {
		if s, ok := v.(string); ok && s != "" {
			switch any(fallback).(type) {
			case string:
				return any(s).(T)
			case int64:
				if n, err := strconv.ParseInt(s, 10, 64); err == nil {
					return any(n).(T)
				}
			case float64:
				if n, err := strconv.ParseFloat(s, 64); err == nil {
					return any(n).(T)
				}
			}
		}
	}
	v2, err2 := s.GetCore("plugin." + "browser" + "." + key)
	if err2 == nil && v2 != nil {
		if s, ok := v2.(string); ok && s != "" {
			switch any(fallback).(type) {
			case string:
				return any(s).(T)
			case int64:
				if n, err := strconv.ParseInt(s, 10, 64); err == nil {
					return any(n).(T)
				}
			case float64:
				if n, err := strconv.ParseFloat(s, 64); err == nil {
					return any(n).(T)
				}
			}
		}
	}
	return fallback
}

func readArg[T string | int64 | float64](args map[string]interface{}, key string, fallback T) T {
	v, ok := args[key]
	if !ok || v == nil {
		return fallback
	}
	switch any(fallback).(type) {
	case string:
		if s, ok := v.(string); ok {
			return any(s).(T)
		}
	case int64:
		switch val := v.(type) {
		case float64:
			return any(int64(val)).(T)
		case string:
			if n, err := strconv.ParseInt(val, 10, 64); err == nil {
				return any(n).(T)
			}
		}
	case float64:
		switch val := v.(type) {
		case float64:
			return any(val).(T)
		case string:
			if n, err := strconv.ParseFloat(val, 64); err == nil {
				return any(n).(T)
			}
		}
	}
	return fallback
}

func errResult(msg string) map[string]interface{} {
	return map[string]interface{}{"isError": true, "content": msg}
}

func parseBrowserSessionTimeout(args map[string]interface{}) (time.Duration, error) {
	raw := strings.TrimSpace(readArg(args, "timeout", ""))
	if raw == "" {
		return 0, fmt.Errorf("timeout is required；创建浏览器会话时必须明确指定关闭时长，如 15m 或 2h")
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout <= 0 {
		return 0, fmt.Errorf("invalid timeout %q；请使用大于 0 的时长，如 15m 或 2h", raw)
	}
	return timeout, nil
}

func newHTTPClient(timeout int, proxyURL string) *http.Client {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   time.Duration(timeout) * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   time.Duration(timeout) * time.Second,
		ResponseHeaderTimeout: time.Duration(timeout) * time.Second,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{
		Timeout:   time.Duration(timeout) * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}

}

// ── Start / Stop ──────────────────────────────────────────

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s
	s.SetAutoRestart(true)
	// 入站通道：本插件用 p.name 通道注入输入（见 InjectInputSync 调用），
	// 输入侧必须显式登记 —— 否则"把该 inputch 划给驻留子"会报 `inputch 未注册`。
	_ = s.RegisterInputChannel(p.name, sdk.ChannelDef{})

	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "timeout", Default: "30", Type: "int",
		DisplayName: "HTTP 超时(秒)", Description: "HTTP 请求超时时间",
		Category: "browser",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "proxy", Default: "", Type: "string",
		DisplayName: "HTTP 代理", Description: "HTTP 代理地址，如 http://proxy:port",
		Category: "browser",
	})

	t := readCfg(s.Settings(), "timeout", float64(30))
	p.timeout = int(t)
	if p.timeout < 5 {
		p.timeout = 5
	}
	if p.timeout > 120 {
		p.timeout = 120
	}
	p.proxy = readCfg(s.Settings(), "proxy", "")
	p.client = newHTTPClient(p.timeout, p.proxy)

	// 持久化 profile 根目录：<data>/browser_profiles
	if dd, err := s.Settings().GetCore("daemon.data_dir"); err == nil {
		if s2, ok := dd.(string); ok && s2 != "" {
			p.profilesDir = filepath.Join(s2, "browser_profiles")
		}
	}

	tp := p.name + "_"

	cleaner := func(output string) string {
		var r struct{ Content string }
		if json.Unmarshal([]byte(output), &r) == nil && r.Content != "" {
			return r.Content
		}
		return output
	}

	s.RegisterTool(tp+"search", sdk.ToolDef{
		Name:        tp + "search",
		Description: "使用 Bing 搜索网页。返回标题、URL 和摘要。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{"type": "string", "description": "搜索关键词"},
				"count": map[string]interface{}{"type": "integer", "description": "结果数量(1-20，默认5)"},
			},
			"required": []string{"query"},
		},
		Cleaner: cleaner,
	}, p.handleSearch)

	s.RegisterTool(tp+"fetch", sdk.ToolDef{
		Name:        tp + "fetch",
		Description: "抓取 URL 内容。mode=auto 时遇 403/429 自动降级用无头 Chromium 渲染；mode=render 强制用 Chromium；mode=quick 纯 HTTP 不降级。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"url":       map[string]interface{}{"type": "string", "description": "HTTP/HTTPS URL"},
				"max_chars": map[string]interface{}{"type": "integer", "description": "最大返回字符数(默认20000)"},
				"mode":      map[string]interface{}{"type": "string", "description": "auto(默认)/render(强制Chromium)/quick(纯HTTP)"},
			},
			"required": []string{"url"},
		},
		Cleaner: cleaner,
	}, p.handleFetch)

	s.RegisterTool(tp+"render", sdk.ToolDef{
		Name:        tp + "render",
		Description: "无头 Chromium 渲染网页并提取文本(normal 模式)。支持 JS 渲染的页面。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"url":  map[string]interface{}{"type": "string", "description": "URL"},
				"wait": map[string]interface{}{"type": "integer", "description": "等待 JS 渲染的秒数(默认0)"},
			},
			"required": []string{"url"},
		},
		Cleaner: cleaner,
	}, p.handleRender)

	s.RegisterTool(tp+"start", sdk.ToolDef{
		Name:        tp + "start",
		Description: "启动交互式浏览器会话。Agent 必须在创建时明确指定 timeout；到期后插件关闭标签页。同来源复用已有标签页时，也按本次 timeout 重新设定关闭时间。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"url":     map[string]interface{}{"type": "string", "description": "初始导航 URL（可选）"},
				"timeout": map[string]interface{}{"type": "string", "description": "必填，会话关闭前的存活时长，如 15m、2h；必须大于 0"},
				"profile": map[string]interface{}{"type": "string", "description": "持久化档案名（可选，如 main）。同名档案共享登录态与浏览历史；不指定则为一次性临时会话"},
			},
			"required": []string{"timeout"},
		},
	}, p.handleBrowserStart)

	s.RegisterTool(tp+"navigate", sdk.ToolDef{
		Name:        tp + "navigate",
		Description: "在交互式浏览器中导航到指定 URL。自动等待页面 body 加载完成。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":   map[string]interface{}{"type": "string", "description": "浏览器会话 ID"},
				"url":  map[string]interface{}{"type": "string", "description": "目标 URL"},
				"wait": map[string]interface{}{"type": "integer", "description": "页面加载后额外等待秒数(默认2，反爬页面建议5)"},
			},
			"required": []string{"id", "url"},
		},
	}, p.handleNavigate)

	s.RegisterTool(tp+"screenshot", sdk.ToolDef{
		Name:        tp + "screenshot",
		Description: "对交互式浏览器当前页面截图。返回 base64 编码的 PNG 图片。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":     map[string]interface{}{"type": "string", "description": "浏览器会话 ID"},
				"full":   map[string]interface{}{"type": "boolean", "description": "是否全页截图(默认 false，仅视口)"},
				"format": map[string]interface{}{"type": "string", "description": "图片格式: 仅支持 png(默认 png)"},
			},
			"required": []string{"id"},
		},
	}, p.handleScreenshot)

	s.RegisterTool(tp+"html", sdk.ToolDef{
		Name:        tp + "html",
		Description: "获取交互式浏览器当前页面 JS 渲染后的完整 HTML。用于模型分析页面结构、定位元素。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":        map[string]interface{}{"type": "string", "description": "浏览器会话 ID"},
				"max_chars": map[string]interface{}{"type": "integer", "description": "最大返回字符数(默认50000)"},
			},
			"required": []string{"id"},
		},
	}, p.handleHTML)

	s.RegisterTool(tp+"click", sdk.ToolDef{
		Name:        tp + "click",
		Description: "在交互式浏览器中点击元素。自动等待元素可见后再点击。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":       map[string]interface{}{"type": "string", "description": "浏览器会话 ID"},
				"selector": map[string]interface{}{"type": "string", "description": "CSS 选择器"},
				"wait":     map[string]interface{}{"type": "integer", "description": "等待元素出现的超时毫秒数(默认3000)"},
			},
			"required": []string{"id", "selector"},
		},
	}, p.handleClick)

	s.RegisterTool(tp+"type", sdk.ToolDef{
		Name:        tp + "type",
		Description: "在交互式浏览器中向输入框输入文字。自动等待元素可见、聚焦后清空再输入。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":       map[string]interface{}{"type": "string", "description": "浏览器会话 ID"},
				"selector": map[string]interface{}{"type": "string", "description": "CSS 选择器"},
				"text":     map[string]interface{}{"type": "string", "description": "要输入的文字"},
				"submit":   map[string]interface{}{"type": "boolean", "description": "输入后按回车(默认 false)"},
				"wait":     map[string]interface{}{"type": "integer", "description": "等待元素出现的超时毫秒数(默认3000)"},
			},
			"required": []string{"id", "selector", "text"},
		},
	}, p.handleType)

	s.RegisterTool(tp+"scroll", sdk.ToolDef{
		Name:        tp + "scroll",
		Description: "在交互式浏览器中滚动页面。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":     map[string]interface{}{"type": "string", "description": "浏览器会话 ID"},
				"dir":    map[string]interface{}{"type": "string", "description": "方向: up, down, left, right(默认 down)"},
				"amount": map[string]interface{}{"type": "integer", "description": "滚动像素数(默认 500)"},
			},
			"required": []string{"id"},
		},
	}, p.handleScroll)

	s.RegisterTool(tp+"install", sdk.ToolDef{
		Name:        tp + "install",
		Description: "安装并启动共享浏览器后端（homeagent-browser.service，systemd 托管）。前提：本机已有 chromium 二进制（无则先提示用户安装：apt install chromium 或等价命令）。安装后所有 agent 共享同一浏览器实例与登录态。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, p.handleBrowserInstall)

	s.RegisterTool(tp+"close", sdk.ToolDef{
		Name:        tp + "close",
		Description: "关闭交互式浏览器会话，释放资源。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{"type": "string", "description": "浏览器会话 ID"},
			},
			"required": []string{"id"},
		},
	}, p.handleBrowserClose)

	p.wg.Add(1)
	go p.cleanupLoop()

	log.Printf("[%s] started, timeout=%ds proxy=%q", p.name, p.timeout, p.proxy)
	return nil
}

func (p *Plugin) Stop() error {
	p.stopOnce.Do(func() {
		close(p.stopCh)
		p.wg.Wait()
		if p.client != nil {
			p.client.CloseIdleConnections()
		}
		p.mu.Lock()
		for _, s := range p.sessions {
			s.Close()
		}
		p.sessions = nil
		p.mu.Unlock()
		log.Printf("[%s] stopped", p.name)
	})
	return nil
}

// ── SSRF ──────────────────────────────────────────────────

var privateCIDRs []*net.IPNet

func init() {
	for _, c := range []string{
		"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12",
		"192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16",
		"::1/128", "fc00::/7", "fe80::/10",
	} {
		_, n, _ := net.ParseCIDR(c)
		if n != nil {
			privateCIDRs = append(privateCIDRs, n)
		}
	}
}

func isPrivateIP(ip net.IP) bool {
	for _, n := range privateCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (p *Plugin) ssrfCheck(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http/https allowed, got: %s", u.Scheme)
	}
	ips, err := net.LookupHost(u.Hostname())
	if err != nil {
		return fmt.Errorf("DNS lookup failed: %w", err)
	}
	for _, ip := range ips {
		if parsed := net.ParseIP(ip); parsed != nil && isPrivateIP(parsed) {
			return fmt.Errorf("blocked request to private IP: %s (%s)", u.Hostname(), ip)
		}
	}
	return nil
}

// ── Bing Search ───────────────────────────────────────────

type searchResult struct {
	Title, URL, Snippet string
}

func (p *Plugin) bingSearch(query string, count int) ([]searchResult, error) {
	// 用 cn.bing.com：www.bing.com 对程序化请求常回 302（同意/重定向页），拿不到结果块。
	// 另：Bing 忽略 count 参数，翻页靠 first=，这里保留 count 只为兼容旧调用语义。
	u := fmt.Sprintf("https://cn.bing.com/search?q=%s&first=1&count=%d&setlang=zh-CN", url.QueryEscape(query), count)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Bing 返回 HTTP %d（%d 字节）", resp.StatusCode, len(body))
	}
	results := parseBingResults(string(body), count)
	if len(results) == 0 {
		// 关键：把「解析不出来」与「真的没结果」区分开。
		// 以前两者都变成 "No results found."，版式一变就静默退化成「搜不到」。
		return nil, fmt.Errorf("Bing 返回 %d 字节但未解析出结果（可能被反爬或版式变更，可改用 deepsearch 插件）", len(body))
	}
	return results, nil
}

var (
	bingBlockRe   = regexp.MustCompile(`<li class="b_algo"`)
	bingTitleRe   = regexp.MustCompile(`(?s)<h2[^>]*>\s*<a[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	bingAnyLinkRe = regexp.MustCompile(`(?s)<a[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	bingSnipRe    = regexp.MustCompile(`(?s)<p class="b_lineclamp[^"]*"[^>]*>(.*?)</p>`)
	bingCaptionRe = regexp.MustCompile(`(?s)<div class="b_caption"[^>]*>(.*?)</div>`)
)

// splitBingBlocks 按块标记切分，每块内容延伸到下一个块标记为止。
//
// 不用 `<li class="b_algo"(?s)(.*?)</li>`：结果块内部可能嵌套 <li>（deep links），
// 非贪婪匹配会在错误位置截断；而且块内第一个 <a> 往往是 Bing 的「来源行」，
// 取到的是 `deepin.orghttps://www.deepin.org` 这种垃圾标题。
func splitBingBlocks(pageHTML string) []string {
	locs := bingBlockRe.FindAllStringIndex(pageHTML, -1)
	if len(locs) == 0 {
		return nil
	}
	blocks := make([]string, 0, len(locs))
	for i, loc := range locs {
		end := len(pageHTML)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		blocks = append(blocks, pageHTML[loc[1]:end])
	}
	return blocks
}

func parseBingResults(pageHTML string, count int) []searchResult {
	if count <= 0 {
		count = 5
	}
	var results []searchResult
	for _, block := range splitBingBlocks(pageHTML) {
		if len(results) >= count {
			break
		}
		// 标题：现代 Bing 是 <h2><a href=...>标题</a></h2>；没有 h2 时才退回到块内第一个链接。
		var href, title string
		if m := bingTitleRe.FindStringSubmatch(block); m != nil {
			href, title = m[1], html.UnescapeString(stripTags(m[2]))
		} else if m := bingAnyLinkRe.FindStringSubmatch(block); m != nil {
			href, title = m[1], html.UnescapeString(stripTags(m[2]))
		}
		href = bingRealURL(html.UnescapeString(href))

		// 摘要：新版在 p.b_lineclamp*，旧版在 div.b_caption > p
		var snippet string
		if m := bingSnipRe.FindStringSubmatch(block); m != nil {
			snippet = html.UnescapeString(stripTags(m[1]))
		} else if m := bingCaptionRe.FindStringSubmatch(block); m != nil {
			snippet = html.UnescapeString(stripTags(m[1]))
		}

		title, snippet = strings.TrimSpace(title), strings.TrimSpace(snippet)
		if href == "" || title == "" || !strings.HasPrefix(href, "http") {
			continue
		}
		results = append(results, searchResult{Title: title, URL: href, Snippet: snippet})
	}
	return results
}

// bingRealURL 解开 Bing 的跳转包装：/ck/a?...&u=a1<base64url>&... → 真实 URL。
// 不解的话模型拿到的是 `https://cn.bing.com/ck/a?...` 这种不可读地址。
func bingRealURL(href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	if !strings.Contains(href, "/ck/a") && !strings.Contains(href, "u=a1") {
		return href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	raw := u.Query().Get("u")
	if !strings.HasPrefix(raw, "a1") {
		return href
	}
	b64 := raw[2:]
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding} {
		if dec, err := enc.DecodeString(b64); err == nil {
			s := string(dec)
			if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
				return s
			}
		}
	}
	return href
}

func (p *Plugin) handleSearch(args map[string]interface{}) (interface{}, error) {
	query := readArg(args, "query", "")
	if query == "" {
		return errResult("query is required"), nil
	}
	count := 5
	if v, ok := args["count"].(float64); ok && v > 0 {
		count = int(v)
	}
	if count < 1 {
		count = 1
	}
	if count > 20 {
		count = 20
	}
	results, err := p.bingSearch(query, count)
	if err != nil {
		return errResult("search failed: " + err.Error()), nil
	}
	if len(results) == 0 {
		return map[string]interface{}{"content": "No results found."}, nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Search results for %q:\n\n", query))
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("%d. %s\n   %s\n   %s\n\n", i+1, r.Title, r.URL, r.Snippet))
	}
	return map[string]interface{}{"content": strings.TrimSpace(sb.String())}, nil
}

// ── Web Fetch (quick) ──────────────────────────────────────

func htmlToText(html string) string {
	for _, tag := range []string{"<script", "<style"} {
		closing := "</" + tag[1:] + ">"
		for {
			start := strings.Index(strings.ToLower(html), tag)
			if start < 0 {
				break
			}
			end := strings.Index(html[start:], closing)
			if end < 0 {
				break
			}
			html = html[:start] + html[start+end+len(closing):]
		}
	}
	for _, tag := range []string{"</p>", "</div>", "</h1>", "</h2>", "</h3>", "</h4>", "</h5>", "</h6>", "</li>", "</tr>", "</blockquote>", "<br", "</pre>"} {
		html = strings.ReplaceAll(html, tag, "\n")
	}
	html = stripTags(html)
	for _, pair := range [][2]string{
		{"&amp;", "&"}, {"&lt;", "<"}, {"&gt;", ">"},
		{"&quot;", "\""}, {"&#39;", "'"}, {"&nbsp;", " "},
	} {
		html = strings.ReplaceAll(html, pair[0], pair[1])
	}
	lines := strings.Split(html, "\n")
	var cleaned []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		in := []rune(line)
		var out []rune
		space := false
		for _, r := range in {
			if unicode.IsSpace(r) {
				if !space {
					out = append(out, ' ')
					space = true
				}
			} else {
				out = append(out, r)
				space = false
			}
		}
		cleaned = append(cleaned, string(out))
	}
	return strings.Join(cleaned, "\n")
}

func stripTags(s string) string {
	var out strings.Builder
	inTag := false
	for _, r := range s {
		if r == '<' {
			inTag = true
			continue
		}
		if r == '>' {
			inTag = false
			continue
		}
		if !inTag {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func (p *Plugin) handleFetch(args map[string]interface{}) (interface{}, error) {
	rawURL := readArg(args, "url", "")
	if rawURL == "" {
		return errResult("url is required"), nil
	}
	maxChars := 20000
	if v, ok := args["max_chars"].(float64); ok && v > 0 {
		maxChars = int(v)
	}
	if maxChars > 500000 {
		maxChars = 500000
	}
	if err := p.ssrfCheck(rawURL); err != nil {
		return errResult(err.Error()), nil
	}

	mode := readArg(args, "mode", "auto")
	if mode == "render" {
		return p.fetchWithChromium(rawURL, maxChars)
	}

	req, _ := http.NewRequest("GET", rawURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	resp, err := p.client.Do(req)
	if err != nil {
		return errResult("fetch failed: " + err.Error()), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		resp.Body.Close()
		if (resp.StatusCode == 403 || resp.StatusCode == 429) && mode == "auto" {
			return p.fetchWithChromium(rawURL, maxChars)
		}
		return errResult(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, resp.Status)), nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, int64(maxChars)+50000))
	rawText := string(body)
	ct := resp.Header.Get("Content-Type")
	var extracted string
	if strings.Contains(ct, "text/html") {
		extracted = htmlToText(rawText)
	} else if strings.Contains(ct, "application/json") {
		var v interface{}
		if json.Unmarshal(body, &v) == nil {
			if pretty, err := json.MarshalIndent(v, "", "  "); err == nil {
				extracted = string(pretty)
			}
		}
		if extracted == "" {
			extracted = rawText
		}
	} else {
		extracted = rawText
	}
	extracted = strings.TrimSpace(extracted)
	if len(extracted) > maxChars {
		extracted = extracted[:maxChars] + "\n\n[Content truncated]"
	}
	if extracted == "" {
		extracted = "(empty content)"
	}
	return map[string]interface{}{
		"content": extracted,
		"details": map[string]interface{}{"url": rawURL, "status": resp.StatusCode, "content_type": ct},
	}, nil
}

// ── Chromium Render (normal) ──────────────────────────────

func (p *Plugin) fetchWithChromium(rawURL string, maxChars int) (interface{}, error) {
	chromiumPath := "/usr/local/bin/chromium"
	if _, err := os.Stat(chromiumPath); err != nil {
		return errResult("HTTP 403 且 Chromium 不可用，无法降级渲染"), nil
	}
	var out bytes.Buffer
	cmd := exec.Command(chromiumPath, "--headless", "--disable-gpu", "--no-sandbox", "--dump-dom", rawURL)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return errResult("Chromium 渲染失败: " + err.Error()), nil
	}
	text := htmlToText(out.String())
	if len(text) > maxChars {
		text = text[:maxChars] + "\n\n[Content truncated]"
	}
	return map[string]interface{}{
		"content": text,
		"details": map[string]interface{}{"url": rawURL, "mode": "render_fallback", "content_type": "text/html"},
	}, nil
}

// handleRender 无头渲染 JS 页面并提取文本（normal 模式）。
// 主路径走共享浏览器后端：开临时标签页（带全机登录态）→ 渲染 → 取 text → 关标签页；
// 后端不可用时 failback 到独立 chromium --dump-dom（无登录态，仅保功能）。
func (p *Plugin) handleRender(args map[string]interface{}) (interface{}, error) {
	rawURL := readArg(args, "url", "")
	if rawURL == "" {
		return errResult("url is required"), nil
	}
	if err := p.ssrfCheck(rawURL); err != nil {
		return errResult(err.Error()), nil
	}
	waitSec := int64(readArg(args, "wait", float64(0)))

	var title, html string
	rendered := false

	ok, needInstall, _ := p.ensureBackend()
	if ok {
		remoteCtx, remoteCancel := chromedp.NewRemoteAllocator(context.Background(), cdpEndpoint)
		defer remoteCancel()
		tabCtx, tabCancel := chromedp.NewContext(remoteCtx)
		defer tabCancel()
		actions := []chromedp.Action{
			chromedp.Navigate(rawURL),
			chromedp.WaitReady("body"),
		}
		if waitSec > 0 {
			actions = append(actions, chromedp.Sleep(time.Duration(waitSec)*time.Second))
		}
		actions = append(actions,
			chromedp.Title(&title),
			chromedp.OuterHTML("html", &html),
		)
		// 整体限时 30s，防慢页拖死工具
		rctx, rcancel := context.WithTimeout(tabCtx, 30*time.Second)
		defer rcancel()
		if err := chromedp.Run(rctx, actions...); err == nil {
			rendered = true
		} else {
			log.Printf("[%s] render via backend failed (%v), fallback to dump-dom", p.name, err)
		}
	} else if needInstall {
		return map[string]interface{}{
			"error":        "browser backend not installed",
			"need_install": true,
			"guide":        "调用 browser_install 安装共享后端；或重试本工具自动降级为独立 chromium 渲染（不带登录态）",
		}, nil
	}

	if !rendered {
		chromiumPath := "/usr/local/bin/chromium"
		if _, err := os.Stat(chromiumPath); err != nil {
			if _, e2 := exec.LookPath("chromium"); e2 == nil {
				chromiumPath = "chromium"
			} else {
				return errResult("no chromium available"), nil
			}
		}
		var out bytes.Buffer
		cmd := exec.Command(chromiumPath, "--headless", "--disable-gpu", "--no-sandbox", "--dump-dom", rawURL)
		cmd.Stdout = &out
		done := make(chan error, 1)
		go func() { done <- cmd.Run() }()
		select {
		case err := <-done:
			if err != nil {
				return errResult("chromium: " + err.Error()), nil
			}
		case <-time.After(30 * time.Second):
			cmd.Process.Kill()
			<-done // 回收子进程避免僵尸
			return errResult("chromium dump-dom timeout (30s)"), nil
		}
		html = out.String()
	}

	text := htmlToText(html)
	origLen := len(text)
	truncated := origLen > 5000
	if truncated {
		text = text[:5000]
	}
	result := ""
	if title != "" {
		result = fmt.Sprintf("标题: %s\nURL: %s\n\n", title, rawURL)
	}
	result += text
	if truncated {
		result += fmt.Sprintf("\n\n...(仅显示前 5000 字符，共 %d 字符)", origLen)
	}
	mode := "backend-tab"
	if !rendered {
		mode = "local-dump-dom"
	}
	return map[string]interface{}{"content": result, "title": title, "mode": mode}, nil
}

func cdpReachable(endpoint string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(endpoint + "/json/version")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// systemdUnitActive 检查 homeagent-browser.service 是否已安装。
func systemdUnitInstalled() bool {
	out, err := exec.Command("systemctl", "cat", "homeagent-browser.service").CombinedOutput()
	return err == nil && len(out) > 0
}

// startSystemdUnit 尝试 systemctl start（单元已安装但未运行时用）。
func startSystemdUnit() error {
	return exec.Command("systemctl", "start", "homeagent-browser.service").Run()
}

// cdpEndpoint 是共享 Chromium 后端的 CDP 地址（homeagent-browser.service）。
const cdpEndpoint = "http://127.0.0.1:9222"

// ensureBackend 确保共享浏览器后端可用：探测 → 拉起已装服务 → 报告未装。
// 返回 (ok, needInstall, err)。
func (p *Plugin) ensureBackend() (bool, bool, error) {
	if cdpReachable(cdpEndpoint) {
		return true, false, nil
	}
	if systemdUnitInstalled() {
		if err := startSystemdUnit(); err == nil {
			// 等待 CDP 就绪（chromium 启动 ~1-3s）
			for i := 0; i < 10; i++ {
				time.Sleep(500 * time.Millisecond)
				if cdpReachable(cdpEndpoint) {
					return true, false, nil
				}
			}
		}
		return false, false, fmt.Errorf("browser backend service installed but failed to start")
	}
	return false, true, nil // 未安装
}

// sharedTab 在共享后端上开一个新标签页（RemoteAllocator + NewContext）。
func sharedTab(allocCtx context.Context) (context.Context, context.CancelFunc, error) {
	tabCtx, tabCancel := chromedp.NewContext(allocCtx)
	if err := chromedp.Run(tabCtx); err != nil {
		tabCancel()
		return nil, nil, err
	}
	return tabCtx, tabCancel, nil
}

// localSpawnFailback 本地拉起一次性 Chromium（离线机器无法装 systemd 服务的兜底）。
// 用临时 profile，登录态不跨会话保留——仅保证功能可用。
func (p *Plugin) localSpawnFailback() (context.Context, context.CancelFunc, context.CancelFunc, error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.WindowSize(1280, 800),
	)
	if p.proxy != "" {
		opts = append(opts, chromedp.Flag("proxy-server", p.proxy))
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, _ := chromedp.NewContext(allocCtx)
	if err := chromedp.Run(ctx); err != nil {
		cancelAlloc()
		return nil, nil, nil, err
	}
	return allocCtx, cancelAlloc, nil, nil
}

func (p *Plugin) handleBrowserStart(args map[string]interface{}) (interface{}, error) {
	timeout, err := parseBrowserSessionTimeout(args)
	if err != nil {
		return errResult(err.Error()), nil
	}

	source := readArg(args, "source", "")
	if source == "" {
		source = "default"
	}

	// 同 source 复用已有标签页
	p.mu.Lock()
	for _, s := range p.sessions {
		if s.shared && s.sessionKey == source && !s.closed {
			s.mu.Lock()
			id := s.id
			cur := s.currentURL
			s.createdAt = time.Now()
			s.timeout = timeout
			closesAt := s.createdAt.Add(timeout)
			s.mu.Unlock()
			p.mu.Unlock()
			log.Printf("[%s] reused browser session %s: timeout=%v closes_at=%s source=%s", p.name, id, timeout, closesAt.Format(time.RFC3339), source)
			return map[string]interface{}{
				"id":        id,
				"status":    "reused",
				"url":       cur,
				"timeout":   timeout.String(),
				"closes_at": closesAt.Format(time.RFC3339),
				"note":      "已复用本来源的现有标签页，并按本次 timeout 重新设定关闭时间",
			}, nil
		}
	}
	p.mu.Unlock()

	var session *BrowserSession

	// 路径一：systemd 托管的共享后端（主路径）
	ok, needInstall, berr := p.ensureBackend()
	if ok {
		remoteCtx, remoteCancel := chromedp.NewRemoteAllocator(context.Background(), cdpEndpoint)
		probe, _ := chromedp.NewContext(remoteCtx)
		if err := chromedp.Run(probe); err != nil {
			remoteCancel()
			return errResult("connect to browser backend failed: " + err.Error()), nil
		}
		tabCtx, tabCancel := chromedp.NewContext(remoteCtx)
		if err := chromedp.Run(tabCtx); err != nil {
			remoteCancel()
			return errResult("open tab failed: " + err.Error()), nil
		}
		session = &BrowserSession{
			allocCtx:   remoteCtx,
			cancel:     tabCancel,
			ctx:        tabCtx,
			createdAt:  time.Now(),
			timeout:    timeout,
			shared:     true,
			sessionKey: source,
		}
	} else if needInstall {
		guide := "浏览器后端未安装。请确认后调用 browser_install 工具完成安装：" +
			"需要本机有 chromium 二进制（apt install chromium 或等价命令），" +
			"插件会注册 homeagent-browser.service 并启动。" +
			"若本机无法联网安装 chromium，可继续用本地临时模式（重试 browser_start 即自动降级）。"
		return map[string]interface{}{
			"error":        "backend not installed",
			"need_install": true,
			"guide":        guide,
		}, nil
	} else {
		return errResult("browser backend error: " + berr.Error()), nil
	}

	p.mu.Lock()
	p.nextID++
	id := fmt.Sprintf("browser_%d", p.nextID)
	session.id = id
	p.sessions[id] = session
	p.mu.Unlock()

	initURL := readArg(args, "url", "")
	if initURL != "" {
		if err := chromedp.Run(session.ctx,
			chromedp.Navigate(initURL),
			chromedp.WaitReady("body"),
		); err != nil {
			session.Close()
			p.mu.Lock()
			delete(p.sessions, id)
			p.mu.Unlock()
			return errResult("navigate failed: " + err.Error()), nil
		}
		session.currentURL = initURL
	}

	closesAt := session.createdAt.Add(timeout)
	log.Printf("[%s] created browser session %s: url=%s timeout=%v closes_at=%s source=%s", p.name, id, initURL, timeout, closesAt.Format(time.RFC3339), source)
	return map[string]interface{}{
		"id":        id,
		"status":    "created",
		"mode":      "shared-backend",
		"url":       initURL,
		"timeout":   timeout.String(),
		"closes_at": closesAt.Format(time.RFC3339),
	}, nil
}

func (p *Plugin) getSession(id string) (*BrowserSession, error) {
	p.mu.Lock()
	s, ok := p.sessions[id]
	p.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("浏览器会话 %s 不存在或已关闭", id)
	}
	return s, nil
}

func (p *Plugin) handleNavigate(args map[string]interface{}) (interface{}, error) {
	id := readArg(args, "id", "")
	rawURL := readArg(args, "url", "")
	if id == "" || rawURL == "" {
		return errResult("id 和 url 不能为空"), nil
	}
	s, err := p.getSession(id)
	if err != nil {
		return errResult(err.Error()), nil
	}
	waitSec := int64(readArg(args, "wait", float64(2)))
	if err := chromedp.Run(s.ctx,
		chromedp.Navigate(rawURL),
		chromedp.WaitReady("body"),
		chromedp.Sleep(time.Duration(waitSec)*time.Second),
	); err != nil {
		return errResult("navigate failed: " + err.Error()), nil
	}
	s.currentURL = rawURL
	p.sdk.InjectTextNoMemory(p.name, p.name, fmt.Sprintf("[浏览器 %s 已导航到 %s]", id, rawURL))
	return map[string]interface{}{"status": "ok", "url": rawURL}, nil
}

func (p *Plugin) handleScreenshot(args map[string]interface{}) (interface{}, error) {
	id := readArg(args, "id", "")
	if id == "" {
		return errResult("id is required"), nil
	}
	s, sessErr := p.getSession(id)
	if sessErr != nil {
		return errResult(sessErr.Error()), nil
	}
	full := false
	if v, ok := args["full"].(bool); ok {
		full = v
	}
	format := readArg(args, "format", "png")
	if format != "png" {
		return errResult("仅支持 png 格式"), nil
	}
	var buf []byte
	var err error
	if full {
		err = chromedp.Run(s.ctx, chromedp.FullScreenshot(&buf, 90))
	} else {
		err = chromedp.Run(s.ctx, chromedp.Screenshot("body", &buf))
	}
	if err != nil {
		return errResult("screenshot failed: " + err.Error()), nil
	}
	b64 := base64.StdEncoding.EncodeToString(buf)
	return map[string]interface{}{
		"status":   "ok",
		"format":   format,
		"size":     len(buf),
		"base64":   b64,
		"data_uri": fmt.Sprintf("data:image/png;base64,%s", b64),
	}, nil
}

func (p *Plugin) handleHTML(args map[string]interface{}) (interface{}, error) {
	id := readArg(args, "id", "")
	if id == "" {
		return errResult("id is required"), nil
	}
	s, err := p.getSession(id)
	if err != nil {
		return errResult(err.Error()), nil
	}
	maxChars := 50000
	if v, ok := args["max_chars"].(float64); ok && v > 0 {
		maxChars = int(v)
	}
	var html string
	if err := chromedp.Run(s.ctx, chromedp.OuterHTML("html", &html)); err != nil {
		return errResult("get html failed: " + err.Error()), nil
	}
	var title, currentURL string
	chromedp.Run(s.ctx,
		chromedp.Title(&title),
		chromedp.Location(&currentURL),
	)
	truncated := len(html) > maxChars
	if truncated {
		html = html[:maxChars] + "\n\n[HTML truncated]"
	}
	return map[string]interface{}{
		"status": "ok",
		"title":  title,
		"url":    currentURL,
		"html":   html,
		"length": len(html),
	}, nil
}

func (p *Plugin) handleClick(args map[string]interface{}) (interface{}, error) {
	id := readArg(args, "id", "")
	selector := readArg(args, "selector", "")
	if id == "" || selector == "" {
		return errResult("id 和 selector 不能为空"), nil
	}
	s, err := p.getSession(id)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if err := chromedp.Run(s.ctx,
		chromedp.WaitVisible(selector),
		chromedp.Click(selector),
	); err != nil {
		return errResult("click failed (element may not exist or page blocking): " + err.Error()), nil
	}
	return map[string]interface{}{"status": "ok", "selector": selector}, nil
}

func (p *Plugin) handleType(args map[string]interface{}) (interface{}, error) {
	id := readArg(args, "id", "")
	selector := readArg(args, "selector", "")
	text := readArg(args, "text", "")
	if id == "" || selector == "" || text == "" {
		return errResult("id, selector, text 不能为空"), nil
	}
	s, err := p.getSession(id)
	if err != nil {
		return errResult(err.Error()), nil
	}
	actions := []chromedp.Action{
		chromedp.WaitVisible(selector),
		chromedp.Click(selector, chromedp.NodeVisible),
		chromedp.Clear(selector),
		chromedp.SendKeys(selector, text),
	}
	submit := false
	if v, ok := args["submit"].(bool); ok {
		submit = v
	}
	if submit {
		actions = append(actions, chromedp.SendKeys(selector, "\r"))
	}
	if err := chromedp.Run(s.ctx, actions...); err != nil {
		return errResult("type failed (element may not exist or page blocking): " + err.Error()), nil
	}
	return map[string]interface{}{"status": "ok", "selector": selector}, nil
}

func (p *Plugin) handleScroll(args map[string]interface{}) (interface{}, error) {
	id := readArg(args, "id", "")
	if id == "" {
		return errResult("id is required"), nil
	}
	s, err := p.getSession(id)
	if err != nil {
		return errResult(err.Error()), nil
	}
	dir := readArg(args, "dir", "down")
	amount := readArg(args, "amount", float64(500))
	var scrollJS string
	switch dir {
	case "up":
		scrollJS = fmt.Sprintf("window.scrollBy(0, -%d)", int(amount))
	case "down":
		scrollJS = fmt.Sprintf("window.scrollBy(0, %d)", int(amount))
	case "left":
		scrollJS = fmt.Sprintf("window.scrollBy(-%d, 0)", int(amount))
	case "right":
		scrollJS = fmt.Sprintf("window.scrollBy(%d, 0)", int(amount))
	default:
		return errResult("dir 必须是 up/down/left/right"), nil
	}
	if err := chromedp.Run(s.ctx, chromedp.Evaluate(scrollJS, nil)); err != nil {
		return errResult("scroll failed: " + err.Error()), nil
	}
	return map[string]interface{}{"status": "ok", "dir": dir, "amount": amount}, nil
}

func (p *Plugin) handleBrowserClose(args map[string]interface{}) (interface{}, error) {
	id := readArg(args, "id", "")
	if id == "" {
		return errResult("id is required"), nil
	}
	p.mu.Lock()
	s, ok := p.sessions[id]
	if ok {
		delete(p.sessions, id)
	}
	p.mu.Unlock()
	if !ok {
		return errResult(fmt.Sprintf("浏览器会话 %s 不存在或已关闭", id)), nil
	}
	s.Close()
	log.Printf("[%s] closed browser session %s", p.name, id)
	return map[string]interface{}{"status": "closed", "id": id}, nil
}

func (s *BrowserSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.cancel()
}

func (p *Plugin) cleanupLoop() {
	defer p.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			now := time.Now()
			p.mu.Lock()
			for id, s := range p.sessions {
				s.mu.Lock()
				closesAt := s.createdAt.Add(s.timeout)
				expired := !now.Before(closesAt)
				s.mu.Unlock()
				if expired {
					log.Printf("[%s] cleanup: browser session %s reached agent-specified close time %s", p.name, id, closesAt.Format(time.RFC3339))
					delete(p.sessions, id)
					s.Close()
					// NoMemory：会话生命周期通知，不是记忆内容。
					p.sdk.InjectInterruptTextOpts(p.name, p.name,
						fmt.Sprintf("[浏览器会话 %s 已按指定时间关闭]", id), sdk.InjectOptions{NoMemory: true})
				}
			}
			p.mu.Unlock()
		}
	}
}

// ── browser_install：安装 systemd 托管的共享浏览器后端 ──────────

// handleBrowserInstall 注册 homeagent-browser.service 并启动，验证 CDP 可达。
// 返回给 agent 的结果含全机共享使用指南（由 agent 转述给用户）。
func (p *Plugin) handleBrowserInstall(args map[string]interface{}) (interface{}, error) {
	if cdpReachable(cdpEndpoint) {
		return map[string]interface{}{"status": "already_running", "endpoint": cdpEndpoint}, nil
	}

	// 探测 chromium 二进制
	chromePath := ""
	for _, c := range []string{
		"/usr/bin/chromium", "/usr/bin/chromium-browser",
		"/usr/local/bin/chromium", "/usr/bin/google-chrome",
	} {
		if _, err := os.Stat(c); err == nil {
			chromePath = c
			break
		}
	}
	if out, err := exec.LookPath("chromium"); err == nil && chromePath == "" {
		chromePath = out
	} else if out, err := exec.LookPath("google-chrome"); err == nil && chromePath == "" {
		chromePath = out
	}
	if chromePath == "" {
		return map[string]interface{}{
			"error": "chromium binary not found",
			"hint":  "请先安装 chromium：apt install chromium 或等价命令，然后重试 browser_install",
		}, nil
	}

	profileDir := ""
	if p.profilesDir != "" {
		profileDir = filepath.Join(p.profilesDir, "shared")
		os.MkdirAll(profileDir, 0755)
	} else {
		// profilesDir 未注入（无 data_dir），退到 /var/lib/homeagent-browser
		profileDir = "/var/lib/homeagent-browser"
		os.MkdirAll(profileDir, 0755)
	}

	unit := fmt.Sprintf(`[Unit]
Description=HomeAgent Shared Browser Backend (headless chromium, CDP :9222)
After=network.target

[Service]
Type=simple
ExecStart=%s --headless --no-sandbox --disable-gpu --disable-dev-shm-usage --remote-debugging-port=9222 --user-data-dir=%s --window-size=1280,800 about:blank
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
`, chromePath, profileDir)

	unitPath := "/etc/systemd/system/homeagent-browser.service"
	if err := os.WriteFile(unitPath, []byte(unit), 0644); err != nil {
		return map[string]interface{}{
			"error": "write unit failed (need root): " + err.Error(),
			"hint":  "插件进程无权限写 /etc/systemd/system 时，请让用户手动执行安装命令（见 manual_cmds）",
			"manual_cmds": []string{
				"sudo tee /etc/systemd/system/homeagent-browser.service <<'EOF'\n" + unit + "EOF",
				"sudo systemctl daemon-reload",
				"sudo systemctl enable --now homeagent-browser.service",
			},
		}, nil
	}
	for _, cmd := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "--now", "homeagent-browser.service"},
	} {
		if out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput(); err != nil {
			return map[string]interface{}{
				"error": fmt.Sprintf("%v: %s", cmd, string(out)),
			}, nil
		}
	}
	// 等待 CDP 就绪
	for i := 0; i < 20; i++ {
		time.Sleep(500 * time.Millisecond)
		if cdpReachable(cdpEndpoint) {
			guide := "共享浏览器后端已就绪（CDP " + cdpEndpoint + "）。\n" +
				"全机共享说明：本机所有 agent（HomeAgent、pi、opencode、deepseekharness 等）都可连接此实例：" +
				"登录一次全机可用；各 agent 各自占用独立标签页互不干扰；\n" +
				"- HomeAgent 内部：browser_start 即自动连接本后端\n" +
				"- 其他 agent：让其浏览器工具/MCP 连接 CDP 端点 " + cdpEndpoint + "（如 playwright connectOverCDP / puppeteer connect）\n" +
				"- 服务由 systemd 托管：崩溃自动重启，登录态持久保存在 " + profileDir
			log.Printf("[%s] browser backend installed and running (chrome=%s profile=%s)", p.name, chromePath, profileDir)
			return map[string]interface{}{
				"status":   "installed",
				"endpoint": cdpEndpoint,
				"chrome":   chromePath,
				"profile":  profileDir,
				"guide":    guide,
			}, nil
		}
	}
	return map[string]interface{}{"error": "service started but CDP not reachable after 10s"}, nil
}
