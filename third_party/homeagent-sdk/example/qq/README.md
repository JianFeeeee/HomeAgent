# qq 插件讲解

QQ 集成插件，通过 [NapCat](https://github.com/NapNeko/NapCat) OneBot 协议对接 QQ 机器人框架。

## 工具清单（15 个）

| 工具 | 功能 | 源码 |
|------|------|------|
| `qq_get_message` | 获取通过中断通知的消息正文 | `handleGetMessage` |
| `qq_send_private_msg` | 发送私聊消息 | `handleSendPrivate` |
| `qq_send_group_msg` | 发送群消息 | `handleSendGroup` |
| `qq_send_file` | 发送文件/图片到私聊或群聊 | `handleSendFile` |
| `qq_get_history` | 获取历史消息 | `handleGetHistory` |
| `qq_get_groups` | 获取群列表 | `handleGetGroups` |
| `qq_get_friends` | 获取好友列表 | `handleGetFriends` |
| `qq_resolve_name` | 解析 QQ 号/群号为可读名称 | `handleResolveName` |
| `qq_get_group_member_info` | 获取群成员信息 | `handleGetGroupMemberInfo` |
| `qq_group_manage` | 群综合管理（踢人/禁言/改名等 18 个子命令） | `handleGroupManage` |
| `qq_friend_action` | 好友管理（删除/拉黑/同意请求等） | `handleFriendAction` |
| `qq_get_group_files` | 群文件操作（列表/搜索/下载） | `handleGetGroupFiles` |
| `qq_upload_group_file` | 上传文件到群 | `handleUploadGroupFile` |
| `qq_send_like` | 点赞/戳一戳 | `handleSendLike` |
| `qq_ocr_image` | 图片文字识别 | `handleOcrImage` |

## 核心设计

### 消息接收：Webhook + 中断

插件启动一个 HTTP 服务器监听 NapCat 的回调 webhook，收到消息后先保存到内存循环缓冲区：

```go
// plugin.go:handleWebhook
p.mu.Lock()
localID := p.nextID
p.nextID++
msg := &SavedMessage{LocalID: localID, UserID: evt.UserID, ...}
p.messages = append(p.messages, msg)
// 保留最近 maxMessages(2000) 条
```

然后通过 `InjectInterruptText` 将摘要推送给 LLM，LLM 再主动调用 `qq_get_message` 获取完整内容：

```go
// plugin.go:handleWebhook - interrupt text
interrupt = fmt.Sprintf("来自%s的群聊消息，通过id%d使用%sget_message工具获取消息正文",
    nickname, localID, tp)
p.sdk.InjectInterruptText(p.name, p.name, interrupt)
```

这种"先通知摘要，按需拉取全文"的设计避免了大量消息涌入 LLM 上下文。

### 管理员优先级标记

配置 `admin` 后，管理员消息的中断文本会加 `【重要！老大消息】` 前缀：

```go
if p.adminID > 0 && evt.UserID == p.adminID {
    interrupt = "【重要！老大消息】" + interrupt
}
```

### 消息过滤

`sensitiveFilter` 在发出消息前过滤敏感信息：

```go
func (p *Plugin) sensitiveFilter(text string) string {
    text = reAPIKey.ReplaceAllString(text, "$1=***")
    text = reSKKey.ReplaceAllString(text, "sk-***")
    text = reInternalIP.ReplaceAllString(text, "[IP]")
    return text
}
```

保护 API Key、`sk-` 开头的密钥串、内网 IP 不被发到外部。

### NapCat HTTP 调用

所有 NapCat API 调用通过 `napcat()` 方法统一转发：

```go
func (p *Plugin) napcat(action string, params map[string]interface{}) (interface{}, error) {
    url := fmt.Sprintf("%s/%s", p.napcatURL, action)
    resp, err := http.Post(url, "application/json", bytes.NewReader(data))
    // 返回原始 JSON 字符串
}
```

NapCat API 地址通过配置 `plugin.qq.napcat_url` 设置。

### 消息存储

使用循环缓冲区（`[]*SavedMessage`），最多保留 2000 条。每条消息包含本地 ID、QQ 号、昵称、群号、群名、文本内容、时间戳。`qq_get_message` 通过 `local_id` 查找。

## 配置项

| Key | 默认值 | 说明 |
|-----|--------|------|
| `plugin.qq.listen` | `127.0.0.1:<port>` | Webhook 监听地址 |
| `plugin.qq.napcat_url` | `http://127.0.0.1:<port>` | NapCat HTTP API 基地址 |
| `plugin.qq.admin` | 空 | 管理员 QQ 号 |

## 注意事项

- 依赖 NapCat 框架运行，需先启动 NapCat 并配置 webhook 指向本插件地址
- 群管理中的破坏性操作（踢人、退群等）在描述中已写明需先请示管理员
- `qq_get_message` 返回的消息对象包含完整字段，LLM 可据此判断消息类型和来源
