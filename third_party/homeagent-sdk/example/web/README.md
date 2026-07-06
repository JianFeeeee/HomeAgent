# web 插件讲解

网络工具插件，提供网页搜索和内容抓取功能。

## 工具清单

| 工具 | 功能 | 源码 |
|------|------|------|
| `web_search` | 通过 DuckDuckGo 搜索网页 | `handleSearch` |
| `web_fetch` | 抓取指定 URL 的内容 | `handleFetch` |

## 核心设计

### 搜索实现

`web_search` 使用 DuckDuckGo 的 HTML 搜索页面（非 API，免注册）：

```go
// plugin.go:handleSearch
url := fmt.Sprintf("https://html.duckduckgo.com/html/?q=%s", url.QueryEscape(query))
resp, err := p.httpClient().Get(url)
```

解析策略：扫描 HTML 查找 `<a class="result__a"` 标签提取标题和链接，查找 `<a class="result__snippet"` 提取摘要。搜索最多返回 8 条结果。

由于 DuckDuckGo 在国内被墙，支持通过代理访问（见配置项）。

### 页面抓取

`web_fetch` 直接 HTTP GET 目标 URL 并以文本形式返回内容：

```go
// plugin.go:handleFetch
resp, err := p.httpClient().Get(rawURL)
body, _ := io.ReadAll(resp.Body)
```

返回内容限制最大 100KB，超过截断并提示。

### SSRF 防护

`handleFetch` 检查 URL 是否为内网 IP，防止服务端请求伪造攻击：

```go
// plugin.go:handleFetch — SSRF guard
if !strings.HasPrefix(parsedURL, "http") {
    return errorResult("only http/https allowed"), nil
}
// 内网 IP 段检查（127.0.0.0/8, 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16）
```

### HTTP 客户端

`newHTTPClient()` 创建带超时和可选代理的 HTTP 客户端：

```go
// plugin.go:newHTTPClient
func (p *Plugin) newHTTPClient() *http.Client {
    transport := &http.Transport{}
    if p.proxy != "" {
        proxyURL, _ := url.Parse(p.proxy)
        transport.Proxy = http.ProxyURL(proxyURL)
    }
    return &http.Client{
        Timeout:   time.Duration(p.timeout) * time.Second,
        Transport: transport,
    }
}
```

超时和代理均在配置中设置。

### 配置读取

```go
// plugin.go:Start
p.timeout = getSetting[float64](s.Settings(), "timeout", 30)
p.proxy = getSetting[string](s.Settings(), "proxy", "")
```

`getSetting` 是泛型辅助函数，支持类型安全的配置读取。

## 配置项

| Key | 默认值 | 说明 |
|-----|--------|------|
| `plugin.web.timeout` | `30` | HTTP 请求超时（秒） |
| `plugin.web.proxy` | 空 | HTTP 代理地址，如 `http://<proxy-host>:<proxy-port>` |

## 注意事项

- DuckDuckGo HTML 格式可能随网站更新变化，如果搜索结果解析失败需调整 `parseSearchResults` 中的 HTML 标记匹配
- SSRF 防护默认阻止内网请求，如需访问内网资源需修改 `isInternalIP` 逻辑
- 搜索结果依赖 DuckDuckGo 可用性，在国内使用建议配置代理
