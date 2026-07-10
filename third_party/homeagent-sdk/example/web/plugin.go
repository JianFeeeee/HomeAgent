package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	"gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

type Plugin struct {
	name    string
	sdk     *sdk.PluginSDK
	mu      sync.RWMutex
	timeout int
	proxy   string
	client  *http.Client
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
		u, err := url.Parse(proxyURL)
		if err == nil {
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

func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key:         "plugin.web.timeout",
		Default:     "30",
		Type:        "int",
		DisplayName: "HTTP 超时(秒)",
		Description: "Web fetch 和搜索的 HTTP 请求超时时间",
		Category:    "web",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key:         "plugin.web.proxy",
		Default:     "",
		Type:        "string",
		DisplayName: "HTTP 代理",
		Description: "HTTP 代理地址，如 http://<proxy-host>:<proxy-port>。为空则不使用代理",
		Category:    "web",
	})

	t := getSetting[float64](s.Settings(), "timeout", 30)
	p.timeout = int(t)
	if p.timeout < 5 {
		p.timeout = 5
	}
	if p.timeout > 120 {
		p.timeout = 120
	}

	p.proxy = getSetting[string](s.Settings(), "proxy", "")
	p.client = newHTTPClient(p.timeout, p.proxy)

	tp := p.name + "_"

	s.RegisterTool(tp+"search", sdk.ToolDef{
		Name:        tp + "search",
		Description: "Search the web for current information using DuckDuckGo. Returns formatted results with titles, URLs, and snippets.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{"type": "string", "description": "Search query"},
				"count": map[string]interface{}{"type": "integer", "description": "Number of results (1-20, default 5)"},
			},
			"required": []string{"query"},
		},
	}, p.handleSearch)

	s.RegisterTool(tp+"fetch", sdk.ToolDef{
		Name:        tp + "fetch",
		Description: "Fetch a URL and extract readable content as markdown-like text. Blocked on private/internal IPs.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"url":       map[string]interface{}{"type": "string", "description": "HTTP/HTTPS URL to fetch"},
				"max_chars": map[string]interface{}{"type": "integer", "description": "Max characters to return (default 20000)"},
			},
			"required": []string{"url"},
		},
	}, p.handleFetch)

	proxyMsg := ""
	if p.proxy != "" {
		proxyMsg = fmt.Sprintf(", proxy: %s", p.proxy)
	}
	log.Printf("[%s] started, timeout: %ds%s", p.name, p.timeout, proxyMsg)
	return nil
}

func (p *Plugin) Stop() error {
	p.client.CloseIdleConnections()
	log.Printf("[%s] stopped", p.name)
	return nil
}

// ── SSRF 保护 ──────────────────────────────────────────────

var privateCIDRs []*net.IPNet

func init() {
	cidrs := []string{
		"127.0.0.0/8",    // loopback
		"10.0.0.0/8",     // private
		"172.16.0.0/12",  // private
		"192.168.0.0/16", // private
		"100.64.0.0/10",  // carrier-grade NAT
		"169.254.0.0/16", // link-local
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // IPv6 unique local
		"fe80::/10",      // IPv6 link-local
	}
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err == nil {
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
		return fmt.Errorf("only http/https URLs are allowed, got: %s", u.Scheme)
	}

	host := u.Hostname()
	ips, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("DNS lookup failed for %s: %w", host, err)
	}

	for _, ip := range ips {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			continue
		}
		if isPrivateIP(parsed) {
			return fmt.Errorf("blocked request to private IP: %s (%s)", host, ip)
		}
	}
	return nil
}

// ── DuckDuckGo 搜索 ────────────────────────────────────────

type ddgResult struct {
	Title   string
	URL     string
	Snippet string
}

func (p *Plugin) ddgSearch(query string, count int) ([]ddgResult, error) {
	form := url.Values{"q": {query}}
	req, err := http.NewRequest("POST", "https://html.duckduckgo.com/html/", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	return parseDDGResults(string(body), count), nil
}

func parseDDGResults(html string, count int) []ddgResult {
	var results []ddgResult

	// Find all result blocks: <div class="result__body"> ... </div>
	bodyMarker := `result__body"`
	for i := 0; i < len(html); i++ {
		idx := strings.Index(html[i:], bodyMarker)
		if idx < 0 {
			break
		}
		i += idx

		// Find closing </div>
		closeIdx := findClosingTag(html, i, "</div>")
		if closeIdx < 0 {
			break
		}
		block := html[i : closeIdx+6]

		r := parseSingleDDGResult(block)
		if r.URL != "" {
			results = append(results, r)
			if len(results) >= count {
				break
			}
		}

		i = closeIdx + 6
	}

	return results
}

func findClosingTag(s string, start int, tag string) int {
	depth := 1
	pos := start
	for pos < len(s) {
		nextOpen := strings.Index(s[pos:], `<div`)
		nextClose := strings.Index(s[pos:], tag)
		if nextClose < 0 {
			return -1
		}
		if nextOpen >= 0 && nextOpen < nextClose {
			depth++
			pos += nextOpen + 4
		} else {
			depth--
			if depth == 0 {
				return pos + nextClose
			}
			pos += nextClose + len(tag)
		}
	}
	return -1
}

func parseSingleDDGResult(block string) ddgResult {
	var r ddgResult

	// Extract URL and title from: <a rel="nofollow" class="result__a" href="URL">TITLE</a>
	urlMarker := `class="result__a" href="`
	uIdx := strings.Index(block, urlMarker)
	if uIdx >= 0 {
		start := uIdx + len(urlMarker)
		end := strings.Index(block[start:], `"`)
		if end >= 0 {
			r.URL = block[start : start+end]
		}

		aStart := strings.Index(block[start+end:], `>`)
		if aStart >= 0 {
			titleStart := start + end + aStart + 1
			aEnd := strings.Index(block[titleStart:], `</a>`)
			if aEnd >= 0 {
				r.Title = stripTags(block[titleStart : titleStart+aEnd])
			}
		}
	}

	// Extract snippet: <a class="result__snippet" ...> ... </a>
	snippetMarkers := []string{
		`<a class="result__snippet`,
		`<div class="result__snippet`,
	}
	for _, marker := range snippetMarkers {
		sIdx := strings.Index(block, marker)
		if sIdx >= 0 {
			aStart := strings.Index(block[sIdx:], `>`)
			if aStart >= 0 {
				snipStart := sIdx + aStart + 1
				snipEnd := strings.Index(block[snipStart:], `</a>`)
				if snipEnd < 0 {
					snipEnd = strings.Index(block[snipStart:], `</div>`)
				}
				if snipEnd >= 0 {
					r.Snippet = stripTags(block[snipStart : snipStart+snipEnd])
				}
			}
			break
		}
	}

	return r
}

// ── Web Fetch ──────────────────────────────────────────────

func (p *Plugin) handleFetch(args map[string]interface{}) (interface{}, error) {
	rawURL, _ := args["url"].(string)
	if rawURL == "" {
		return errorResult("url is required"), nil
	}

	maxChars := 20000
	if v, ok := args["max_chars"].(float64); ok && v > 0 {
		maxChars = int(v)
	}
	if maxChars > 500000 {
		maxChars = 500000
	}

	if err := p.ssrfCheck(rawURL); err != nil {
		return errorResult(err.Error()), nil
	}

	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return errorResult("invalid URL: " + err.Error()), nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := p.client.Do(req)
	if err != nil {
		return errorResult("fetch failed: " + err.Error()), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return errorResult(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, resp.Status)), nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxChars)+50000))
	if err != nil {
		return errorResult("read error: " + err.Error()), nil
	}

	rawText := string(body)

	// Extract readable content based on content type
	ct := resp.Header.Get("Content-Type")
	var extracted string
	if strings.Contains(ct, "text/html") {
		extracted = htmlToText(rawText)
	} else if strings.Contains(ct, "application/json") {
		// Pretty-print JSON
		var v interface{}
		if json.Unmarshal(body, &v) == nil {
			if pretty, err := json.MarshalIndent(v, "", "  "); err == nil {
				extracted = string(pretty)
			} else {
				extracted = rawText
			}
		} else {
			extracted = rawText
		}
	} else {
		extracted = rawText
	}

	// Clean up and truncate
	extracted = strings.TrimSpace(extracted)
	if len(extracted) > maxChars {
		extracted = extracted[:maxChars] + "\n\n[Content truncated]"
	}

	if extracted == "" {
		extracted = "(empty content)"
	}

	return map[string]interface{}{
		"content": extracted,
		"details": map[string]interface{}{
			"url":          rawURL,
			"status":       resp.StatusCode,
			"content_type": ct,
		},
	}, nil
}

// ── HTML → 文本 ──────────────────────────────────────────────

func htmlToText(html string) string {
	// Remove scripts
	for {
		start := strings.Index(strings.ToLower(html), "<script")
		if start < 0 {
			break
		}
		end := strings.Index(html[start:], "</script>")
		if end < 0 {
			break
		}
		html = html[:start] + html[start+end+9:]
	}

	// Remove styles
	for {
		start := strings.Index(strings.ToLower(html), "<style")
		if start < 0 {
			break
		}
		end := strings.Index(html[start:], "</style>")
		if end < 0 {
			break
		}
		html = html[:start] + html[start+end+8:]
	}

	// Replace block-level tags with newlines
	for _, tag := range []string{"</p>", "</div>", "</h1>", "</h2>", "</h3>", "</h4>", "</h5>", "</h6>", "</li>", "</tr>", "</blockquote>", "<br", "</pre>"} {
		html = strings.ReplaceAll(html, tag, "\n")
	}

	// Remove remaining tags
	html = stripTags(html)

	// Decode common entities
	html = strings.ReplaceAll(html, "&amp;", "&")
	html = strings.ReplaceAll(html, "&lt;", "<")
	html = strings.ReplaceAll(html, "&gt;", ">")
	html = strings.ReplaceAll(html, "&quot;", "\"")
	html = strings.ReplaceAll(html, "&#39;", "'")
	html = strings.ReplaceAll(html, "&nbsp;", " ")

	// Collapse whitespace
	lines := strings.Split(html, "\n")
	var cleaned []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Collapse internal whitespace
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

// ── Search 处理 ──────────────────────────────────────────────

func (p *Plugin) handleSearch(args map[string]interface{}) (interface{}, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return errorResult("query is required"), nil
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

	results, err := p.ddgSearch(query, count)
	if err != nil {
		return errorResult("search failed: " + err.Error()), nil
	}

	if len(results) == 0 {
		return map[string]interface{}{
			"content": "No results found.",
		}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Search results for %q:\n\n", query))
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("%d. %s\n   %s\n   %s\n\n", i+1, r.Title, r.URL, r.Snippet))
	}

	return map[string]interface{}{
		"content": strings.TrimSpace(sb.String()),
	}, nil
}

// ── 工具函数 ──────────────────────────────────────────────

func errorResult(msg string) map[string]interface{} {
	return map[string]interface{}{
		"isError": true,
		"content": msg,
	}
}

func getSetting[T any](s sdk.SettingsAPI, key string, def T) T {
	v, err := s.Get(key)
	if err != nil || v == nil {
		return def
	}
	val, ok := v.(T)
	if !ok {
		return def
	}
	return val
}
