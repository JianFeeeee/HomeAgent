# files 插件讲解

文件系统操作插件，提供文件的读写编辑和目录浏览能力。

## 工具清单

| 工具 | 功能 | 源码 |
|------|------|------|
| `files_read` | 读取文件内容，支持 offset/limit 分段 | `handleRead` |
| `files_write` | 写入文件，支持 4 种模式 | `handleWrite` |
| `files_edit` | 精确字符串替换编辑 | `handleEdit` |
| `files_ls` | 列出目录内容 | `handleLs` |

## 核心设计

### 沙箱路径隔离

`resolvePath()` 方法将用户传入的路径解析为沙箱内的绝对路径。关键逻辑：

```go
// 相对路径以沙箱根目录为基准拼接
if !filepath.IsAbs(userPath) {
    userPath = filepath.Join(p.filesDir, userPath)
}
// 检查是否越界
base := filepath.Clean(p.filesDir)
if base != "/" && !strings.HasPrefix(abs, base+string(filepath.Separator)) && abs != base {
    return "", fmt.Errorf("path outside sandbox")
}
```

当沙箱根设为 `/` 时放行所有路径；设为特定目录时拒绝访问外部。配置项 `plugin.files.dir` 控制此值。

### 分段读取

`files_read` 支持 `offset`（行号，1-indexed）和 `limit`（行数上限），用于大文件分段查看：

```go
// plugin.go:handleRead
lines := strings.Split(text, "\n")
offset := 0  // 从 args["offset"] 解析，1-indexed 转 0-indexed
limit := totalLines - offset
// ...
end := offset + limit
selected := lines[offset:end]
```

如果未读完会在末尾追加提示 `[Showing lines X-Y of Z. Use offset=N to continue.]`。

### 四种写入模式

`files_write` 通过 `mode` 参数区分：

- **overwrite**（默认）：`os.WriteFile` 覆盖写入，自动创建父目录
- **append**：`os.OpenFile` 以 `O_APPEND|O_CREATE|O_WRONLY` 打开，追加内容
- **insert**：将文件按行分割，在指定行号前插入新内容，再写回
- **create**：先检查文件是否已存在，存在则报错，不存在才创建

### 精确编辑

`files_edit` 接收 `edits` 数组，每个元素有 `old` 和 `new`。要求每个 `old` 在原文中**恰好出现一次**，防止 LLM 误替换：

```go
count := strings.Count(content, oldText)
if count == 0 { /* 报错未找到 */ }
if count > 1 { /* 报错存在多处匹配 */ }
content = strings.Replace(content, oldText, newText, 1)
```

### 目录列表

`files_ls` 按字母序排序，目录加 `/` 后缀，同时显示文件大小。默认上限 500 条。

## 配置项

| Key | 默认值 | 说明 |
|-----|--------|------|
| `plugin.files.dir` | `/` | 文件操作沙箱根目录 |

## 注意事项

- 所有路径操作前都经过 `resolvePath` 沙箱检查
- 错误结果统一用 `errorResult()` 返回 `{isError: true, content: msg}` 格式，LLM 可据此判断
- `files_write` 的 insert/append 模式不检查文件是否存在（不存在则报错），overwrite/create 模式自动创建父目录
