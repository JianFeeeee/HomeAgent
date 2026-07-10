# memo 插件讲解

备忘插件，支持创建、完成、列出备忘，自动提醒未完成事项。

## 工具清单

| 工具 | 功能 | 源码 |
|------|------|------|
| `memo_create` | 创建一条备忘 | `handleCreate` |
| `memo_complete` | 标记备忘为已完成 | `handleComplete` |
| `memo_list` | 列出所有未完成备忘 | `handleList` |

## 核心设计

### 数据持久化

备忘存储在 JSON 文件中，路径由核心配置 `core.daemon.data_dir` 决定：

```go
// plugin.go:Start
dataDirVal, _ := s.Settings().GetCore("core.daemon.data_dir")
p.filePath = filepath.Join(fmt.Sprint(dataDirVal), "memos.json")
p.load()
```

`load()` 和 `save()` 实现 JSON 文件的读写，格式为 `{memos: [...], next_id: N}`。每次写操作后自动 `save()`，Stop 时也执行一次。

### PreAction 注入提醒

注册 `pre_action` 阶段钩子，在每次 LLM 调用前注入未完成备忘数量：

```go
// plugin.go:stagePreAction
n := p.pendingCount()
if n == 0 { return nil }
ctx.ContextMsgs = append(ctx.ContextMsgs, map[string]interface{}{
    "role":    "system",
    "content": fmt.Sprintf("目前有%d条备忘未完成，调用%slist工具读取具体内容", n, p.tp),
})
```

这样每次 LLM 处理消息时都感知到未完成备忘，无需主动查询。

### 定时打断提醒

每 5 分钟检查未完成备忘，如果有则通过中断通道提醒：

```go
// plugin.go:periodicCheck
ticker := time.NewTicker(5 * time.Minute)
for {
    select {
    case <-p.stopCh: return
    case <-ticker.C:
        n := p.pendingCount()
        if n == 0 { continue }
        p.sdk.InjectInterruptText(p.name, p.name,
            fmt.Sprintf("注意，你还有%d条备忘未标记完成，请检查", n))
    }
}
```

中断消息会打断当前 LLM 处理，在下一轮工具循环前插入 `[打断消息]`，确保 agent 不会长期忽略未完成备忘。

### 工具返回值

所有工具返回 `{content: string}` 或 `{isError: true, content: string}` 格式，LLM 通过 content 字段获取结果文本。

## 注意事项

- `memo_create` 的 content 参数应包含事项的完整描述，方便后续回顾
- `memo_complete` 只标记为 done，不删除数据，保留历史
- 更早的暂停时自动 save，防丢数据
