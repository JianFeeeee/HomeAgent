# QQ `output_send` 回声/无限循环（核心侧缺陷） QQ `output_send` 回声/无限循环（核心侧缺陷，非插件）

> 状态：**已修复**（核心已具备轮次上限：`core.agent.max_tool_turns`，默认 10，
> 在 `internal/agent/core/task.go` 到达上限即强制收尾；回归测试
> `TestMaxToolTurns_CapsRunawayLoop`。本文保留为缺陷定位过程记录。）
>
> 原始状态（修复前）：**待修复**
> 影响：Agent 单轮内每 ~10 秒调用一次 `output_send__qq`，持续数十分钟不结束（实测单轮 `1780624ms`，80+ 次工具调用）
> 定位结论：**问题在核心（富回执 + 每轮重复追加同一条“继续”占位 + 无轮次上限），QQ 插件侧已是最小回执，改插件无效**

---

## 1. 现象

生产日志（`/home/newqqagent`，homed 运行期）：

```
18:31:39 process.go:299: [agent] tool output_send__qq result: 已通过 [qq] 通道发送: map[status:sent]
18:31:47 process.go:299: [agent] tool output_send__qq result: 已通过 [qq] 通道发送: map[status:sent]
18:31:58 process.go:299: [agent] tool output_send__qq result: 已通过 [qq] 通道发送: map[status:sent]
18:32:06 ...
18:32:14 ...
（每 8~12 秒一条，payload 长度各异：387/237/246/203/252/270/254/231/258/227/188/227/200/209/155/284/212/173/198/242/218/191…）
18:31:19 eventloop.go:426: [agent] text from qq → response (1780624ms, tools=[... 80+ 项 ...])
```

- 每条内容**都不同**，所以“相同参数才拦”的插件保险不会触发。
- 不是 webhook 回声（15 分钟内只有 1 条真实入站中断）。
- 是模型每轮都收到“发送成功的富回执”，把它当成“继续下一步”的信号。

---

## 2. 根因链（核心侧，三层）

### 2.1 QQ 插件已经返回最小回执 —— 但被核心丢弃

`third_party/homeagent-sdk/example/qq/plugin.go`（`handleChannelOutput` 尾部）：

```go
if sendErr != nil {
    return nil, sendErr
}
// 成功：返回极简标记。不再回传 NapCat 原始响应（含 message_id 等）给模型，
// 避免模型把"发送成功"当成"上一步完成，继续下一步"的信号驱动循环。
return "ok", nil
```

插件返回的是字符串 `"ok"`。

### 2.2 核心 proc 桥丢弃它并伪造 `status:sent`

`internal/plugin/proc/plugin.go:336`（`(*Plugin).invokeOutput`）：

```go
raw, err := p.proc.Call(MethodOutputInvoke, OutputInvokeParams{Channel: channel, Args: args})
if err != nil { return nil, err }
if len(raw) == 0 {
    return map[string]interface{}{"status": "sent"}, nil
}
var res map[string]interface{}
if err := json.Unmarshal(raw, &res); err != nil {
    return map[string]interface{}{"status": "sent"}, nil   // ← "ok" 不是 JSON object，落到这里
}
if _, ok := res["status"]; !ok {
    res["status"] = "sent"                                  // ← 再兜底
}
return res, nil
```

插件返回 `"ok"` → `json.Unmarshal` 进 `map[string]interface{}` 失败 → 核心合成 `{status: sent}`。
**插件的返回值在这里被完全覆盖，所以只改插件永远修不掉回声。**

### 2.3 核心把这个富回执喂给模型

`internal/agent/core/output.go:81`（HEAD / 部署中的 homed 行为）：

```go
return fmt.Sprintf("已通过 [%s] 通道发送: %v", channel, result)
// → "已通过 [qq] 通道发送: map[status:sent]"
```

模型看到“发送成功 + 详情”后继续调用 `output_send__qq`，形成闭环。

### 2.4 核心没有工具轮次硬上限（放大器）

`core.agent.max_tool_turns` 只在配置层定义，**agent 循环里没有任何读取点**：

```
internal/config/registry.go:551   set("core.agent.max_tool_turns", "10")
internal/config/registry.go:669   reg(ConfigDef{Key: "core.agent.max_tool_turns", ...})
$ grep -rn 'max_tool_turns\|MaxToolTurns' internal/agent/   → 无结果
```

`internal/agent/core/process.go:242` 的唯一终止条件是：

```go
if len(resp.ToolCalls) == 0 {
    return resp.Content, toolsUsed, toolResults, nil
}
```

即：**模型不主动停，循环就永不结束**。`core.agent.max_tool_turns`（本机 DB 现为 `1000`）形同虚设。

### 2.5 每轮重复追加同一条 user 占位（“反复喂相同消息”的直接来源）

`internal/agent/core/process.go:77-82`，位置在 `for turn := 0; ; turn++` 循环的**顶部**：

```go
for turn := 0; ; turn++ {
    for _, interrupt := range a.drainInterrupts() { ... }

    // 工具轮产出的 tool/assistant 消息作结尾会被 400 拒绝,故补一条 user 占位。
    if last := msgs[len(msgs)-1]; last.Role == "assistant" || last.Role == "tool" {
        msgs = append(msgs, agentAPI.Message{          // ← process.go:79
            Role:    "user",
            Content: "请根据以上工具结果继续。",
        })
    }
    ...
}
```

`msgs` 于 `process.go:32` 在循环**外**创建，循环内只增不减：

- 每轮工具调用结束后，`msgs` 尾部必然是 `tool` 消息；
- 下一轮顶部判断成立，于是**再追加一条完全相同的** `请根据以上工具结果继续。`；
- 不做替换、不做去重、不做裁剪（`ContextPolicy: prune` 只裁剪 `a.context`，不裁剪 `msgs`）。

跑 N 轮，模型收到的 prompt 里就叠了 N 条一模一样的“继续”指令。这才是“核心把前面相同消息反复喂给模型”的直接机制，也是把模型持续推向 `output_send` 的持续推力。

**预期行为**：占位消息应当（a）仅在没有尾部 user 消息时补一条，或（b）补之前先移除上一条同类占位，保持至多一条；绝不能线性累积。

---

## 3. 现有未完成/未部署的修复

| 文件 | 状态 | 内容 |
| --- | --- | --- |
| `internal/agent/core/output.go:81` | **已改，未提交** (`M`) | `return fmt.Sprintf("已通过 [%s] 通道发送: %v", ...)` → `return "ok"`（含解释回声的注释） |
| `internal/plugin/proc/plugin.go:336` | **已改，未提交** (`M`) | 仍是伪造 `status:sent` 的版本，未处理非 map 返回值 |
| `third_party/homeagent-sdk/example/qq/plugin.go` | **已改，未提交** (`M`) | `handleChannelOutput` 返回 `"ok"` |

运行中的 `homed` 是 **Sep 6 11:39** 构建的二进制，不含 `output.go` 的极简回执改动 → 仍回显富回执。
另外该二进制用旧 SDK 协议（`shmMagic` 直连，无 `unifiedMagic`），而 `third_party/homeagent-sdk` 仓库 HEAD 已升级到统一区域协议（`fc23612` 起）。**重建并部署 homed 时二者必须对齐**（见 §5）。

---

## 4. 建议修复

### 4.1 （必须）让模型只看到最小回执

**方案 A（最小改动，已在工作区）**：`internal/agent/core/output.go`

```go
// internal/agent/core/output.go:81
// 成功回执：只返回极简标记，不回传完整插件响应。
// 「已通过 [qq] 通道发送: map[status:sent message_id:xxx]」这类富回执
// 会驱动模型继续调用 output_send（回声效应），是 output loop 的根源之一。
return "ok"
```

**方案 B（同时修掉 proc 桥的伪造）**：`internal/plugin/proc/plugin.go:336`
不要对非 map 结果伪造 `status:sent`，保留插件真实返回；例如：

```go
if len(raw) == 0 {
    return map[string]interface{}{"status": "ok"}, nil
}
var res map[string]interface{}
if err := json.Unmarshal(raw, &res); err != nil {
    // 插件返回的是标量（如 "ok"）——原样透传，不要伪造 status
    var scalar interface{}
    if err2 := json.Unmarshal(raw, &scalar); err2 == nil {
        return scalar, nil
    }
    return map[string]interface{}{"status": "ok"}, nil
}
```

注意：`output.go` 仍需要 `status == "unconfirmed"/"queued"` 的判定，改成标量透传时该判定自然跳过（非 map），语义正确。

### 4.2 （必须）工具循环硬上限

在 `internal/agent/core/process.go` 的工具循环里读取并强制 `core.agent.max_tool_turns`：

- 位置：`for turn := 0; ; turn++ {` 循环内，执行工具前/每轮结束后检查。
- 语义：达到上限时追加一条系统消息（如 `[系统] 已达到最大工具轮次 N，请立即总结并停止调用工具`），并终止循环返回当前内容，而不是继续下一轮。
- 至少要在 `turn > maxTurns` 时强制 `break`，避免模型不停调用。

### 4.3 （建议）QQ 插件侧保持最小回执

`third_party/homeagent-sdk/example/qq/plugin.go` 的 `return "ok", nil` 是正确的，保留即可。
**不要**再依赖插件侧修这个回声——见 §2.2。

### 4.4 （必须）修掉每轮重复追加的 user 占位

`internal/agent/core/process.go:77-82`。改为“至多保留一条”，例如：

```go
// 只在尾部是工具轮产物时补位；先移除上一条同类占位，避免线性累积。
if last := msgs[len(msgs)-1]; last.Role == "assistant" || last.Role == "tool" {
    // 若尾部之上已经存在一条我们自己的占位，就不要重复追加。
    if !isContinuationPlaceholder(msgs[len(msgs)-1]) {
        msgs = append(msgs, agentAPI.Message{
            Role:    "user",
            Content: continuationPlaceholder,
        })
    }
}
```

更稳妥的写法：在追加前从 `msgs` 尾部回扫，删除所有此前由本机制插入的占位，再追加一条。判定不要只靠字符串相等，建议给占位加一个可识别标记（例如 `internal:continuation`）或单独的 `NoMemory/Role` 约定，避免误删真实用户消息。

同时建议给 `msgs` 加长度/ token 上限（或定期裁剪历史），防止长任务把上下文堆爆（这正是 §2.4 无轮次上限的伴生问题）。

---

## 5. 部署前提与步骤

> ⚠️ 部署 homed 前必须先对齐 SDK 协议，否则所有子进程插件握手失败（`统一区域魔数不匹配`）。

1. **确认工具链协议与要部署的 homed 一致**
   - 现状：`/usr/local/bin/homed` = 旧协议；`/usr/local/bin/plugindev` 已替换为旧协议版本（备份 `/usr/local/bin/plugindev.bak-20260910-174547`）。
   - 若决定升级到统一区域协议，则需同时：升级 homed 二进制 + 用新 SDK（`third_party/homeagent-sdk` HEAD）重建全部插件。
   - 若维持旧协议：用 `/usr/local/bin/plugindev`（旧）重建插件即可，不要用仓库 HEAD 的 `tools/plugindev` 直接 `go run`。
2. **构建 homed**：`go build ./...` 已验证通过；产出替换 `/usr/local/bin/homed`（按项目部署纪律：备份 → 原子替换）。
3. **重启**：`systemctl restart homeagent.service`
4. **重建受影响的子进程插件**（协议一致时）：至少 `qq`。

---

## 6. 验收标准

修复后，发一条会触发回复的 QQ 消息，应满足：

1. 日志中 `output_send__qq` 的 tool result **不再包含** `已通过 [qq] 通道发送: map[status:sent]`；
2. 单轮只发送 1 条（或模型明确决定的多条**不同**消息），**不出现每 ~10 秒一次的持续调用**；
3. 当模型异常地持续调用工具时，日志出现达到 `core.agent.max_tool_turns` 的终止记录，且该轮在有限步内结束；
4. `eventloop.go:426` 的 `text from qq → response` 耗时应回落到正常量级（秒级~分钟级），不再是 30 分钟；
5. 抓取发往上游的请求（或用调试钩子 dump `req.Messages`），确认 `请根据以上工具结果继续。` 在整轮 prompt 中**至多出现一次**；修复前应为 N 条（N=轮数），这正是 §2.5 的判据。

---

## 7. 相关背景（避免误修）

- QQ 插件的 `beforeToolcall` 循环保险只拦“参数完全相同的重复调用”（`max_duplicate_qq_send`），**拦不住内容各异的循环**；本次循环每条内容都不同，所以保险未触发。这是设计使然，不是 bug。
- 15 分钟内仅 1 条真实 QQ 入站中断，说明**不是** webhook 把出站消息当入站回灌，**不是**插件回声。
- `internal/plugin/proc/plugin.go:122` 的 `invokeCleaner` 签名不匹配（此前导致 `go build` 失败）**已被修复**，当前 `go build ./...` 通过。
