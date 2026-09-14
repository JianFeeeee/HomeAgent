package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

type ForwardRule struct {
	GroupID  int64  `json:"group_id"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Password string `json:"password"`
	Template string `json:"template"`
}

func rconSend(host string, port int, password, cmd string) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("rcon dial: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	buf := make([]byte, 4096)
	// Login
	pkt := rconPacket(1, 3, password)
	if _, err := conn.Write(pkt); err != nil {
		return fmt.Errorf("rcon login write: %w", err)
	}
	if _, err := io.ReadFull(conn, buf[:12]); err != nil {
		return fmt.Errorf("rcon login read: %w", err)
	}
	// Command
	pkt = rconPacket(2, 2, cmd)
	if _, err := conn.Write(pkt); err != nil {
		return fmt.Errorf("rcon cmd write: %w", err)
	}
	n, err := io.ReadFull(conn, buf[:12])
	if err != nil && err != io.ErrUnexpectedEOF {
		return fmt.Errorf("rcon cmd read: %w (n=%d)", err, n)
	}
	return nil
}

func rconPacket(id, typ int32, body string) []byte {
	b := []byte(body)
	b = append(b, 0) // null terminator
	b = append(b, 0) // padding
	length := 4 + 4 + len(b)
	pkt := make([]byte, 12+len(b))
	binary.LittleEndian.PutUint32(pkt, uint32(length))
	binary.LittleEndian.PutUint32(pkt[4:], uint32(id))
	binary.LittleEndian.PutUint32(pkt[8:], uint32(typ))
	copy(pkt[12:], b)
	return pkt
}

type DownloadTask struct {
	FileID    string `json:"file_id"`
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Path      string `json:"path,omitempty"`
	Error     string `json:"error,omitempty"`
	CreatedAt string `json:"created_at"`
}

type qqAuthContext struct {
	active     bool
	owner      bool
	messageID  int64
	userID     int64
	groupID    int64
	isGroup    bool
	generation uint64
}

var qqMessageIDRe = regexp.MustCompile(`message_id=(-?\d+)`)

// 默认只开放公共信息与当前 QQ 会话所需能力。日历、邮件、记忆、知识库、
// 主机文件/命令、设备、配置、插件管理及 QQ 联系人/跨会话列表均不在白名单中。
const defaultPublicToolAllowlist = `["output_send__qq","output_list_channels","qq_get_message","qq_get_history","qq_mark_read","qq_get_group_member_info","qq_get_group_files","qq_video_download","weather_*","browser_search","browser_fetch","browser_render","ocr_*","multimodal_*","bili_*","music_*","ai_image_*"]`

const defaultGroupToolAllowlists = `{"*":["output_send__qq","output_list_channels","qq_get_message","qq_get_history","qq_mark_read","qq_get_group_member_info","qq_get_group_files","qq_video_download","weather_*","browser_search","browser_fetch","browser_render","ocr_*","multimodal_*","bili_*","music_*","ai_image_*"]}`

const (
	// 单轮 QQ 触发的工具调用总数上限（0 = 不限制）。只作跑飞兜底，
	// 不应拦下正常的长时间多步任务。
	defaultMaxQQToolCalls = 200
	// 单轮 QQ 主动发送的不同消息条数上限（0 = 不限制）。
	defaultMaxQQOutputCalls = 20
	// 单轮内同一条消息（参数完全相同）允许重复发送的次数（0 = 不限制）。
	defaultMaxDuplicateSend = 1
)

type Plugin struct {
	name           string
	sdk            *sdk.PluginSDK
	listenAddr     string
	napcatURL      string
	remoteDir      string
	filesDir       string
	webhookToken   string
	ownerIDs       []int64
	botID          int64
	botNickname    string
	dmPolicy       string
	groupPolicy    string
	httpClient     *http.Client
	allowFrom      map[int64]struct{}
	groupAllowFrom map[int64]struct{}
	srv            *http.Server
	agentfsDir     string
	downloadMu     sync.Mutex
	downloadTasks  []*DownloadTask
	typingMu       sync.Mutex
	typingMap      map[int64]*typingState

	authMu               sync.RWMutex
	auth                 qqAuthContext
	authGeneration       uint64
	authByMessageID      map[int64]qqAuthContext
	lastDenial           string
	denialLocked         bool
	toolCallCount        int
	outputCallCount      int
	outputSignatures     map[string]int
	maxQQToolCalls       int
	maxQQOutputCalls     int
	maxDuplicateSend     int
	groupToolAllowlists  map[int64][]string // 0 表示通配配置 "*"
	privateToolAllowlist []string

	// msg_id → peer 映射 + 会话最新状态（<7 天兜底 get_history + list_chats）
	msgMu  sync.Mutex
	msgMap map[int64]msgRef    // message_id → {peer, time}
	chats  map[int64]*chatMeta // peerID → 会话状态（群号或 QQ 号）

	// 消息合并（debounce）：同一会话、同一发送者在 batchWindow 内连续到达的消息
	// 合并成一次中断。同一个人连发「在吗」「帮我看看」「报错是这个」三条，
	// 逐条注入会把 Agent 唤醒三次，且前两次拿到的信息都不完整。
	batchMu     sync.Mutex
	batches     map[string]*pendingBatch
	batchWindow time.Duration // 最后一条到达后再等多久（<=0 = 关闭合并，逐条投递）
	batchMax    time.Duration // 一批最长等多久（防持续刷屏时永远不投）

	// injectHook 仅供测试：非 nil 时 injectInterrupt 走它而不是真实 SDK。
	injectHook func(text, level string)
}

// pendingBatch 是一批待投递的消息（同一会话、同一发送者、短时间内的连续消息）。
type pendingBatch struct {
	key      string
	isGroup  bool
	userID   int64
	groupID  int64
	nickname string
	msgIDs   []int64
	single   string // 单条时沿用的原文（含所有者/高危前缀），保证 n==1 行为不变
	owner    bool
	highRisk bool
	first    time.Time
	timer    *time.Timer
}

// qqBatchKey 同一会话 + 同一发送者 = 一组。私聊按 QQ 号；群聊按 (群号, QQ 号)——
// 群里不同人各发各的，不该并成一条。
func qqBatchKey(msgType string, groupID, userID int64) string {
	if msgType == "group" {
		return fmt.Sprintf("g:%d:%d", groupID, userID)
	}
	return fmt.Sprintf("p:%d", userID)
}

type typingState struct {
	userID int64
	stopCh chan struct{}
}

// msgRef 一条已见过的消息的引用：只记录 msg_id → (peer, time) 映射，不缓存正文。
// 用途：NapCat get_msg 的临时短号 <7 天失效时，据此把 get_msg 兜底为按 peer 拉 get_history。
type msgRef struct {
	peerID  int64
	isGroup bool
	time    int64 // 秒级时间戳
}

// chatMeta 一个会话（群/私聊）的最新状态，供 list_chats 展示。
// 只维护最新一条的短摘要（≤qqLastSumLen 字符）与未读数，不缓存完整历史。
type chatMeta struct {
	peerID   int64
	isGroup  bool
	name     string
	unread   int
	lastTime int64
	lastText string
	lastNick string
}

const qqMsgTTL = 7 * 86400 // 7 天：msg_id → peer 映射的有效期
const qqLastSumLen = 60    // list_chats 里最新一条摘要的最大长度

// snapshotMsg 记录一条策略允许的消息：更新 msg_id→peer 映射与会话未读/最新状态。
// 不缓存消息正文（仅最新一条留 ≤qqLastSumLen 的摘要供列表展示）。
func (p *Plugin) snapshotMsg(msgID, peerID int64, isGroup bool, t int64, nickname, text string) {
	if msgID <= 0 {
		return
	}
	p.msgMu.Lock()
	defer p.msgMu.Unlock()

	// msg_id 映射（7 天 TTL，惰性清理）
	p.msgMap[msgID] = msgRef{peerID: peerID, isGroup: isGroup, time: t}
	now := time.Now().Unix()
	if len(p.msgMap) > 2000 { // 定期清理过期项
		for k, v := range p.msgMap {
			if now-v.time > qqMsgTTL {
				delete(p.msgMap, k)
			}
		}
	}

	ch := p.chats[peerID]
	if ch == nil {
		ch = &chatMeta{peerID: peerID, isGroup: isGroup}
		p.chats[peerID] = ch
	}
	if ch.name == "" {
		if isGroup {
			ch.name = fmt.Sprintf("群%d", peerID)
		} else {
			ch.name = nickname
		}
	}
	// 按到达次序维护未读与最新摘要：仅当本条更新时才更新 lastTime/lastText（保持按时间排）
	if t > ch.lastTime {
		ch.lastTime = t
		ch.lastText = text
		ch.lastNick = nickname
	}
	ch.unread++
}

// lookupMsgRef 查 msg_id 映射，返回 (peer, isGroup, time, ok)。超过 7 天视为无效（交给 get_history）。
func (p *Plugin) lookupMsgRef(msgID int64) (int64, bool, int64, bool) {
	p.msgMu.Lock()
	defer p.msgMu.Unlock()
	ref, ok := p.msgMap[msgID]
	if !ok {
		return 0, false, 0, false
	}
	now := time.Now().Unix()
	if now-ref.time > qqMsgTTL {
		delete(p.msgMap, msgID)
		return 0, false, 0, false
	}
	return ref.peerID, ref.isGroup, ref.time, true
}

// markChatRead 清零某会话未读数（模型处理完该会话后调用）。
func (p *Plugin) markChatRead(peerID int64) {
	p.msgMu.Lock()
	defer p.msgMu.Unlock()
	if ch := p.chats[peerID]; ch != nil {
		ch.unread = 0
	}
}

// listChats 返回会话列表：按最新消息时间降序，含未读数与最新一条摘要。
func (p *Plugin) listChats(capN int) []map[string]interface{} {
	p.msgMu.Lock()
	list := make([]*chatMeta, 0, len(p.chats))
	for _, c := range p.chats {
		list = append(list, c)
	}
	p.msgMu.Unlock()

	// 降序（最新消息在前）
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].lastTime > list[j-1].lastTime; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	if len(list) > capN {
		list = list[:capN]
	}

	out := make([]map[string]interface{}, 0, len(list))
	for _, c := range list {
		typ := "private"
		if c.isGroup {
			typ = "group"
		}
		item := map[string]interface{}{
			"peer_id":   c.peerID,
			"type":      typ,
			"name":      c.name,
			"unread":    c.unread,
			"last_text": c.lastText,
			"last_nick": c.lastNick,
		}
		if c.lastTime > 0 {
			item["last_time"] = time.Unix(c.lastTime, 0).Format("2006-01-02 15:04")
		}
		out = append(out, item)
	}
	return out
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s

	s.Settings().RegisterDef(sdk.ConfigDef{Key: "listen", Default: "0.0.0.0:25580", Type: "string", DisplayName: "监听地址", Description: "Webhook HTTP 监听地址", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "napcat_url", Default: "http://127.0.0.1:3000", Type: "string", DisplayName: "NapCat 地址", Description: "NapCat HTTP API 基础 URL", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "owner", Default: "", Type: "string", DisplayName: "Bot 所有者 QQ", Description: "Bot 所有者 QQ 号列表，逗号分隔。所有者无论私聊或群聊均拥有完整工具权限", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "admin", Default: "", Type: "string", DisplayName: "Bot 所有者 QQ（旧配置）", Description: "兼容旧版 admin 配置；owner 为空时作为 Bot 所有者列表", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "group_tool_allowlists", Default: defaultGroupToolAllowlists, Type: "string", DisplayName: "群聊工具白名单", Description: "JSON 对象：群号到允许工具名/前缀*的数组；* 为未单独配置群的默认白名单。Bot 所有者不受限制", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "private_tool_allowlist", Default: defaultPublicToolAllowlist, Type: "string", DisplayName: "非所有者私聊工具白名单", Description: "JSON 数组，支持工具精确名和尾部 * 前缀。硬性私人资源工具不能由此白名单放行；Bot 所有者不受权限限制", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "max_qq_tool_calls", Default: defaultMaxQQToolCalls, Type: "int", DisplayName: "单轮 QQ 工具调用上限", Description: "QQ 输入触发的单轮推理最多调用工具次数（0=不限制）；仅作跑飞兜底，不拦参数不同的必需调用", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "max_qq_output_calls", Default: defaultMaxQQOutputCalls, Type: "int", DisplayName: "单轮 QQ 发送上限", Description: "单轮最多主动发送的不同消息条数（0=不限制）；参数不同的消息不视为重复", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "max_duplicate_qq_send", Default: defaultMaxDuplicateSend, Type: "int", DisplayName: "单轮相同 QQ 发送上限", Description: "单轮内参数完全相同的 output_send__qq 允许重复的次数（0=不限制）；这才是循环保险的真正触发条件", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "dm_policy", Default: "open", Type: "string", DisplayName: "私聊策略", Description: "open / allowlist / disabled", Category: "qq", Options: []string{"open", "allowlist", "disabled"}})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "allow_from", Default: "", Type: "string", DisplayName: "私聊白名单", Description: "允许私聊机器人的 QQ 号列表，逗号分隔", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "group_policy", Default: "open", Type: "string", DisplayName: "群聊策略", Description: "open / allowlist / disabled", Category: "qq", Options: []string{"open", "allowlist", "disabled"}})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "group_allow_from", Default: "", Type: "string", DisplayName: "群聊白名单", Description: "允许接入的群号列表，逗号分隔", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "forward_rules", Default: "[]", Type: "string", DisplayName: "转发规则", Description: "JSON 数组，每项 {group_id,host,port,password,template}。匹配的群消息通过 RCON 转发到 Minecraft。template 支持 {nickname} {message} 占位", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "files_dir", Default: "/home/newqqagent/agentfs/merged/qq_files", Type: "string", DisplayName: "文件存储目录", Description: "从QQ接收的文件保存目录（CQ file/image 自动下载到此目录）", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "remote_dir", Default: "/home/program/qq-workspace/remote", Type: "string", DisplayName: "NapCat容器共享目录", Description: "与NapCat容器共享的文件目录，主机路径。发文件时文件会复制到此目录，NapCat内部映射为/app/files/", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "webhook_token", Default: "", Type: "string", DisplayName: "Webhook 令牌", Description: "NapCat 上报请求头 X-Webhook-Token 校验值，留空则不校验", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "agentfs_dir", Default: "/home/newqqagent/agentfs/merged", Type: "string", DisplayName: "AgentFS目录", Description: "文件读写的工作目录，read_document/video_download 等工具的默认工作目录", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "batch_window_ms", Default: "1500", Type: "int", DisplayName: "消息合并窗口(毫秒)", Description: "同一会话同一发送者的连续消息在该窗口内合并成一次中断并告知共几条；0=关闭合并（逐条投递）", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "batch_max_ms", Default: "30000", Type: "int", DisplayName: "消息合并上限(毫秒)", Description: "一批消息最长等这么久就投递，避免对方持续刷屏时一直不唤醒 Agent", Category: "qq"})

	settings := s.Settings()

	p.listenAddr = getSetting[string](settings, "listen", "0.0.0.0:25580")
	p.webhookToken = getSetting[string](settings, "webhook_token", "")
	p.napcatURL = strings.TrimRight(getSetting[string](settings, "napcat_url", "http://127.0.0.1:3000"), "/")
	ownerRaw := getSetting[string](settings, "owner", "")
	if strings.TrimSpace(ownerRaw) == "" {
		ownerRaw = getSetting[string](settings, "admin", "")
	}
	p.ownerIDs = parseIDList(ownerRaw)
	p.groupToolAllowlists = parseGroupToolAllowlists(getSetting[string](settings, "group_tool_allowlists", defaultGroupToolAllowlists))
	p.privateToolAllowlist = parseToolAllowlist(getSetting[string](settings, "private_tool_allowlist", defaultPublicToolAllowlist))
	p.maxQQToolCalls = nonNegativeOrDefault(int(getSetting[int64](settings, "max_qq_tool_calls", int64(defaultMaxQQToolCalls))), defaultMaxQQToolCalls)
	p.maxQQOutputCalls = nonNegativeOrDefault(int(getSetting[int64](settings, "max_qq_output_calls", int64(defaultMaxQQOutputCalls))), defaultMaxQQOutputCalls)
	p.maxDuplicateSend = nonNegativeOrDefault(int(getSetting[int64](settings, "max_duplicate_qq_send", int64(defaultMaxDuplicateSend))), defaultMaxDuplicateSend)
	p.dmPolicy = normalizePolicy(getSetting[string](settings, "dm_policy", "open"))
	p.groupPolicy = normalizePolicy(getSetting[string](settings, "group_policy", "open"))
	p.allowFrom = parseIDSet(getSetting[string](settings, "allow_from", ""))
	p.groupAllowFrom = parseIDSet(getSetting[string](settings, "group_allow_from", ""))
	p.filesDir = strings.TrimRight(getSetting[string](settings, "files_dir", "/home/newqqagent/agentfs/merged/qq_files"), "/")
	p.agentfsDir = strings.TrimRight(getSetting[string](settings, "agentfs_dir", "/home/newqqagent/agentfs/merged"), "/")
	p.remoteDir = strings.TrimRight(getSetting[string](settings, "remote_dir", "/home/program/qq-workspace/remote"), "/")
	p.batchWindow = time.Duration(getSetting[int64](settings, "batch_window_ms", 1500)) * time.Millisecond
	p.batchMax = time.Duration(getSetting[int64](settings, "batch_max_ms", 30000)) * time.Millisecond
	if p.batchWindow < 0 {
		p.batchWindow = 0
	}
	os.MkdirAll(p.remoteDir, 0755)

	p.httpClient = &http.Client{Timeout: 30 * time.Second}

	// msg_id → peer 映射 + 会话状态（不缓存正文）
	p.msgMap = make(map[int64]msgRef)
	p.batches = make(map[string]*pendingBatch)
	p.chats = make(map[int64]*chatMeta)

	// 从 NapCat 获取 Bot 身份（阻塞等待，最多 5s）
	p.fetchBotInfo()
	if p.botID == 0 {
		log.Printf("[qq] warning: 获取 Bot 身份失败，群 @ 检查将拒绝所有未提及消息")
	}

	tp := p.name + "_"

	botInfo := ""
	if p.botNickname != "" {
		botInfo = fmt.Sprintf("你的QQ昵称是%s", p.botNickname)
		if p.botID > 0 {
			botInfo += fmt.Sprintf("，QQ号是%d", p.botID)
		}
		botInfo += "。"
	}

	// ---- 注册输出通道 ----
	s.RegisterOutputChannel("qq", sdk.CapText|sdk.CapFile|sdk.CapImage|sdk.CapAudio,
		`发送QQ群聊/私聊消息，支持文字、语音、图片、文件。
meta JSON 格式：
{
  "group_id": 123456,       // 群号
  "user_id": 123456,        // QQ号
  "reply_to": 12345         // 可选，回复指定消息 ID
}
路由规则：仅 group_id 发群；仅 user_id 发私聊；两者同时存在时发到 group_id，并在消息头 @user_id。
type 枚举: text（文字）/ voice（语音转文字后发送）/ image（图片URL）/ file（文件URL）`,
		sdk.ChannelDef{}, p.handleChannelOutput)

	// ---- 注册输入通道（记忆计算层行为） ----
	inputCleaner := func(text string) string {
		// 从中断模板中提取语义内容：消息来源和发送者昵称
		// 模板: 来自「昵称」的私聊/群聊消息(message_id=N)...
		// 模板: 【重要！老大消息】来自「昵称」...
		cleaned := text
		// 去掉模板前缀
		if strings.HasPrefix(cleaned, "【重要！老大消息】") {
			cleaned = strings.TrimPrefix(cleaned, "【重要！老大消息】")
		}
		// 提取 "来自「XXX」" 中的昵称作为关键词
		if start := strings.Index(cleaned, "来自「"); start >= 0 {
			if end := strings.Index(cleaned[start:], "」"); end >= 0 {
				nick := cleaned[start+len("来自「") : start+end]
				cleaned = nick
			}
		}
		return cleaned
	}
	s.RegisterInputChannel("qq", sdk.ChannelDef{NoMemory: true, Cleaner: inputCleaner})

	// 查询类工具输出清洗器：提取 JSON 中的 content/文本字段参与向量化
	cleaner := func(output string) string {
		var r struct{ Content string }
		if json.Unmarshal([]byte(output), &r) == nil && r.Content != "" {
			return r.Content
		}
		return output
	}

	// ---- 消息 ----
	p.regTool(s, sdk.ToolDef{
		Name: tp + "get_message", Description: botInfo + "通过 message_id 从 NapCat 实时获取消息正文、发送者、附件等信息。message_id 从中断消息的 message_id=N 获取，或从 reply_to 的 message_id 获取。",
		NoMemory: false,
		Cleaner:  cleaner,
		// 消息正文只在当轮需要（决策怎么回复）；用完即裁剪。
		// 不裁的后果是每条 QQ 消息的完整正文都留在 L0 上下文里，
		// 长会话下持续挤占 token 预算（§13.8）。
		ContextPolicy: "prune",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"message_id": map[string]interface{}{"type": "integer", "description": "NapCat消息ID（从中断消息的 message_id=N 或 reply_to.message_id 获取）"},
			}, "required": []string{"message_id"},
		},
	}, p.handleGetMessage)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "send_file", Description: "发送文件/图片到QQ（私聊或群聊）。文件先复制到remote目录供NapCat容器访问。",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"group_id": map[string]interface{}{"type": "integer", "description": "目标群号（与user_id二选一）"},
				"user_id":  map[string]interface{}{"type": "integer", "description": "目标QQ号（与group_id二选一）"},
				"file":     map[string]interface{}{"type": "string", "description": "本地文件路径"},
				"name":     map[string]interface{}{"type": "string", "description": "文件名（可选，默认取原文件名）"},
				"as_image": map[string]interface{}{"type": "boolean", "description": "作为图片发送（true）还是作为文件（false，默认）"},
			},
		},
		NoMemory: true,
	}, p.handleSendFile)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "get_history", Description: "获取QQ群聊/私聊最近历史消息。当收到引用回复消息或需要了解对话上下文时应优先调用此工具查看前后文。返回值每条格式为 [时间] 发送者: 消息内容。如果消息包含文件，会额外返回 files 字段（含 file_id 和 name），可用 qq_download_file 工具下载。",
		NoMemory: false,
		Cleaner:  cleaner,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"group_id": map[string]interface{}{"type": "integer", "description": "群号（与user_id二选一）"},
				"user_id":  map[string]interface{}{"type": "integer", "description": "QQ号私聊历史（与group_id二选一）"},
				"count":    map[string]interface{}{"type": "integer", "description": "拉取条数，默认10"},
			}, "required": []string{},
		},
	}, p.handleGetHistory)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "list_chats", Description: "获取QQ会话列表，与真人客户端一致：按最新消息先后排序，每条标注会话（群/私聊）、会话名、未读消息数、最新一条消息摘要与时间。用于发现有未读消息的会话，再配合 qq_get_history 拉取对应会话内容、output_send__qq 回复。",
		NoMemory: false,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"count": map[string]interface{}{"type": "integer", "description": "最多返回会话数，默认10"},
			}, "required": []string{},
		},
	}, p.handleListChats)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "mark_read", Description: "将某个会话的未读计数清零（对象：群聊传 group_id，私聊传 user_id）。处理完某会话消息后可调用，让 list_chats 的未读数回到0，与真人客户端标记已读一致。",
		NoMemory: false,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"group_id": map[string]interface{}{"type": "integer", "description": "群号（与user_id二选一）"},
				"user_id":  map[string]interface{}{"type": "integer", "description": "QQ号（与group_id二选一）"},
			}, "required": []string{},
		},
	}, p.handleMarkRead)

	// ---- 查询 ----
	p.regTool(s, sdk.ToolDef{
		Name: tp + "get_groups", Description: "获取QQ群列表，可按关键词搜索群名",
		NoMemory: false,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"keyword": map[string]interface{}{"type": "string", "description": "搜索关键词（可选）"},
			},
		},
	}, p.handleGetGroups)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "get_friends", Description: "获取QQ好友列表，可按昵称/备注关键词搜索",
		NoMemory: false,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"keyword": map[string]interface{}{"type": "string", "description": "搜索关键词（可选）"},
			},
		},
	}, p.handleGetFriends)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "get_recent_contacts", Description: "查看最近有消息的联系人和群聊，返回最近消息概览（含消息数、最后一条消息内容）。可用于发现有谁发过消息但未处理。",
		NoMemory: false,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"count": map[string]interface{}{"type": "integer", "description": "获取数量，默认10"},
			},
		},
	}, p.handleGetRecentContacts)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "resolve_name", Description: "将QQ号或群号解析为可读的用户昵称或群名称",
		NoMemory: false,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"user_id":  map[string]interface{}{"type": "integer", "description": "QQ号（与group_id二选一）"},
				"group_id": map[string]interface{}{"type": "integer", "description": "群号（与user_id二选一）"},
			},
		},
	}, p.handleResolveName)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "resolve_nickname", Description: "按昵称/备注/群名片搜索QQ用户，返回匹配的QQ号和详细信息。支持搜索好友列表或指定群成员。",
		NoMemory: false,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"keyword":  map[string]interface{}{"type": "string", "description": "搜索关键词（昵称/备注/群名片）"},
				"group_id": map[string]interface{}{"type": "integer", "description": "所在群号（可选），不传则搜索好友列表"},
			}, "required": []string{"keyword"},
		},
	}, p.handleResolveNickname)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "get_group_member_info", Description: "获取QQ群成员详细信息",
		NoMemory: false,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"group_id": map[string]interface{}{"type": "integer", "description": "群号"},
				"user_id":  map[string]interface{}{"type": "integer", "description": "QQ号"},
			}, "required": []string{"group_id", "user_id"},
		},
	}, p.handleGetGroupMemberInfo)

	// ---- 群管理 ----
	p.regTool(s, sdk.ToolDef{
		Name: tp + "group_manage", Description: "QQ群综合管理。通过command参数执行各种操作：leave退群, kick踢人, ban禁言, unban解禁, rename改名, mute-all全员禁言, set-card设名片, set-admin设管理, set-title设头衔, member-list成员列表, group-info群详情, member-info成员详情, at-all-remain@全体剩余, msg-history消息历史, recall撤回, pin-msg精华, list-files文件列表, pending-requests待处理请求, folder-create创建文件夹。注意：leave/kick/ban/unban/mute-all/set-admin等破坏性操作必须先请示管理员确认后再执行。",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command":    map[string]interface{}{"type": "string", "description": "操作命令"},
				"group_id":   map[string]interface{}{"type": "integer", "description": "群号"},
				"user_id":    map[string]interface{}{"type": "integer", "description": "QQ号（踢人/禁言/设名片等需要）"},
				"message_id": map[string]interface{}{"type": "integer", "description": "消息ID（撤回/精华）"},
				"name":       map[string]interface{}{"type": "string", "description": "群名称（rename）或文件夹名（folder-create）"},
				"card":       map[string]interface{}{"type": "string", "description": "群名片（set-card）"},
				"title":      map[string]interface{}{"type": "string", "description": "群头衔（set-title）"},
				"enable":     map[string]interface{}{"type": "boolean", "description": "启用/禁用（set-admin/mute-all）"},
				"minutes":    map[string]interface{}{"type": "integer", "description": "禁言分钟数（ban），0=解禁"},
				"count":      map[string]interface{}{"type": "integer", "description": "消息条数（msg-history），默认10"},
				"folder_id":  map[string]interface{}{"type": "string", "description": "文件夹ID（list-files）"},
				"reject_add": map[string]interface{}{"type": "boolean", "description": "踢出时拒绝加群（kick）"},
				"confirm":    map[string]interface{}{"type": "boolean", "description": "高风险操作确认标记。执行 leave/kick/ban/unban/rename/mute-all/set-card/set-admin/set-title/recall/pin-msg/folder-create 时必须传 true"},
			},
		},
		NoMemory: true,
	}, p.handleGroupManage)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "friend_action", Description: "QQ好友管理：delete删除好友, block拉黑（删好友+从所有群踢出+拒绝加群）, approve-friend同意好友请求, reject-friend拒绝好友请求, list-friends列出好友。注意：涉及删除/拉黑的操作必须请示管理员确认后再执行，未经授权不可操作。",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"command":  map[string]interface{}{"type": "string", "description": "操作: delete|block|approve-friend|reject-friend|list-friends"},
				"user_id":  map[string]interface{}{"type": "integer", "description": "目标QQ号"},
				"flag":     map[string]interface{}{"type": "string", "description": "好友请求flag（approve-friend/reject-friend需要）"},
				"remark":   map[string]interface{}{"type": "string", "description": "好友备注（approve-friend可选）"},
				"group_id": map[string]interface{}{"type": "integer", "description": "仅从指定群踢出（block配合）"},
				"confirm":  map[string]interface{}{"type": "boolean", "description": "高风险操作确认标记。执行 delete/block/approve-friend/reject-friend 时必须传 true"},
			},
		},
		NoMemory: true,
	}, p.handleFriendAction)

	// ---- 文件 ----
	p.regTool(s, sdk.ToolDef{
		Name: tp + "get_group_files", Description: "查询群文件列表、搜索文件、下载文件到本地。操作: list列出, search搜索, download下载",
		NoMemory: false,
		Cleaner:  cleaner,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"group_id":  map[string]interface{}{"type": "integer", "description": "群号"},
				"command":   map[string]interface{}{"type": "string", "description": "操作: list|search|download"},
				"folder_id": map[string]interface{}{"type": "string", "description": "文件夹ID（list指定文件夹）"},
				"keyword":   map[string]interface{}{"type": "string", "description": "搜索关键词（search）"},
				"file_id":   map[string]interface{}{"type": "string", "description": "文件ID（download）"},
				"filename":  map[string]interface{}{"type": "string", "description": "保存文件名（download可选）"},
			},
		},
	}, p.handleGetGroupFiles)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "download_file", Description: "从聊天记录下载文件到本地。file_id 从 qq_get_message/qq_get_history 的 files 字段获取。群文件建议提供 group_id，私聊文件建议提供 user_id 以提高成功率。异步下载，完成后推送通知。",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"file_id":  map[string]interface{}{"type": "string", "description": "文件 ID（从 qq_get_message 或 qq_get_history 的 files 字段获取）"},
				"url":      map[string]interface{}{"type": "string", "description": "文件下载 URL（可选，qq_get_message 返回的 url 字段）"},
				"group_id": map[string]interface{}{"type": "integer", "description": "群号（可选，群文件下载）"},
				"user_id":  map[string]interface{}{"type": "integer", "description": "私聊对象QQ号（可选，私聊文件下载）"},
				"filename": map[string]interface{}{"type": "string", "description": "保存文件名（可选，默认用原文件名）"},
			}, "required": []string{"file_id"},
		},
		NoMemory: true,
	}, p.handleDownloadFile)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "get_download_tasks", Description: "查看所有下载任务及状态（running/done/failed），包含文件名、保存路径、错误信息等",
		NoMemory: false,
		Cleaner:  cleaner,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{},
		},
	}, p.handleGetDownloadTasks)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "upload_group_file", Description: "上传文件到QQ群（通过base64编码发送，同时出现在群消息和群文件柜）",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"group_id": map[string]interface{}{"type": "integer", "description": "目标群号"},
				"file":     map[string]interface{}{"type": "string", "description": "本地文件路径"},
				"name":     map[string]interface{}{"type": "string", "description": "文件名（可选，默认取原文件名）"},
			}, "required": []string{"group_id", "file"},
		},
		NoMemory: true,
	}, p.handleUploadGroupFile)

	// ---- 文档/视频/网页工具 ----
	p.regTool(s, sdk.ToolDef{
		Name: tp + "read_document", Description: "读取文档内容文本。支持 PDF、DOCX、DOC、XLSX、XLS、PPTX、PPT、TXT、CSV、MD 格式。使用 libreoffice + pandoc 转换提取文本，返回前 20000 字符。适合处理用户发来的文档文件。",
		NoMemory: false,
		Cleaner:  cleaner,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"path": map[string]interface{}{"type": "string", "description": "文档文件路径（已保存到本地的文件路径）"},
			}, "required": []string{"path"},
		},
	}, p.handleReadDocument)

	p.regTool(s, sdk.ToolDef{
		Name: tp + "video_download", Description: "下载视频到本地。支持 B站、YouTube 等主流视频网站（通过 yt-dlp）。先调用 info_only 查看视频信息，再下载。下载后文件保存在 agentfs 目录。",
		NoMemory: false,
		Cleaner:  cleaner,
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"url":       map[string]interface{}{"type": "string", "description": "视频分享链接"},
				"info_only": map[string]interface{}{"type": "boolean", "description": "仅获取视频信息（标题、时长、清晰度列表），不下"},
			}, "required": []string{"url"},
		},
	}, p.handleVideoDownload)

	// ---- 附加 ----
	p.regTool(s, sdk.ToolDef{
		Name: tp + "send_like", Description: "给QQ好友点赞/戳一戳",
		Parameters: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"user_id": map[string]interface{}{"type": "integer", "description": "目标QQ号"},
				"times":   map[string]interface{}{"type": "integer", "description": "点赞次数1-20，默认1"},
			}, "required": []string{"user_id"},
		},
		NoMemory: true,
	}, p.handleSendLike)

	// 全局权限门：只有 QQ 当前输入需要受此插件约束；Bot 所有者始终完整放行。
	s.RegisterStage(sdk.StageOnInput, p.onInputAuthContext, sdk.StageScopeGlobal)
	s.RegisterStage(sdk.StageBeforeToolcall, p.beforeToolcall, sdk.StageScopeGlobal)
	// before_toolcall 的 Response 只用于拒绝当前工具。下一次模型补全到达时必须清掉，
	// 否则内核会把它误当作“结束整个推理”的最终响应。
	s.RegisterStage(sdk.StagePostAction, p.clearDeniedResponse, sdk.StageScopeGlobal)
	s.RegisterStage(sdk.StageAfterOutput, p.afterOutputAuthContext, sdk.StageScopeGlobal)

	// ---- HTTP server for NapCat webhook ----
	mux := http.NewServeMux()
	mux.HandleFunc("/", p.handleWebhook)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	})
	p.srv = &http.Server{Addr: p.listenAddr, Handler: mux}
	go func() {
		log.Printf("[qq] webhook %s napcat=%s", p.listenAddr, p.napcatURL)
		if err := p.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[qq] http: %v", err)
		}
	}()

	log.Printf("[qq] plugin started: %s (%d tools)", p.name, 15)
	return nil
}

func (p *Plugin) Stop() error {
	p.typingMu.Lock()
	for _, st := range p.typingMap {
		select {
		case <-st.stopCh:
		default:
			close(st.stopCh)
		}
	}
	p.typingMu.Unlock()
	// 停机前把未到点的合并批次立刻投出去，别把对方的消息吞掉。
	p.flushAllBatches()
	if p.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.srv.Shutdown(ctx)
	}
	return nil
}

func (p *Plugin) regTool(s *sdk.PluginSDK, def sdk.ToolDef, handler sdk.ToolHandler) {
	s.RegisterTool(def.Name, def, handler)
}

// ======== Bot Identity ========

func (p *Plugin) fetchBotInfo() {
	resp, err := p.rawNapcat("get_login_info", nil)
	if err != nil {
		log.Printf("[qq] fetch login info: %v", err)
		return
	}
	var info struct {
		Status string `json:"status"`
		Data   *struct {
			UserID   int64  `json:"user_id"`
			Nickname string `json:"nickname"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(resp), &info); err != nil {
		log.Printf("[qq] parse login info: %v", err)
		return
	}
	if info.Data != nil {
		p.botID = info.Data.UserID
		p.botNickname = info.Data.Nickname
		log.Printf("[qq] bot identity: %s (%d)", p.botNickname, p.botID)
	}
}

// rawNapcat sends a request to NapCat and returns raw JSON string.
func (p *Plugin) rawNapcat(action string, params map[string]interface{}) (string, error) {
	data, _ := json.Marshal(params)
	url := fmt.Sprintf("%s/%s", p.napcatURL, action)
	resp, err := p.httpClient.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("napcat %s: %w", action, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body), nil
}

// getSetting reads a setting from the SDK; returns fallback if unset or wrong type.
func getSetting[T string | int64 | float64](s sdk.SettingsAPI, key string, fallback T) T {
	v, err := s.Get(key)
	if err != nil || v == nil {
		return fallback
	}
	switch any(fallback).(type) {
	case string:
		if str, ok := v.(string); ok {
			return any(str).(T)
		}
	case int64:
		switch val := v.(type) {
		case int:
			return any(int64(val)).(T)
		case int64:
			return any(val).(T)
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

func normalizePolicy(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "allowlist":
		return "allowlist"
	case "disabled":
		return "disabled"
	default:
		return "open"
	}
}

func parseIDSet(raw string) map[int64]struct{} {
	out := make(map[int64]struct{})
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if n, err := strconv.ParseInt(part, 10, 64); err == nil {
			out[n] = struct{}{}
		}
	}
	return out
}

func parseIDList(raw string) []int64 {
	var out []int64
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if n, err := strconv.ParseInt(part, 10, 64); err == nil && n > 0 {
			out = append(out, n)
			continue
		}
		// 兼容历史坏数据：科学计数法存库的值（如 2.198972886e+09）
		if f, err := strconv.ParseFloat(part, 64); err == nil && f > 0 && f == math.Trunc(f) {
			out = append(out, int64(f))
		}
	}
	return out
}

func (p *Plugin) isOwner(userID int64) bool {
	for _, id := range p.ownerIDs {
		if id == userID {
			return true
		}
	}
	return false
}

func parseToolAllowlist(raw string) []string {
	var patterns []string
	if json.Unmarshal([]byte(raw), &patterns) != nil {
		return nil
	}
	out := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		if pattern = strings.TrimSpace(pattern); pattern != "" {
			out = append(out, pattern)
		}
	}
	return out
}

func parseGroupToolAllowlists(raw string) map[int64][]string {
	var encoded map[string][]string
	if json.Unmarshal([]byte(raw), &encoded) != nil {
		return map[int64][]string{}
	}
	out := make(map[int64][]string, len(encoded))
	for key, patterns := range encoded {
		var groupID int64
		if key != "*" {
			parsed, err := strconv.ParseInt(strings.TrimSpace(key), 10, 64)
			if err != nil || parsed <= 0 {
				continue
			}
			groupID = parsed
		}
		clean := make([]string, 0, len(patterns))
		for _, pattern := range patterns {
			if pattern = strings.TrimSpace(pattern); pattern != "" {
				clean = append(clean, pattern)
			}
		}
		out[groupID] = clean
	}
	return out
}

func matchesToolAllowlist(name string, patterns []string) bool {
	for _, pattern := range patterns {
		if pattern == name {
			return true
		}
		if strings.HasSuffix(pattern, "*") && strings.HasPrefix(name, strings.TrimSuffix(pattern, "*")) {
			return true
		}
	}
	return false
}

// nonNegativeOrDefault 保留 0（表示“不限制”），仅把负数纠正为默认值。
func nonNegativeOrDefault(value, fallback int) int {
	if value < 0 {
		return fallback
	}
	return value
}

// isHardPrivateTool 是不可由群/私聊白名单覆盖的私人资源边界。
// output_send__qq 及 QQ 当前会话工具在参数级另行约束，不在此处按名称误杀。
func isHardPrivateTool(name string) bool {
	for _, prefix := range []string{
		"calendar_", "email_", "mail_", "agentmail_", "memory_", "knowledge_",
		"device_", "devicectl_", "terminal_", "shell_", "command_", "exec_",
		"filesystem_", "agentfs_", "config_", "settings_", "plugin_", "plugins_",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return matchesToolAllowlist(name, []string{
		"read_file", "write_file", "edit_file", "delete_file", "list_files", "run_command",
		"homeagent_config", "homeagent_restart", "output_send__email", "output_send__mail",
	})
}

func argInt64(args map[string]interface{}, key string) (int64, bool) {
	value, exists := args[key]
	if !exists || value == nil {
		return 0, false
	}
	parsed, err := convInt64(value)
	return parsed, err == nil && parsed != 0
}

func (p *Plugin) sessionToolArgsAllowed(name string, args map[string]interface{}, auth qqAuthContext) (bool, string) {
	if !auth.active || auth.owner {
		return true, ""
	}
	// 输出工具**不受"当前会话"身份限制**（先于身份判据返回）。
	//
	// 为什么：输出是 agent 的**主动调用**，发到哪个会话由它自己给的 meta
	// （group_id / user_id）决定 —— handleChannelOutput 会强制要求该字段存在，
	// 缺了会得到明确的报错。这里再要求"本轮能精确匹配可信 OneBot 事件"是多余的门，
	// 而且会把合法发送一起拒掉：现场（被子的中断唤醒的一轮）父带齐 meta 也发不出去，
	// 报「可信 QQ 会话身份不完整」。
	// 「只能访问当前会话」这类限制只对**读取类**工具（get_history / mark_read /
	// get_message）成立 —— 那才是真的不能跨会话读。
	if name == "output_send__"+p.name {
		return true, ""
	}

	currentPeer := auth.userID
	if auth.isGroup {
		currentPeer = auth.groupID
	}
	if currentPeer == 0 {
		return false, "可信 QQ 会话身份不完整"
	}
	matchCurrentPeer := func() bool {
		groupID, hasGroup := argInt64(args, "group_id")
		userID, hasUser := argInt64(args, "user_id")
		if auth.isGroup {
			return hasGroup && groupID == auth.groupID && !hasUser
		}
		return hasUser && userID == auth.userID && !hasGroup
	}

	switch name {
	case p.name + "_get_history", p.name + "_mark_read":
		if !matchCurrentPeer() {
			return false, "只能访问当前 QQ 会话"
		}
	case p.name + "_get_message":
		messageID, ok := argInt64(args, "message_id")
		if !ok {
			return false, "缺少有效 message_id"
		}
		if messageID == auth.messageID {
			return true, ""
		}
		peerID, isGroup, _, found := p.lookupMsgRef(messageID)
		if !found || isGroup != auth.isGroup || peerID != currentPeer {
			return false, "message_id 不属于当前 QQ 会话"
		}
	case p.name + "_get_group_member_info", p.name + "_get_group_files":
		groupID, ok := argInt64(args, "group_id")
		if !auth.isGroup || !ok || groupID != auth.groupID {
			return false, "只能访问当前 QQ 群的数据"
		}
	}
	return true, ""
}

// activateAuthContext 只接收 OneBot 事件中的可信 ID。多个中断在同一推理轮合并时
// 采用最小权限合并，防止“非所有者请求 + 随后所有者消息”意外提升前一请求权限。
// message_id 映射供排队输入在 StageOnInput 精确恢复身份，不依赖昵称或用户正文。
func (p *Plugin) activateAuthContext(messageID, userID, groupID int64, isGroup bool) {
	p.authMu.Lock()
	defer p.authMu.Unlock()
	if p.authByMessageID == nil {
		p.authByMessageID = make(map[int64]qqAuthContext)
	}
	p.authGeneration++
	next := qqAuthContext{
		active: true, owner: p.isOwner(userID), messageID: messageID, userID: userID,
		groupID: groupID, isGroup: isGroup, generation: p.authGeneration,
	}
	if messageID != 0 {
		p.authByMessageID[messageID] = next
		if len(p.authByMessageID) > 2048 {
			cutoff := p.authGeneration - 1024
			for id, auth := range p.authByMessageID {
				if auth.generation < cutoff {
					delete(p.authByMessageID, id)
				}
			}
		}
	}
	if !p.auth.active {
		p.auth = next
		return
	}
	if p.auth.userID == userID && p.auth.groupID == groupID && p.auth.isGroup == isGroup {
		p.auth.owner = p.auth.owner && next.owner
		p.auth.messageID = next.messageID
		p.auth.generation = next.generation
		return
	}
	// 不同可信来源被内核合并到同一推理时，只在双方都是所有者时保留完整权限。
	bothOwners := p.auth.owner && next.owner
	sameGroup := p.auth.isGroup && next.isGroup && p.auth.groupID == groupID
	p.auth.owner = bothOwners
	p.auth.messageID = 0
	p.auth.userID = 0
	p.auth.isGroup = sameGroup
	if sameGroup {
		p.auth.groupID = groupID
	} else {
		p.auth.groupID = 0
	}
	p.auth.generation = next.generation
}

func messageIDFromInput(raw string) int64 {
	match := qqMessageIDRe.FindStringSubmatch(raw)
	if len(match) != 2 {
		return 0
	}
	id, _ := strconv.ParseInt(match[1], 10, 64)
	return id
}

func (p *Plugin) onInputAuthContext(ctx *sdk.StageContext) error {
	ctx.RLock()
	source, _ := ctx.Extra["input_source"].(string)
	raw := ctx.RawMessage
	ctx.RUnlock()
	p.authMu.Lock()
	defer p.authMu.Unlock()
	if source != p.name {
		p.auth = qqAuthContext{}
		p.resetTurnGuardLocked()
		return nil
	}
	if messageID := messageIDFromInput(raw); messageID != 0 {
		if auth, ok := p.authByMessageID[messageID]; ok {
			p.auth = auth
			delete(p.authByMessageID, messageID)
			p.resetTurnGuardLocked()
			return nil
		}
	}
	// QQ 来源却无法精确匹配可信 OneBot 事件时必须强制降权，不能复用上一条消息的身份。
	p.auth = qqAuthContext{active: true}
	p.resetTurnGuardLocked()
	return nil
}

func (p *Plugin) resetTurnGuardLocked() {
	p.lastDenial = ""
	p.denialLocked = false
	p.toolCallCount = 0
	p.outputCallCount = 0
	p.outputSignatures = make(map[string]int)
}

func (p *Plugin) afterOutputAuthContext(ctx *sdk.StageContext) error {
	p.authMu.Lock()
	p.auth = qqAuthContext{}
	p.resetTurnGuardLocked()
	p.authMu.Unlock()
	return nil
}

func (p *Plugin) currentToolAllowed(name string) (bool, qqAuthContext) {
	p.authMu.RLock()
	auth := p.auth
	var patterns []string
	if auth.active && !auth.owner {
		if auth.isGroup {
			patterns, _ = p.groupToolAllowlists[auth.groupID]
			if patterns == nil {
				patterns = p.groupToolAllowlists[0]
			}
		} else {
			patterns = p.privateToolAllowlist
		}
	}
	p.authMu.RUnlock()
	if !auth.active || auth.owner {
		return true, auth
	}
	return matchesToolAllowlist(name, patterns), auth
}

// isAtBot checks if the message contains an @-mention of the bot.
func (p *Plugin) isAtBot(msg interface{}) bool {
	segments, ok := msg.([]interface{})
	if !ok {
		return false
	}
	botIDStr := strconv.FormatInt(p.botID, 10)
	for _, seg := range segments {
		s, ok := seg.(map[string]interface{})
		if !ok {
			continue
		}
		if s["type"] == "at" {
			if data, ok := s["data"].(map[string]interface{}); ok {
				if qq, ok := data["qq"]; ok {
					switch v := qq.(type) {
					case string:
						if v == botIDStr || v == "all" {
							return true
						}
					case float64:
						if int64(v) == p.botID {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// ======== Webhook ========

func (p *Plugin) isDMAllowed(userID int64) bool {
	switch p.dmPolicy {
	case "disabled":
		return false
	case "allowlist":
		_, ok := p.allowFrom[userID]
		return ok
	default:
		return true
	}
}

func (p *Plugin) isGroupAllowed(groupID int64) bool {
	switch p.groupPolicy {
	case "disabled":
		return false
	case "allowlist":
		_, ok := p.groupAllowFrom[groupID]
		return ok
	default:
		return true
	}
}

func (p *Plugin) setDenial(ctx *sdk.StageContext, message string) {
	ctx.Response = &message
	p.authMu.Lock()
	p.lastDenial = message
	p.authMu.Unlock()
}

func (p *Plugin) clearPreviousDenial(ctx *sdk.StageContext) {
	p.authMu.Lock()
	last := p.lastDenial
	p.lastDenial = ""
	p.authMu.Unlock()
	if last != "" && ctx.Response != nil && *ctx.Response == last {
		ctx.Response = nil
	}
}

func (p *Plugin) clearDeniedResponse(ctx *sdk.StageContext) error {
	ctx.Lock()
	defer ctx.Unlock()
	p.clearPreviousDenial(ctx)
	return nil
}

func (p *Plugin) beforeToolcall(ctx *sdk.StageContext) error {
	ctx.Lock()
	defer ctx.Unlock()
	p.clearPreviousDenial(ctx)
	if len(ctx.ToolCalls) == 0 {
		return nil
	}
	tc := &ctx.ToolCalls[0]
	allowed, auth := p.currentToolAllowed(tc.Name)
	if !auth.active {
		return nil
	}

	p.authMu.Lock()
	p.toolCallCount++
	toolCount := p.toolCallCount
	denialLocked := p.denialLocked
	if tc.Name == "output_send__"+p.name {
		p.outputCallCount++
		signatureBytes, _ := json.Marshal(tc.Arguments)
		signature := string(signatureBytes)
		p.outputSignatures[signature]++
		duplicateCount := p.outputSignatures[signature]
		distinctCount := len(p.outputSignatures)
		// 循环保险只拦“参数完全相同的重复调用”。参数不同的必需调用一律放行，
		// 否则多次 cmd_run / update_schedule / 多条不同消息都会被误杀。
		if p.maxDuplicateSend > 0 && duplicateCount > p.maxDuplicateSend {
			p.authMu.Unlock()
			msg := fmt.Sprintf("QQ 循环保险已阻止重复发送：本轮第 %d 次出现参数完全相同的消息；请勿重复发送同一内容", duplicateCount)
			p.setDenial(ctx, msg)
			return nil
		}
		if p.maxQQOutputCalls > 0 && distinctCount > p.maxQQOutputCalls {
			p.authMu.Unlock()
			msg := fmt.Sprintf("QQ 循环保险已阻止本次发送：单轮主动发送的不同消息数已达上限 %d（0=不限制，可在插件配置调整）", p.maxQQOutputCalls)
			p.setDenial(ctx, msg)
			return nil
		}
	}
	if p.maxQQToolCalls > 0 && toolCount > p.maxQQToolCalls {
		p.authMu.Unlock()
		msg := fmt.Sprintf("QQ 循环保险已阻止工具调用：单轮工具调用总数已达上限 %d（0=不限制，可在插件配置调整）", p.maxQQToolCalls)
		p.setDenial(ctx, msg)
		return nil
	}
	p.authMu.Unlock()

	if denialLocked && tc.Name != "output_send__"+p.name {
		msg := fmt.Sprintf("QQ 权限策略已锁止本轮后续工具 %s；仅允许发送一次权限说明", tc.Name)
		p.setDenial(ctx, msg)
		return nil
	}
	if !auth.owner && isHardPrivateTool(tc.Name) {
		p.authMu.Lock()
		p.denialLocked = true
		p.authMu.Unlock()
		msg := fmt.Sprintf("QQ 权限策略拒绝私人资源工具 %s；该限制不能由群聊或私聊白名单覆盖，请不要重试", tc.Name)
		p.setDenial(ctx, msg)
		return nil
	}
	if !allowed {
		scope := "非所有者私聊"
		if auth.isGroup {
			scope = fmt.Sprintf("群聊 %d", auth.groupID)
		}
		msg := fmt.Sprintf("QQ 权限策略拒绝工具 %s：%s 的工具白名单未包含该工具；请不要重试，改为直接说明权限限制", tc.Name, scope)
		p.setDenial(ctx, msg)
		return nil
	}
	if argsAllowed, reason := p.sessionToolArgsAllowed(tc.Name, tc.Arguments, auth); !argsAllowed {
		msg := fmt.Sprintf("QQ 权限策略拒绝工具 %s：%s；请不要改用其他会话 ID 重试", tc.Name, reason)
		p.setDenial(ctx, msg)
		return nil
	}
	if tc.Name == p.name+"_group_manage" {
		cmd, _ := tc.Arguments["command"].(string)
		if requiresConfirmGroupCommand(cmd) {
			if ok, _ := tc.Arguments["confirm"].(bool); !ok {
				msg := fmt.Sprintf("QQ群管理命令 %s 属于高风险操作，必须显式传入 confirm=true 后才能执行", cmd)
				p.setDenial(ctx, msg)
				return nil
			}
		}
	}
	if tc.Name == p.name+"_friend_action" {
		cmd, _ := tc.Arguments["command"].(string)
		if requiresConfirmFriendCommand(cmd) {
			if ok, _ := tc.Arguments["confirm"].(bool); !ok {
				msg := fmt.Sprintf("QQ好友管理命令 %s 属于高风险操作，必须显式传入 confirm=true 后才能执行", cmd)
				p.setDenial(ctx, msg)
				return nil
			}
		}
	}
	return nil
}

func requiresConfirmGroupCommand(cmd string) bool {
	switch cmd {
	case "leave", "kick", "ban", "unban", "rename", "mute-all", "set-card", "set-admin", "set-title", "recall", "pin-msg", "folder-create":
		return true
	default:
		return false
	}
}

func requiresConfirmFriendCommand(cmd string) bool {
	switch cmd {
	case "delete", "block", "approve-friend", "reject-friend":
		return true
	default:
		return false
	}
}

// enqueueInterrupt 把一条已通过策略/@ 检查的消息并入待投批次，并重置 debounce 计时。
//
// batchWindow<=0 时退回逐条投递（合并前行为）。
func (p *Plugin) enqueueInterrupt(msgType string, userID, groupID, messageID int64, nickname, single string, owner, highRisk bool) {
	if p.sdk == nil && p.injectHook == nil {
		return
	}
	if p.batchWindow <= 0 {
		p.injectInterrupt(single, p.interruptLevel(owner))
		return
	}
	key := qqBatchKey(msgType, groupID, userID)
	p.batchMu.Lock()
	if p.batches == nil {
		p.batches = make(map[string]*pendingBatch)
	}
	b := p.batches[key]
	if b == nil {
		b = &pendingBatch{key: key, first: time.Now()}
		p.batches[key] = b
	}
	b.isGroup = msgType == "group"
	b.userID, b.groupID, b.nickname = userID, groupID, nickname
	b.msgIDs = append(b.msgIDs, messageID)
	b.single = single
	b.owner = b.owner || owner
	b.highRisk = b.highRisk || highRisk
	// debounce：每来一条就推迟；但整体不超过 batchMax（否则持续刷屏会一直不投）。
	delay := p.batchWindow
	if p.batchMax > 0 {
		if remain := p.batchMax - time.Since(b.first); remain < delay {
			delay = remain
		}
	}
	if delay < 0 {
		delay = 0
	}
	if b.timer != nil {
		b.timer.Stop()
	}
	b.timer = time.AfterFunc(delay, func() { p.flushBatch(key) })
	p.batchMu.Unlock()
}

// interruptLevel 决定一条 QQ 消息的中断级别。
//
//   - Bot 所有者/管理员的消息 → **L2**（一般提醒）；
//   - 其他人的消息 → L1（后台，完全可等）。
//
// 为什么不能一律 L1：L1 之间可以随时互相抢占、也可以被任何更高一级打断，
// 于是「老板发的话」会被路人的闲聊挤到后面，甚至对方持续刷屏时一直排在队尾。
// 为什么也不该给 L3：L3 是时钟/终端那类"需要及时处理"的实时工作，QQ 是异步
// 消息，抬到 L3 会反过来打断真正实时的事情。
func (p *Plugin) interruptLevel(owner bool) string {
	if owner {
		return sdk.PriorityL2
	}
	return sdk.PriorityL1
}

// injectInterrupt 投递一条中断提示（NoMemory：HTTP 侧来的不是对话内容）。
func (p *Plugin) injectInterrupt(text, level string) {
	if text == "" {
		return
	}
	if level == "" {
		level = sdk.PriorityL1
	}
	if p.injectHook != nil {
		p.injectHook(text, level)
		return
	}
	if p.sdk == nil {
		return
	}
	p.sdk.InjectInterruptTextOpts(p.name, p.name, text, sdk.InjectOptions{
		NoMemory: true,
		Priority: level,
	})
}

// flushBatch 投递一批：n==1 沿用单条原文；n>1 生成「共几条」的合并中断。
func (p *Plugin) flushBatch(key string) {
	p.batchMu.Lock()
	b := p.batches[key]
	delete(p.batches, key)
	p.batchMu.Unlock()
	if b == nil {
		return
	}
	text := b.single
	if len(b.msgIDs) > 1 {
		text = p.buildBatchInterrupt(b)
	}
	// 一批里只要有一条来自 Bot 所有者，整批按 L2 投递（不因混入路人消息而降低）。
	p.injectInterrupt(text, p.interruptLevel(b.owner))
}

// flushAllBatches 停机前把未到点的批次立刻投出去（best effort）。
func (p *Plugin) flushAllBatches() {
	p.batchMu.Lock()
	keys := make([]string, 0, len(p.batches))
	for k := range p.batches {
		keys = append(keys, k)
	}
	p.batchMu.Unlock()
	for _, k := range keys {
		p.flushBatch(k)
	}
}

// buildBatchInterrupt 生成合并中断：说清「一共几条」「分别是哪些 message_id」，
// 并给出一次拿全上下文的建议（get_history），避免模型逐条 get_message。
func (p *Plugin) buildBatchInterrupt(b *pendingBatch) string {
	tp := p.name + "_"
	outputTool := "output_send__" + p.name
	n := len(b.msgIDs)
	ids := formatMsgIDs(b.msgIDs)
	var s string
	if b.isGroup {
		s = fmt.Sprintf("来自「%s」在群里短时间内连续发来 %d 条消息(message_id=%s)。建议先用%sget_history(group_id=%d, count=%d)一次拉取这几条上下文再统一回复；也可用%sget_message 取单条。用%s回复群聊",
			b.nickname, n, ids, tp, b.groupID, n+5, tp, outputTool)
	} else {
		s = fmt.Sprintf("来自「%s」的私聊短时间内连续发来 %d 条消息(message_id=%s, user_id=%d)。建议先用%sget_history(user_id=%d, count=%d)一次拉取这几条上下文再统一回复；也可用%sget_message 取单条。用%s回复对方",
			b.nickname, n, ids, b.userID, tp, b.userID, n+5, tp, outputTool)
	}
	if b.highRisk {
		s = "【⚠️ 高危信息，谨慎处理】" + s
	}
	if b.owner {
		s = "【重要！Bot 所有者消息】" + s
	}
	return s
}

// formatMsgIDs 把 message_id 列表压成一行；过多时截断，避免中断文字过长。
func formatMsgIDs(ids []int64) string {
	const capN = 12
	parts := make([]string, 0, len(ids)+1)
	for i, id := range ids {
		if i >= capN {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
}

func (p *Plugin) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "", http.StatusMethodNotAllowed)
		return
	}
	if p.webhookToken != "" && !hmac.Equal([]byte(r.Header.Get("X-Webhook-Token")), []byte(p.webhookToken)) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var evt struct {
		PostType    string      `json:"post_type"`
		MessageType string      `json:"message_type,omitempty"`
		MessageID   int64       `json:"message_id"`
		UserID      int64       `json:"user_id,omitempty"`
		GroupID     int64       `json:"group_id,omitempty"`
		RawMessage  string      `json:"raw_message,omitempty"`
		Message     interface{} `json:"message,omitempty"`
		Time        int64       `json:"time"`
		Sender      *struct {
			Nickname string `json:"nickname"`
			Card     string `json:"card,omitempty"`
		} `json:"sender,omitempty"`
	}
	if json.Unmarshal(body, &evt) != nil || evt.PostType != "message" {
		w.WriteHeader(http.StatusOK)
		return
	}
	// 群消息到达即记录（用于排查 napcat→webhook 链路漏报/丢弃）
	if evt.MessageType == "group" {
		log.Printf("[qq] webhook recv group msg id=%d from=%d in=%d raw=%.100s",
			evt.MessageID, evt.UserID, evt.GroupID, evt.RawMessage)
	}

	rawCQ := evt.RawMessage
	text := rawCQ
	if text == "" {
		if s, ok := evt.Message.(string); ok {
			text = s
		}
	}
	nickname := ""
	if evt.Sender != nil {
		nickname = evt.Sender.Nickname
		if evt.Sender.Card != "" {
			nickname = evt.Sender.Card
		}
	}

	if evt.MessageType == "private" {
		if !p.isDMAllowed(evt.UserID) {
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	if evt.MessageType == "group" {
		if !p.isGroupAllowed(evt.GroupID) {
			log.Printf("[qq] group msg from %d rejected: policy=%s", evt.GroupID, p.groupPolicy)
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	// ---- 记录 msg_id→peer 映射与会话状态（不缓存正文，仅最新一条短摘要）----
	// 策略允许的消息（群/私聊、是否 @bot 均记），供 get_msg 兜底与 list_chats 使用；
	// @bot 与否只决定是否发中断，不影响记录——与真人客户端一致看到全部会话。
	{
		peerID, isGroup := evt.UserID, false
		if evt.MessageType == "group" {
			peerID, isGroup = evt.GroupID, true
		}
		sum := text
		runes := []rune(sum)
		if len(runes) > qqLastSumLen {
			sum = string(runes[:qqLastSumLen]) + "…"
		}
		if evt.Time == 0 {
			evt.Time = time.Now().Unix()
		}
		p.snapshotMsg(evt.MessageID, peerID, isGroup, evt.Time, nickname, sum)
	}

	if evt.MessageType == "group" {
		// 群消息必须 @ 机器人才响应
		if p.botID == 0 {
			log.Printf("[qq] bot ID unknown, rejecting group message from %d", evt.GroupID)
			w.WriteHeader(http.StatusOK)
			return
		}
		if !p.isAtBot(evt.Message) {
			// 诊断：@ 解析失败时打印 at 段原文与 botID，定位漏报问题
			log.Printf("[qq] group msg from %d/%d not @bot (botID=%d, raw=%.120s)",
				evt.GroupID, evt.UserID, p.botID, rawCQ)
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	tp := p.name + "_"
	outputTool := "output_send__" + p.name
	var interrupt string
	if evt.MessageType == "group" {
		interrupt = fmt.Sprintf("来自「%s」在群「%s」的消息(message_id=%d)。先用%sget_message(message_id=%d)取正文；若取不到（消息已过期），改用%sget_history(group_id=%d)按会话拉取上下文，或用%slist_chats 查看未读会话。用%s回复群聊", nickname, "群聊", evt.MessageID, tp, evt.MessageID, tp, evt.GroupID, tp, outputTool)
	} else {
		interrupt = fmt.Sprintf("来自「%s」的私聊消息(message_id=%d, user_id=%d)。先用%sget_message(message_id=%d)取正文；若取不到（消息已过期），改用%sget_history(user_id=%d)按会话拉取上下文，或用%slist_chats 查看未读会话。用%s回复对方", nickname, evt.MessageID, evt.UserID, tp, evt.MessageID, tp, evt.UserID, tp, outputTool)
	}
	if p.isOwner(evt.UserID) {
		interrupt = "【重要！Bot 所有者消息】" + interrupt
	}

	if text != "" {
		text = stripCQRe.ReplaceAllString(text, "")
		text = strings.TrimSpace(text)
	}
	if text == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	highRisk := false
	if highRiskRe.MatchString(text) {
		highRisk = true
		interrupt = "【⚠️ 高危信息，谨慎处理】" + interrupt
	}

	if evt.MessageType == "group" && p.sdk != nil {
		rulesRaw := getSetting[string](p.sdk.Settings(), "forward_rules", "[]")
		var rules []ForwardRule
		if json.Unmarshal([]byte(rulesRaw), &rules) == nil {
			for _, rule := range rules {
				if evt.GroupID == rule.GroupID {
					mcMsg := fmt.Sprintf("%s 说 %s", nickname, text)
					go func(r ForwardRule, msg string) {
						defer func() { _ = recover() }()
						if err := rconSend(r.Host, r.Port, r.Password, "say "+msg); err != nil {
							log.Printf("[qq] rcon forward to %s:%d: %v", r.Host, r.Port, err)
						}
					}(rule, mcMsg)
				}
			}
		}
	}

	// 必须在注入前记录 OneBot 可信来源；权限判断绝不依赖昵称、正文或模型参数。
	p.activateAuthContext(evt.MessageID, evt.UserID, evt.GroupID, evt.MessageType == "group")

	if evt.MessageType == "private" {
		p.startTyping(evt.UserID)
	}

	// 合并投递：同一会话同一发送者在 batchWindow 内的连续消息并成一次中断。
	p.enqueueInterrupt(evt.MessageType, evt.UserID, evt.GroupID, evt.MessageID, nickname, interrupt, p.isOwner(evt.UserID), highRisk)

	w.WriteHeader(http.StatusOK)
}

// ======== Tool Handlers ========

// getMsgFromHistoryByTime 按 (peer, isGroup, targetTime) 从 NapCat 拉最近历史，返回距 targetTime 最近的完整消息。
func (p *Plugin) getMsgFromHistoryByTime(peerID int64, isGroup bool, targetTime int64) (map[string]interface{}, bool) {
	ep := "get_friend_msg_history"
	params := map[string]interface{}{"user_id": peerID, "count": 50}
	if isGroup {
		ep = "get_group_msg_history"
		params = map[string]interface{}{"group_id": peerID, "count": 50}
	}
	raw, err := p.napcat(ep, params)
	if err != nil {
		return nil, false
	}
	rawStr, _ := rawString(raw)
	if rawStr == "" {
		return nil, false
	}
	var resp struct {
		Data *struct {
			Messages []interface{} `json:"messages"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(rawStr), &resp) != nil || resp.Data == nil {
		return nil, false
	}
	var best map[string]interface{}
	bestAbs := int64(-1)
	for _, m := range resp.Data.Messages {
		mm, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		mt, _ := mm["time"].(float64)
		t := int64(mt)
		if t == 0 {
			continue
		}
		abs := t - targetTime
		if abs < 0 {
			abs = -abs
		}
		if bestAbs < 0 || abs < bestAbs {
			bestAbs = abs
			best = mm
		}
	}
	if best == nil {
		return nil, false
	}
	return best, true
}

// msgToGetMsgResult 把一条 NapCat 历史消息对象转成与 get_msg 同构的结果（历史包装语义）。
func msgToGetMsgResult(msg map[string]interface{}) map[string]interface{} {
	nickname := ""
	if s, ok := msg["sender"].(map[string]interface{}); ok {
		if n, _ := s["nickname"].(string); n != "" {
			nickname = n
		}
		if c, _ := s["card"].(string); c != "" {
			nickname = c
		}
	}
	rawText, _ := msg["raw_message"].(string)
	content := rawText
	if content == "" {
		if segs, ok := msg["message"].([]interface{}); ok {
			var parts []string
			for _, seg := range segs {
				segMap, _ := seg.(map[string]interface{})
				if segMap == nil {
					continue
				}
				typ, _ := segMap["type"].(string)
				segData, _ := segMap["data"].(map[string]interface{})
				if segData == nil {
					continue
				}
				switch typ {
				case "text":
					if t, _ := segData["text"].(string); t != "" {
						parts = append(parts, t)
					}
				case "image":
					parts = append(parts, "[图片]")
				case "file":
					if n, _ := segData["name"].(string); n != "" {
						parts = append(parts, "[文件:"+n+"]")
					}
				default:
					if typ != "" {
						parts = append(parts, "["+typ+"]")
					}
				}
			}
			if len(parts) > 0 {
				content = strings.Join(parts, " ")
			}
		}
	}
	mid, _ := msg["message_id"].(float64)
	uid, _ := msg["user_id"].(float64)
	gid, _ := msg["group_id"].(float64)
	mt, _ := msg["time"].(float64)
	mtType, _ := msg["message_type"].(string)
	loc := "私聊"
	if mtType == "group" || gid > 0 {
		loc = "群聊"
	}
	return map[string]interface{}{
		"content":      content,
		"message_id":   int64(mid),
		"user_id":      int64(uid),
		"group_id":     int64(gid),
		"nickname":     nickname,
		"message_type": mtType,
		"type":         loc,
		"time":         time.Unix(int64(mt), 0).Format("2006-01-02 15:04:05"),
	}
}

func (p *Plugin) handleGetMessage(args map[string]interface{}) (interface{}, error) {
	msgID, err := convInt64(args["message_id"])
	if err != nil {
		return map[string]interface{}{
			"content":   "需要提供 message_id 参数",
			"not_found": true,
		}, nil
	}

	// 本地 msg_id→peer 映射命中且 <7 天 → 用 get_history 语义兜底（NapCat 临时短号失效也不怕）
	if peerID, isGroup, t, ok := p.lookupMsgRef(msgID); ok {
		if m, found := p.getMsgFromHistoryByTime(peerID, isGroup, t); found {
			// 找到同会话、时间最接近的消息，包装为 get_msg 同构返回
			res := msgToGetMsgResult(m)
			res["resolved_via"] = "history" // 标明由历史查询兜底
			return res, nil
		}
		// 历史窗口内没找到（消息可能被裁剪/更早），回退 NapCat 原查询
	}
	return p.getMsgFromNapcat(msgID)
}

func (p *Plugin) getMsgFromNapcat(msgID int64) (interface{}, error) {
	raw, err := p.napcat("get_msg", map[string]interface{}{"message_id": msgID})
	if err != nil {
		return map[string]interface{}{
			"content":    fmt.Sprintf("查询 NapCat 失败: %s。该 message_id 可能已过期，请改用 qq_get_history 按会话拉取最近消息（或用 qq_list_chats 看未读会话）", err),
			"message_id": msgID,
			"not_found":  true,
		}, nil
	}
	rawStr, _ := rawString(raw)
	if rawStr == "" {
		return map[string]interface{}{
			"content":    "NapCat 返回空响应",
			"message_id": msgID,
			"not_found":  true,
		}, nil
	}

	var resp struct {
		Data *struct {
			MessageID   int64       `json:"message_id"`
			UserID      int64       `json:"user_id"`
			GroupID     int64       `json:"group_id"`
			Time        int64       `json:"time"`
			MessageType string      `json:"message_type"`
			RawMessage  string      `json:"raw_message"`
			Message     interface{} `json:"message"`
			Sender      *struct {
				Nickname string `json:"nickname"`
				Card     string `json:"card"`
			} `json:"sender"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(rawStr), &resp); err != nil || resp.Data == nil {
		return map[string]interface{}{
			"content":    "解析 NapCat 响应失败（消息可能已过期）。请改用 qq_get_history 按会话拉取最近消息，或用 qq_list_chats 查看未读会话",
			"message_id": msgID,
			"not_found":  true,
		}, nil
	}

	d := resp.Data
	nickname := ""
	if d.Sender != nil {
		nickname = d.Sender.Nickname
		if d.Sender.Card != "" {
			nickname = d.Sender.Card
		}
	}

	content := d.RawMessage
	var files []map[string]interface{}
	var replyToID int64
	var hasImage, hasFile bool
	if segs, ok := d.Message.([]interface{}); ok {
		var parts []string
		for _, seg := range segs {
			segMap, _ := seg.(map[string]interface{})
			if segMap == nil {
				continue
			}
			typ, _ := segMap["type"].(string)
			segData, _ := segMap["data"].(map[string]interface{})
			if segData == nil {
				continue
			}
			switch typ {
			case "text":
				if t, _ := segData["text"].(string); t != "" {
					parts = append(parts, t)
				}
			case "at":
				if qq, _ := segData["qq"].(string); qq != "" {
					parts = append(parts, "@"+qq)
				}
			case "reply":
				if idStr, ok := segData["id"].(string); ok {
					replyToID, _ = strconv.ParseInt(idStr, 10, 64)
				} else if id, ok := segData["id"].(float64); ok {
					replyToID = int64(id)
				}
				parts = append(parts, fmt.Sprintf("[回复id=%d]", replyToID))
			case "file":
				hasFile = true
				fid, _ := segData["file_id"].(string)
				if fid == "" {
					fid, _ = segData["file"].(string)
				}
				name, _ := segData["name"].(string)
				if name == "" {
					name, _ = segData["file"].(string)
				}
				fileURL, _ := segData["url"].(string)
				if fid != "" {
					entry := map[string]interface{}{
						"file_id": fid,
						"name":    name,
					}
					if fileURL != "" {
						entry["url"] = fileURL
					}
					files = append(files, entry)
				}
			case "image":
				hasImage = true
			}
		}
		if len(parts) > 0 {
			content = strings.Join(parts, " ")
		}
	}

	loc := "私聊"
	if d.MessageType == "group" {
		loc = "群聊"
	}
	result := map[string]interface{}{
		"content":      content,
		"message_id":   d.MessageID,
		"user_id":      d.UserID,
		"group_id":     d.GroupID,
		"nickname":     nickname,
		"message_type": d.MessageType,
		"type":         loc,
		"time":         time.Unix(d.Time, 0).Format("2006-01-02 15:04:05"),
	}
	if replyToID > 0 {
		result["reply_to"] = map[string]interface{}{"message_id": replyToID}
	}
	if hasImage {
		result["has_image"] = true
	}
	if hasFile {
		result["has_file"] = true
	}
	if len(files) > 0 {
		result["files"] = files
	}

	// 异步标记已读
	go func() {
		defer func() { _ = recover() }() // 后台任务不允许 panic 冒泡带崩进程
		if d.MessageType == "group" && d.GroupID > 0 {
			p.napcat("mark_group_msg_as_read", map[string]interface{}{"group_id": d.GroupID})
		} else if d.UserID > 0 {
			p.napcat("mark_private_msg_as_read", map[string]interface{}{"user_id": d.UserID})
		}
	}()

	return result, nil
}

// handleChannelOutput — output_send(channel="qq") 的处理器
// args 包含 payload, type, (可选 meta)
// 成功时只返回极简标记，不回传 NapCat 完整响应——避免"已发送"类富回执喂给模型
// 造成"看到成功→继续发下一条"的回声循环（issue: output loop echo）。
func (p *Plugin) handleChannelOutput(args map[string]interface{}) (interface{}, error) {
	payload, _ := args["payload"].(string)
	rawType, _ := args["type"].(string)
	meta, _ := args["meta"].(string)
	log.Printf("[qq] handleChannelOutput type=%s payload_len=%d meta=%s", rawType, len(payload), meta)
	if payload == "" || rawType == "" {
		return nil, fmt.Errorf("payload 和 type 参数不能为空")
	}

	// 解析 meta
	var groupID, userID int64
	var replyTo int64
	if meta != "" {
		var m struct {
			GroupID int64 `json:"group_id"`
			UserID  int64 `json:"user_id"`
			ReplyTo int64 `json:"reply_to"`
		}
		if err := json.Unmarshal([]byte(meta), &m); err == nil {
			groupID = m.GroupID
			userID = m.UserID
			replyTo = m.ReplyTo
		}
	}
	if groupID == 0 && userID == 0 {
		return nil, fmt.Errorf("meta 中需要 group_id 或 user_id 字段。用 output_send__qq_help 查看格式说明")
	}

	if userID > 0 {
		p.stopTyping(userID)
	}

	var sendErr error
	switch rawType {
	case "text":
		text := p.sensitiveFilter(payload)
		message := interface{}(text)
		if groupID != 0 && userID != 0 {
			message = messageWithMention(userID, map[string]interface{}{"type": "text", "data": map[string]interface{}{"text": text}})
		}
		msg := map[string]interface{}{"message": message}
		if groupID != 0 {
			msg["group_id"] = groupID
		} else {
			msg["user_id"] = userID
		}
		if replyTo > 0 {
			msg["reply_to"] = replyTo
		}
		if groupID != 0 {
			_, sendErr = p.napcat("send_group_msg", msg)
		} else {
			_, sendErr = p.napcat("send_private_msg", msg)
		}

	case "voice", "audio":
		text := p.sensitiveFilter(payload)
		audioFile, err := p.ttsToFile(text)
		if err != nil {
			return nil, fmt.Errorf("语音生成失败: %w", err)
		}
		os.MkdirAll(p.remoteDir, 0755)
		dest := filepath.Join(p.remoteDir, filepath.Base(audioFile))
		data, err := os.ReadFile(audioFile)
		if err != nil {
			return nil, fmt.Errorf("读取音频文件失败: %w", err)
		}
		if err := os.WriteFile(dest, data, 0644); err != nil {
			return nil, fmt.Errorf("写入共享目录失败: %w", err)
		}
		os.Remove(audioFile)
		uri := fmt.Sprintf("file:///app/files/%s", filepath.Base(dest))
		cqMsg := fmt.Sprintf("[CQ:record,file=%s]", uri)
		message := interface{}(cqMsg)
		if groupID != 0 && userID != 0 {
			message = messageWithMention(userID, map[string]interface{}{"type": "record", "data": map[string]interface{}{"file": uri}})
		}
		msg := map[string]interface{}{"message": message}
		if groupID != 0 {
			msg["group_id"] = groupID
		} else {
			msg["user_id"] = userID
		}
		if groupID != 0 {
			_, sendErr = p.napcat("send_group_msg", msg)
		} else {
			_, sendErr = p.napcat("send_private_msg", msg)
		}

	case "image", "file":
		uri := payload
		if !strings.HasPrefix(payload, "http://") && !strings.HasPrefix(payload, "https://") &&
			!strings.HasPrefix(payload, "file://") {
			if _, err := os.Stat(payload); err != nil {
				return nil, fmt.Errorf("%s 文件不存在: %s", rawType, payload)
			}
			os.MkdirAll(p.remoteDir, 0755)
			dest := filepath.Join(p.remoteDir, sanitizeFilename(filepath.Base(payload)))
			data, err := os.ReadFile(payload)
			if err != nil {
				return nil, fmt.Errorf("读取文件失败: %w", err)
			}
			if err := os.WriteFile(dest, data, 0644); err != nil {
				return nil, fmt.Errorf("写入共享目录失败: %w", err)
			}
			uri = "file:///app/files/" + filepath.Base(dest)
		}
		cqTag := "file"
		if rawType == "image" {
			cqTag = "image"
		}
		message := interface{}(fmt.Sprintf("[CQ:%s,file=%s]", cqTag, uri))
		if groupID != 0 && userID != 0 {
			message = messageWithMention(userID, map[string]interface{}{"type": cqTag, "data": map[string]interface{}{"file": uri}})
		}
		msg := map[string]interface{}{"message": message}
		if groupID != 0 {
			msg["group_id"] = groupID
		} else {
			msg["user_id"] = userID
		}
		if groupID != 0 {
			_, sendErr = p.napcat("send_group_msg", msg)
		} else {
			_, sendErr = p.napcat("send_private_msg", msg)
		}

	default:
		return nil, fmt.Errorf("不支持的 type: %s（枚举值: text/voice/image/file）", rawType)
	}

	if sendErr != nil {
		return nil, sendErr
	}
	// 成功：返回极简标记。不再回传 NapCat 原始响应（含 message_id 等）给模型，
	// 避免模型把"发送成功"当成"上一步完成，继续下一步"的信号驱动循环。
	return "ok", nil
}

func (p *Plugin) buildOutputHelp() string {
	return `【参数】
payload — 消息载荷。type=text时直接填文字，type=voice时填文字（自动转语音），type=image/file时填URL
meta    — JSON 元数据，含 group_id（群聊）和/或 user_id（私聊或群内@），可选 reply_to
type    — text / voice / image / file

【示例】
群聊文字：output_send__qq(payload="你好", meta="{\"group_id\":123456789}", type="text")
私聊语音：output_send__qq(payload="你好", meta="{\"user_id\":123456789}", type="voice")
群内@用户：output_send__qq(payload="你好", meta="{\"group_id\":123456789,\"user_id\":987654321}", type="text")
发送图片：output_send__qq(payload="https://example.com/img.jpg", meta="{\"group_id\":123456789}", type="image")

【注意】
- group_id 与 user_id 同时存在时始终发送到 group_id，并在消息头 @user_id
- type=text 时 payload 直接是文字，无需 JSON 包裹
- type=voice 时 payload 是文字内容，自动转语音发送
- type=image/file 时 payload 是 URL 或路径`
}

// ttsToFile 用 edge-tts 将文本转为音频文件，返回临时文件路径
func (p *Plugin) ttsToFile(text string) (string, error) {
	// 清理文本中的特殊字符
	clean := strings.Map(func(r rune) rune {
		if r == '"' || r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, text)
	clean = strings.TrimSpace(clean)
	if clean == "" {
		clean = " "
	}

	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("qq_tts_%d.mp3", time.Now().UnixNano()))
	cmd := exec.Command("edge-tts",
		"--voice", "zh-CN-XiaoxiaoNeural",
		"--text", clean,
		"--write-media", tmpFile,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("edge-tts: %w\nstderr: %s", err, stderr.String())
	}
	if _, err := os.Stat(tmpFile); os.IsNotExist(err) {
		return "", fmt.Errorf("edge-tts 未生成输出文件")
	}
	return tmpFile, nil
}

func (p *Plugin) handleSendFile(args map[string]interface{}) (interface{}, error) {
	gid, gerr := convInt64(args["group_id"])
	uid, uerr := convInt64(args["user_id"])
	if gerr != nil && uerr != nil {
		return nil, fmt.Errorf("need group_id or user_id")
	}
	filePath, _ := args["file"].(string)
	if filePath == "" {
		return nil, fmt.Errorf("need file path")
	}
	name, _ := args["name"].(string)
	if name == "" {
		name = filepath.Base(filePath)
	}
	name = sanitizeFilename(name)
	asImage, _ := args["as_image"].(bool)

	// copy to remote dir for NapCat container access
	dest := filepath.Join(p.remoteDir, name)
	srcData, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if err := os.WriteFile(dest, srcData, 0644); err != nil {
		return nil, fmt.Errorf("write remote: %w", err)
	}

	uri := fmt.Sprintf("file:///app/files/%s", name)
	var cqMsg string
	if asImage {
		cqMsg = fmt.Sprintf("[CQ:image,file=%s]", uri)
	} else {
		cqMsg = fmt.Sprintf("[CQ:file,file=%s,title=%s]", uri, name)
	}

	params := map[string]interface{}{"message": cqMsg}
	if gerr == nil {
		params["group_id"] = gid
		return p.napcat("send_group_msg", params)
	}
	params["user_id"] = uid
	return p.napcat("send_private_msg", params)
}

func (p *Plugin) handleListChats(args map[string]interface{}) (interface{}, error) {
	count := 10
	if c, err := convInt64(args["count"]); err == nil && c > 0 && c < 100 {
		count = int(c)
	}
	chats := p.listChats(count)
	return map[string]interface{}{
		"chats": chats,
		"total": len(chats),
		"hint":  "按最新消息先后排序；unread 为该会话未读消息数，处理完用 qq_mark_read 清零；用 qq_get_history(group_id/user_id) 拉取会话内容",
	}, nil
}

func (p *Plugin) handleMarkRead(args map[string]interface{}) (interface{}, error) {
	if gid, err := convInt64(args["group_id"]); err == nil {
		p.markChatRead(gid)
		return map[string]interface{}{"status": "ok", "group_id": gid, "unread": 0}, nil
	}
	if uid, err := convInt64(args["user_id"]); err == nil {
		p.markChatRead(uid)
		return map[string]interface{}{"status": "ok", "user_id": uid, "unread": 0}, nil
	}
	return nil, fmt.Errorf("need group_id or user_id")
}

func (p *Plugin) handleGetHistory(args map[string]interface{}) (interface{}, error) {
	gid, gerr := convInt64(args["group_id"])
	uid, uerr := convInt64(args["user_id"])
	count := 10
	if c, err := convInt64(args["count"]); err == nil && c > 0 {
		count = int(c)
	}

	var endpoint string
	var params map[string]interface{}
	if gerr == nil {
		endpoint = "get_group_msg_history"
		params = map[string]interface{}{"group_id": gid, "count": count}
	} else if uerr == nil {
		endpoint = "get_friend_msg_history"
		params = map[string]interface{}{"user_id": uid, "count": count}
	} else {
		return nil, fmt.Errorf("need group_id or user_id")
	}

	rawResp, err := p.napcat(endpoint, params)
	if err != nil {
		return nil, err
	}
	rawStr, _ := rawString(rawResp)
	if rawStr == "" {
		return map[string]interface{}{"messages": []interface{}{}, "note": "未获取到历史消息"}, nil
	}

	// 解析 NapCat 响应，提取消息列表
	var resp struct {
		Data *struct {
			Messages []interface{} `json:"messages"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(rawStr), &resp); err != nil || resp.Data == nil {
		return map[string]interface{}{"raw_response": rawStr, "note": "解析 NapCat 响应失败"}, nil
	}

	// 格式化消息为可读文本，并提取文件信息
	var lines []string
	var files []map[string]interface{}
	for _, m := range resp.Data.Messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		sender := ""
		if s, ok := msg["sender"].(map[string]interface{}); ok {
			if nick, _ := s["nickname"].(string); nick != "" {
				sender = nick
			}
			if card, _ := s["card"].(string); card != "" {
				sender = card
			}
		}
		msgText, _ := msg["raw_message"].(string)
		if msgText == "" {
			if segs, ok := msg["message"].([]interface{}); ok {
				var parts []string
				for _, seg := range segs {
					segMap, _ := seg.(map[string]interface{})
					if segMap == nil {
						continue
					}
					typ, _ := segMap["type"].(string)
					segData, _ := segMap["data"].(map[string]interface{})
					if segData == nil {
						continue
					}
					switch typ {
					case "text":
						if t, _ := segData["text"].(string); t != "" {
							parts = append(parts, t)
						}
					case "file":
						name, _ := segData["name"].(string)
						if name == "" {
							name, _ = segData["file"].(string)
						}
						fid, _ := segData["file_id"].(string)
						fileURL, _ := segData["url"].(string)
						if name != "" {
							parts = append(parts, "[文件:"+name+"]")
						}
						if fid != "" {
							entry := map[string]interface{}{
								"file_id":  fid,
								"name":     name,
								"position": len(lines),
							}
							if fileURL != "" {
								entry["url"] = fileURL
							}
							files = append(files, entry)
						}
					case "image":
						parts = append(parts, "[图片]")
					case "video":
						parts = append(parts, "[视频]")
					}
				}
				if len(parts) > 0 {
					msgText = strings.Join(parts, " ")
				}
			}
		}
		if msgText == "" {
			continue
		}
		ts := ""
		if t, ok := msg["time"].(float64); ok {
			ts = time.Unix(int64(t), 0).Format("2006-01-02 15:04")
		}
		line := msgText
		if sender != "" {
			line = sender + ": " + msgText
		}
		if ts != "" {
			line = "[" + ts + "] " + line
		}
		lines = append(lines, line)
	}

	if len(lines) == 0 {
		return map[string]interface{}{
			"messages": []interface{}{},
			"note":     "未找到历史消息，可能群内暂无消息记录",
		}, nil
	}

	result := map[string]interface{}{
		"messages": lines,
		"count":    len(lines),
	}
	if len(files) > 0 {
		result["files"] = files
	}
	// 拉取过某会话历史即视为已读（与真人客户端一致：看过=已读）
	if gerr == nil {
		p.markChatRead(gid)
	} else if uerr == nil {
		p.markChatRead(uid)
	}
	return result, nil
}

func (p *Plugin) handleGetGroups(args map[string]interface{}) (interface{}, error) {
	return p.napcat("get_group_list", map[string]interface{}{})
}

func (p *Plugin) handleGetFriends(args map[string]interface{}) (interface{}, error) {
	return p.napcat("get_friend_list", map[string]interface{}{})
}

func (p *Plugin) handleGetRecentContacts(args map[string]interface{}) (interface{}, error) {
	count := 10
	if c, err := convInt64(args["count"]); err == nil && c > 0 {
		count = int(c)
	}
	return p.napcat("get_recent_contact", map[string]interface{}{"count": count})
}

func (p *Plugin) handleResolveName(args map[string]interface{}) (interface{}, error) {
	if uid, err := convInt64(args["user_id"]); err == nil {
		return p.napcat("get_stranger_info", map[string]interface{}{"user_id": uid, "no_cache": true})
	}
	if gid, err := convInt64(args["group_id"]); err == nil {
		return p.napcat("get_group_info", map[string]interface{}{"group_id": gid, "no_cache": true})
	}
	return nil, fmt.Errorf("need user_id or group_id")
}

func (p *Plugin) handleResolveNickname(args map[string]interface{}) (interface{}, error) {
	keyword, _ := args["keyword"].(string)
	if keyword == "" {
		return nil, fmt.Errorf("keyword is required")
	}
	keyword = strings.ToLower(keyword)

	gid, groupErr := convInt64(args["group_id"])
	if groupErr == nil {
		v, err := p.napcat("get_group_member_list", map[string]interface{}{"group_id": gid})
		if err != nil {
			return nil, err
		}
		raw, _ := rawString(v)
		return filterMemberList(raw, keyword)
	}

	v, err := p.napcat("get_friend_list", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	raw, _ := rawString(v)
	return filterFriendList(raw, keyword)
}

func filterFriendList(raw, keyword string) (interface{}, error) {
	var resp struct {
		Data []struct {
			UserID   int64  `json:"user_id"`
			Nickname string `json:"nickname"`
			Remark   string `json:"remark"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return raw, nil
	}
	var matches []map[string]interface{}
	for _, f := range resp.Data {
		if strings.Contains(strings.ToLower(f.Nickname), keyword) ||
			strings.Contains(strings.ToLower(f.Remark), keyword) {
			matches = append(matches, map[string]interface{}{
				"user_id":  f.UserID,
				"nickname": f.Nickname,
				"remark":   f.Remark,
			})
		}
	}
	if len(matches) == 0 {
		return fmt.Sprintf("未找到昵称/备注包含 %q 的好友", keyword), nil
	}
	return matches, nil
}

func filterMemberList(raw, keyword string) (interface{}, error) {
	var resp struct {
		Data []struct {
			UserID   int64  `json:"user_id"`
			Nickname string `json:"nickname"`
			Card     string `json:"card"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return raw, nil
	}
	var matches []map[string]interface{}
	for _, m := range resp.Data {
		if strings.Contains(strings.ToLower(m.Nickname), keyword) ||
			strings.Contains(strings.ToLower(m.Card), keyword) {
			matches = append(matches, map[string]interface{}{
				"user_id":  m.UserID,
				"nickname": m.Nickname,
				"card":     m.Card,
			})
		}
	}
	if len(matches) == 0 {
		return fmt.Sprintf("未找到昵称/名片包含 %q 的群成员", keyword), nil
	}
	return matches, nil
}

func (p *Plugin) handleGetGroupMemberInfo(args map[string]interface{}) (interface{}, error) {
	gid, _ := convInt64(args["group_id"])
	uid, _ := convInt64(args["user_id"])
	return p.napcat("get_group_member_info", map[string]interface{}{"group_id": gid, "user_id": uid})
}

func (p *Plugin) handleGroupManage(args map[string]interface{}) (interface{}, error) {
	cmd, _ := args["command"].(string)
	if cmd == "" {
		return nil, fmt.Errorf("need command")
	}
	if requiresConfirmGroupCommand(cmd) {
		if ok, _ := args["confirm"].(bool); !ok {
			return map[string]interface{}{"isError": true, "content": fmt.Sprintf("高风险操作 %s 需要 confirm=true", cmd)}, nil
		}
	}

	switch cmd {
	case "group-list":
		return p.napcat("get_group_list", map[string]interface{}{})
	case "group-info", "member-list", "member-info", "at-all-remain", "msg-history":
		gid, _ := convInt64(args["group_id"])
		if cmd == "msg-history" {
			count := 10
			if c, err := convInt64(args["count"]); err == nil && c > 0 {
				count = int(c)
			}
			return p.napcat("get_group_msg_history", map[string]interface{}{"group_id": gid, "count": count})
		}
		if cmd == "member-info" {
			uid, _ := convInt64(args["user_id"])
			return p.napcat("get_group_member_info", map[string]interface{}{"group_id": gid, "user_id": uid})
		}
		if cmd == "at-all-remain" {
			return p.napcat("get_group_at_all_remain", map[string]interface{}{"group_id": gid})
		}
		if cmd == "group-info" {
			return p.napcat("get_group_info", map[string]interface{}{"group_id": gid})
		}
		return p.napcat("get_group_member_list", map[string]interface{}{"group_id": gid})

	case "list-files":
		gid, _ := convInt64(args["group_id"])
		folderID, _ := args["folder_id"].(string)
		if folderID != "" {
			return p.napcat("get_group_files_by_folder", map[string]interface{}{"group_id": gid, "folder_id": folderID})
		}
		return p.napcat("get_group_root_files", map[string]interface{}{"group_id": gid})

	case "pending-requests":
		return p.napcat("get_group_system_msg", map[string]interface{}{})

	case "leave":
		gid, _ := convInt64(args["group_id"])
		return p.napcat("set_group_leave", map[string]interface{}{"group_id": gid})

	case "kick":
		gid, _ := convInt64(args["group_id"])
		uid, _ := convInt64(args["user_id"])
		reject, _ := args["reject_add"].(bool)
		return p.napcat("set_group_kick", map[string]interface{}{"group_id": gid, "user_id": uid, "reject_add_request": reject})

	case "ban":
		gid, _ := convInt64(args["group_id"])
		uid, _ := convInt64(args["user_id"])
		minutes := 10
		if m, err := convInt64(args["minutes"]); err == nil {
			minutes = int(m)
		}
		return p.napcat("set_group_ban", map[string]interface{}{"group_id": gid, "user_id": uid, "duration": minutes * 60})

	case "unban":
		gid, _ := convInt64(args["group_id"])
		uid, _ := convInt64(args["user_id"])
		return p.napcat("set_group_ban", map[string]interface{}{"group_id": gid, "user_id": uid, "duration": 0})

	case "rename":
		gid, _ := convInt64(args["group_id"])
		name, _ := args["name"].(string)
		return p.napcat("set_group_name", map[string]interface{}{"group_id": gid, "group_name": name})

	case "mute-all":
		gid, _ := convInt64(args["group_id"])
		enable, _ := args["enable"].(bool)
		return p.napcat("set_group_whole_ban", map[string]interface{}{"group_id": gid, "enable": enable})

	case "set-card":
		gid, _ := convInt64(args["group_id"])
		uid, _ := convInt64(args["user_id"])
		card, _ := args["card"].(string)
		return p.napcat("set_group_card", map[string]interface{}{"group_id": gid, "user_id": uid, "card": card})

	case "set-admin":
		gid, _ := convInt64(args["group_id"])
		uid, _ := convInt64(args["user_id"])
		enable, _ := args["enable"].(bool)
		return p.napcat("set_group_admin", map[string]interface{}{"group_id": gid, "user_id": uid, "enable": enable})

	case "set-title":
		gid, _ := convInt64(args["group_id"])
		uid, _ := convInt64(args["user_id"])
		title, _ := args["title"].(string)
		return p.napcat("set_group_special_title", map[string]interface{}{"group_id": gid, "user_id": uid, "special_title": title})

	case "recall":
		mid, _ := convInt64(args["message_id"])
		return p.napcat("delete_msg", map[string]interface{}{"message_id": mid})

	case "pin-msg":
		mid, _ := convInt64(args["message_id"])
		return p.napcat("set_essence_msg", map[string]interface{}{"message_id": mid})

	case "folder-create":
		gid, _ := convInt64(args["group_id"])
		name, _ := args["name"].(string)
		return p.napcat("create_group_file_folder", map[string]interface{}{"group_id": gid, "name": name})

	default:
		return nil, fmt.Errorf("unknown group_manage command: %s", cmd)
	}
}

func (p *Plugin) handleFriendAction(args map[string]interface{}) (interface{}, error) {
	cmd, _ := args["command"].(string)
	if requiresConfirmFriendCommand(cmd) {
		if ok, _ := args["confirm"].(bool); !ok {
			return map[string]interface{}{"isError": true, "content": fmt.Sprintf("高风险操作 %s 需要 confirm=true", cmd)}, nil
		}
	}
	switch cmd {
	case "list-friends":
		return p.napcat("get_friend_list", map[string]interface{}{})
	case "delete":
		uid, _ := convInt64(args["user_id"])
		return p.napcat("delete_friend", map[string]interface{}{"user_id": uid})
	case "block":
		uid, _ := convInt64(args["user_id"])
		// delete friend
		p.napcat("delete_friend", map[string]interface{}{"user_id": uid})
		// kick from groups
		gid, err := convInt64(args["group_id"])
		if err != nil {
			return map[string]interface{}{"isError": true, "content": "block 必须提供 group_id（插件不会自动遍历所有群踢人）"}, nil
		}
		p.napcat("set_group_kick", map[string]interface{}{"group_id": gid, "user_id": uid, "reject_add_request": true})
		return `{"status":"ok","message":"blocked"}`, nil
	case "approve-friend":
		flag, _ := args["flag"].(string)
		remark, _ := args["remark"].(string)
		return p.napcat("set_friend_add_request", map[string]interface{}{"flag": flag, "approve": true, "remark": remark})
	case "reject-friend":
		flag, _ := args["flag"].(string)
		return p.napcat("set_friend_add_request", map[string]interface{}{"flag": flag, "approve": false})
	default:
		return nil, fmt.Errorf("unknown friend_action command: %s", cmd)
	}
}

func (p *Plugin) handleGetGroupFiles(args map[string]interface{}) (interface{}, error) {
	gid, _ := convInt64(args["group_id"])
	cmd, _ := args["command"].(string)

	switch cmd {
	case "list":
		folderID, _ := args["folder_id"].(string)
		if folderID != "" {
			return p.napcat("get_group_files_by_folder", map[string]interface{}{"group_id": gid, "folder_id": folderID})
		}
		return p.napcat("get_group_root_files", map[string]interface{}{"group_id": gid})

	case "search":
		return p.napcat("get_group_root_files", map[string]interface{}{"group_id": gid})

	case "download":
		fileID, _ := args["file_id"].(string)
		filename, _ := args["filename"].(string)
		if filename == "" {
			filename = fmt.Sprintf("group_file_%s", fileID)
		}
		filename = sanitizeFilename(filename)
		// get download URL
		resp, err := p.napcat("get_group_file_url", map[string]interface{}{"group_id": gid, "file_id": fileID})
		if err != nil {
			return nil, err
		}
		respStr, ok := resp.(string)
		if !ok {
			return resp, nil
		}
		// parse URL from response
		var parsed struct {
			Data struct {
				URL string `json:"url"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(respStr), &parsed); err != nil || parsed.Data.URL == "" {
			return resp, nil
		}
		dlURL := parsed.Data.URL
		client := &http.Client{Timeout: 120 * time.Second}
		httpResp, err := client.Get(dlURL)
		if err != nil {
			return nil, fmt.Errorf("download: %w", err)
		}
		defer httpResp.Body.Close()
		content, err := io.ReadAll(httpResp.Body)
		if err != nil {
			return nil, fmt.Errorf("read download: %w", err)
		}
		os.MkdirAll(p.filesDir, 0755)
		savePath := filepath.Join(p.filesDir, filename)
		if err := os.WriteFile(savePath, content, 0644); err != nil {
			return nil, fmt.Errorf("save: %w", err)
		}
		return map[string]interface{}{
			"status": "ok", "path": savePath, "filename": filename, "size": len(content),
		}, nil

	default:
		return nil, fmt.Errorf("unknown get_group_files command: %s", cmd)
	}
}

func (p *Plugin) addDownloadTask(fileID, filename string) *DownloadTask {
	p.downloadMu.Lock()
	defer p.downloadMu.Unlock()
	t := &DownloadTask{FileID: fileID, Filename: filename, Status: "running", CreatedAt: time.Now().Format("15:04:05")}
	p.downloadTasks = append(p.downloadTasks, t)
	if len(p.downloadTasks) > 100 {
		p.downloadTasks = p.downloadTasks[len(p.downloadTasks)-100:]
	}
	return t
}

func (p *Plugin) updateDownloadTask(t *DownloadTask, status, path, errMsg string) {
	p.downloadMu.Lock()
	defer p.downloadMu.Unlock()
	t.Status = status
	t.Path = path
	t.Error = errMsg
}

func (p *Plugin) handleDownloadFile(args map[string]interface{}) (interface{}, error) {
	fileID, _ := args["file_id"].(string)
	if fileID == "" {
		return nil, fmt.Errorf("file_id is required")
	}
	fileURL, _ := args["url"].(string)
	filename, _ := args["filename"].(string)
	groupID, _ := convInt64(args["group_id"])
	userID, _ := convInt64(args["user_id"])

	task := p.addDownloadTask(fileID, filename)

	go func(t *DownloadTask, fid, fname, furl string, gid, uid int64) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[qq] download task %s panic: %v", fid, r)
			}
		}()
		savePath := ""
		errMsg := ""
		if furl != "" {
			savePath = p.downloadURL(furl, fname)
		}
		if savePath == "" && gid > 0 {
			savePath = p.downloadGroupFile(gid, fid, fname)
		}
		if savePath == "" && uid > 0 {
			savePath = p.downloadPrivateFile(uid, fid, fname)
		}
		if savePath == "" {
			savePath = p.downloadFromNapCat(fid, fname)
		}
		if savePath != "" {
			p.updateDownloadTask(t, "done", savePath, "")
			log.Printf("[qq] 文件下载完成: %s", savePath)
			if p.sdk != nil {
				// NoMemory：下载完成的状态通知，不是记忆内容。
				// Priority：同上，QQ 侧一律低级别中断（L1）。
				p.sdk.InjectInterruptTextOpts(p.name, p.name,
					fmt.Sprintf("文件下载完成: %s，保存在 %s", filepath.Base(savePath), savePath),
					sdk.InjectOptions{NoMemory: true, Priority: sdk.PriorityL1})
			}
		} else {
			errMsg = "下载失败，文件可能已过期"
			p.updateDownloadTask(t, "failed", "", errMsg)
			log.Printf("[qq] 文件下载失败: %s", fid)
		}
	}(task, fileID, filename, fileURL, groupID, userID)

	return map[string]interface{}{
		"status": "started", "file_id": fileID, "filename": filename,
		"hint": "下载已后台启动。使用 qq_get_download_tasks 查看进度。"}, nil
}

func (p *Plugin) handleGetDownloadTasks(args map[string]interface{}) (interface{}, error) {
	p.downloadMu.Lock()
	defer p.downloadMu.Unlock()
	// 返回最近 50 条
	tasks := p.downloadTasks
	if len(tasks) > 50 {
		tasks = tasks[len(tasks)-50:]
	}
	return map[string]interface{}{
		"tasks": tasks, "total": len(p.downloadTasks),
		"hint": "status 为 running 表示下载中，done 已完成，failed 已失败。用 qq_download_file 重新下载失败的任务。"}, nil
}

func (p *Plugin) handleUploadGroupFile(args map[string]interface{}) (interface{}, error) {
	gid, _ := convInt64(args["group_id"])
	filePath, _ := args["file"].(string)
	name, _ := args["name"].(string)
	if name == "" {
		name = filepath.Base(filePath)
	}
	name = sanitizeFilename(name)

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	b64 := fmt.Sprintf("base64://%s", base64.StdEncoding.EncodeToString(data))

	resp, err := p.napcat("send_group_msg", map[string]interface{}{
		"group_id": gid,
		"message": []map[string]interface{}{
			{"type": "file", "data": map[string]interface{}{"file": b64, "name": name}},
		},
	})
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"status": "ok", "file": name, "napcat": resp}, nil
}

func (p *Plugin) handleSendLike(args map[string]interface{}) (interface{}, error) {
	uid, _ := convInt64(args["user_id"])
	times := 1
	if t, err := convInt64(args["times"]); err == nil && t > 0 && t <= 20 {
		times = int(t)
	}
	return p.napcat("send_like", map[string]interface{}{"user_id": uid, "times": times})
}

// ======== CQ Code / Message Segment Processing ========

func (p *Plugin) processMessageSegments(segments []interface{}, nickname string) string {
	if len(segments) == 0 {
		return ""
	}
	botIDStr := strconv.FormatInt(p.botID, 10)
	var parts []string

	for _, seg := range segments {
		s, ok := seg.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _ := s["type"].(string)
		data, _ := s["data"].(map[string]interface{})
		if data == nil {
			continue
		}

		switch typ {
		case "text":
			if t, _ := data["text"].(string); t != "" {
				parts = append(parts, t)
			}
		case "at":
			qq, _ := data["qq"].(string)
			if qq == "all" {
				parts = append(parts, "@所有人")
			} else if qq == botIDStr {
				continue
			} else {
				parts = append(parts, "@"+qq)
			}
		case "face", "sface":
			if id, _ := data["id"].(string); id != "" {
				parts = append(parts, "[表情]")
			}
		case "file":
			parts = append(parts, fmt.Sprintf("[%s发送了文件]", nickname))
		case "image":
			parts = append(parts, fmt.Sprintf("[%s发送了图片]", nickname))
		case "video":
			parts = append(parts, fmt.Sprintf("[%s发送了视频]", nickname))
		case "reply":
			if id, ok := data["id"].(float64); ok {
				parts = append(parts, fmt.Sprintf("[回复消息id=%.0f]", id))
			}
		case "music":
			if title, _ := data["title"].(string); title != "" {
				parts = append(parts, fmt.Sprintf("[音乐:%s]", title))
			} else {
				parts = append(parts, "[音乐]")
			}
		case "share":
			title, _ := data["title"].(string)
			urlStr, _ := data["url"].(string)
			if title != "" && urlStr != "" {
				parts = append(parts, fmt.Sprintf("[分享:%s %s]", title, urlStr))
			} else if urlStr != "" {
				parts = append(parts, fmt.Sprintf("[分享:%s]", urlStr))
			}
		default:
			if typ != "" {
				parts = append(parts, "["+typ+"]")
			}
		}
	}

	return strings.TrimSpace(strings.Join(parts, " "))
}

func (p *Plugin) downloadGroupFile(groupID int64, fileID, filename string) string {
	resp, err := p.napcat("get_group_file_url", map[string]interface{}{"group_id": groupID, "file_id": fileID})
	if err != nil {
		return ""
	}
	respStr, _ := rawString(resp)
	if respStr == "" {
		return ""
	}
	var parsed struct {
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(respStr), &parsed); err != nil || parsed.Data.URL == "" {
		return ""
	}
	if p.filesDir == "" {
		return ""
	}
	os.MkdirAll(p.filesDir, 0755)
	if filename == "" {
		filename = "group_file_" + fileID
	}
	savePath := filepath.Join(p.filesDir, sanitizeFilename(filename))
	httpResp, err := p.httpClient.Get(parsed.Data.URL)
	if err != nil {
		return ""
	}
	defer httpResp.Body.Close()
	data, err := io.ReadAll(httpResp.Body)
	if err != nil || len(data) == 0 {
		return ""
	}
	os.WriteFile(savePath, data, 0644)
	return savePath
}

func (p *Plugin) downloadPrivateFile(userID int64, fileID, filename string) string {
	resp, err := p.napcat("get_private_file_url", map[string]interface{}{"user_id": userID, "file_id": fileID})
	if err != nil {
		return ""
	}
	respStr, _ := rawString(resp)
	if respStr == "" {
		return ""
	}
	var parsed struct {
		Data *struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(respStr), &parsed) != nil || parsed.Data == nil || parsed.Data.URL == "" {
		return ""
	}
	return p.downloadURL(parsed.Data.URL, filename)
}

func (p *Plugin) downloadFromNapCat(fileID, filename string) string {
	if p.filesDir == "" || fileID == "" {
		return ""
	}
	os.MkdirAll(p.filesDir, 0755)
	raw, err := p.napcat("get_file", map[string]interface{}{"file_id": fileID})
	if err != nil {
		return ""
	}
	rawStr, _ := rawString(raw)
	if rawStr == "" {
		return ""
	}
	var resp struct {
		Data *struct {
			File     string `json:"file"`
			FileName string `json:"file_name"`
			FileSize int64  `json:"file_size"`
			Base64   string `json:"base64"`
			URL      string `json:"url"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(rawStr), &resp) != nil || resp.Data == nil {
		return ""
	}
	info := resp.Data
	if info.FileName != "" {
		filename = info.FileName
	}
	if info.Base64 != "" {
		data, err := base64.StdEncoding.DecodeString(info.Base64)
		if err == nil {
			savePath := filepath.Join(p.filesDir, sanitizeFilename(filename))
			os.WriteFile(savePath, data, 0644)
			return savePath
		}
	}
	if info.URL != "" {
		return p.downloadURL(info.URL, filename)
	}
	if info.File != "" {
		savePath := filepath.Join(p.filesDir, sanitizeFilename(filename))
		if err := os.WriteFile(savePath, []byte(info.File), 0644); err == nil {
			return savePath
		}
	}
	return ""
}

func (p *Plugin) downloadURL(fileURL, filename string) string {
	if p.filesDir == "" {
		return ""
	}
	os.MkdirAll(p.filesDir, 0755)
	if filename == "" {
		filename = "file_" + filepath.Base(fileURL)
	}
	savePath := filepath.Join(p.filesDir, sanitizeFilename(filename))
	dlResp, err := p.httpClient.Get(fileURL)
	if err != nil {
		return ""
	}
	defer dlResp.Body.Close()
	data, err := io.ReadAll(dlResp.Body)
	if err != nil || len(data) == 0 {
		return ""
	}
	os.WriteFile(savePath, data, 0644)
	return savePath
}

func sanitizeFilename(name string) string {
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			return '_'
		}
		return r
	}, name)
	return name
}

// ======== Tool Handlers: Document / Video / Web ========

func (p *Plugin) handleReadDocument(args map[string]interface{}) (interface{}, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return map[string]interface{}{
			"content": fmt.Sprintf("文件不存在: %s", path),
		}, nil
	}

	ext := strings.ToLower(filepath.Ext(path))
	textContent := ""

	switch ext {
	case ".txt", ".md", ".csv":
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read file: %w", err)
		}
		textContent = string(data)
	case ".docx", ".doc", ".epub", ".html", ".htm":
		textContent = p.readWithPandoc(path)
	default:
		// Try pandoc first, fallback to libreoffice
		textContent = p.readWithPandoc(path)
		if textContent == "" {
			textContent = p.readWithLibreoffice(path)
		}
		if textContent == "" {
			// last resort: read as plain text
			data, err := os.ReadFile(path)
			if err == nil {
				textContent = string(data)
			}
		}
	}

	if textContent == "" {
		return map[string]interface{}{
			"content": fmt.Sprintf("无法提取文件内容: %s（不支持的文件格式或文件损坏）", path),
		}, nil
	}

	// 截断到 20000 字符
	origLen := len(textContent)
	truncated := origLen > 20000
	if truncated {
		textContent = textContent[:20000]
	}

	result := textContent
	if truncated {
		result += fmt.Sprintf("\n\n...（内容过长，仅显示前 20000 字符，共 %d 字符）", origLen)
	}
	return map[string]interface{}{
		"content":   result,
		"file":      path,
		"truncated": truncated,
	}, nil
}

func (p *Plugin) readWithPandoc(path string) string {
	var out bytes.Buffer
	cmd := exec.Command("pandoc", path, "-t", "plain", "--wrap=none")
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

func (p *Plugin) readWithLibreoffice(path string) string {
	tmpDir, err := os.MkdirTemp("", "lo-doc-*")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(tmpDir)

	cmd := exec.Command("libreoffice", "--headless", "--convert-to", "txt:Text", "--outdir", tmpDir, path)
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return ""
	}

	// 找生成的 txt 文件
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".txt") {
			data, err := os.ReadFile(filepath.Join(tmpDir, e.Name()))
			if err == nil {
				return strings.TrimSpace(string(data))
			}
		}
	}
	return ""
}

func (p *Plugin) handleVideoDownload(args map[string]interface{}) (interface{}, error) {
	url, _ := args["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("url is required")
	}
	infoOnly, _ := args["info_only"].(bool)

	outputDir := filepath.Join(p.agentfsDir, "videos")
	os.MkdirAll(outputDir, 0755)

	if infoOnly {
		var out bytes.Buffer
		cmd := exec.Command("yt-dlp", "--dump-json", url)
		cmd.Stdout = &out
		cmd.Stderr = nil
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("yt-dlp info: %w", err)
		}
		var info struct {
			Title    string `json:"title"`
			Duration int    `json:"duration"`
			Webpage  string `json:"webpage_url"`
			Formats  []struct {
				FormatID string `json:"format_id"`
				Ext      string `json:"ext"`
				Width    int    `json:"width"`
				Height   int    `json:"height"`
				Filesize int64  `json:"filesize"`
				Format   string `json:"format"`
			} `json:"formats"`
		}
		if err := json.Unmarshal(out.Bytes(), &info); err != nil {
			return string(out.String()), nil
		}
		dur := ""
		if info.Duration > 0 {
			dur = fmt.Sprintf("%d分%d秒", info.Duration/60, info.Duration%60)
		}
		lines := []string{fmt.Sprintf("🎬 %s", info.Title)}
		if dur != "" {
			lines = append(lines, fmt.Sprintf("   时长: %s", dur))
		}
		lines = append(lines, fmt.Sprintf("   链接: %s", info.Webpage))
		lines = append(lines, "")
		for _, f := range info.Formats {
			fs := ""
			if f.Filesize > 0 {
				fs = fmt.Sprintf(" (%.1f MB)", float64(f.Filesize)/1048576)
			}
			res := ""
			if f.Width > 0 && f.Height > 0 {
				res = fmt.Sprintf(" %dx%d", f.Width, f.Height)
			}
			lines = append(lines, fmt.Sprintf("  [%s] %s%s%s", f.FormatID, f.Format, res, fs))
		}
		return map[string]interface{}{"content": strings.Join(lines, "\n")}, nil
	}

	// 下载
	outputTmpl := filepath.Join(outputDir, "%(title)s.%(ext)s")
	var out bytes.Buffer
	cmd := exec.Command("yt-dlp", "-o", outputTmpl, "--no-playlist", "--print", "after_move:filepath", url)
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("yt-dlp download: %w", err)
	}

	// 解析 yt-dlp 输出的文件路径
	dlPath := strings.TrimSpace(out.String())
	if dlPath == "" {
		return map[string]interface{}{
			"content": "下载完成，但无法获取文件路径",
		}, nil
	}
	dlFilename := filepath.Base(dlPath)
	var fileSize int64 = 0
	if fi, err := os.Stat(dlPath); err == nil {
		fileSize = fi.Size()
	}
	return map[string]interface{}{
		"content":  fmt.Sprintf("✅ 下载完成: %s\n  大小: %.1f MB\n  路径: %s", dlFilename, float64(fileSize)/1048576, dlPath),
		"file":     dlPath,
		"filename": dlFilename,
	}, nil
}

// ======== NapCat HTTP Client ========

func messageWithMention(userID int64, content interface{}) []interface{} {
	return []interface{}{
		map[string]interface{}{"type": "at", "data": map[string]interface{}{"qq": strconv.FormatInt(userID, 10)}},
		content,
	}
}

func (p *Plugin) napcat(action string, params map[string]interface{}) (interface{}, error) {
	data, _ := json.Marshal(params)
	url := fmt.Sprintf("%s/%s", p.napcatURL, action)

	resp, err := p.httpClient.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("napcat %s: %w", action, err)
	}
	defer resp.Body.Close()

	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("napcat decode %s: %w", action, err)
	}
	return raw, nil
}

func (p *Plugin) setInputStatus(userID int64, eventType int) (interface{}, error) {
	return p.napcat("set_input_status", map[string]interface{}{
		"user_id":    userID,
		"event_type": eventType,
	})
}

func (p *Plugin) startTyping(userID int64) {
	p.typingMu.Lock()
	if _, ok := p.typingMap[userID]; ok {
		p.typingMu.Unlock()
		return
	}
	ts := &typingState{userID: userID, stopCh: make(chan struct{})}
	p.typingMap[userID] = ts
	p.typingMu.Unlock()
	go p.typingLoop(ts)
}

func (p *Plugin) stopTyping(userID int64) {
	p.typingMu.Lock()
	ts, ok := p.typingMap[userID]
	if ok {
		delete(p.typingMap, userID)
	}
	p.typingMu.Unlock()
	if ok {
		close(ts.stopCh)
	}
}

func (p *Plugin) typingLoop(ts *typingState) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	timeout := time.After(30 * time.Second)
	for {
		select {
		case <-ticker.C:
			p.setInputStatus(ts.userID, 1)
		case <-ts.stopCh:
			return
		case <-timeout:
			p.stopTyping(ts.userID)
			return
		}
	}
}

// ======== Helpers ========

// rawString extracts a string from napcat's return type (json.RawMessage or string).
func rawString(v interface{}) (string, bool) {
	switch r := v.(type) {
	case string:
		return r, true
	case json.RawMessage:
		return string(r), true
	case []byte:
		return string(r), true
	}
	return "", false
}

var reAPIKey = regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password)\s*[=:]\s*\S+`)
var reSKKey = regexp.MustCompile(`sk-[a-zA-Z0-9]{20,}`)
var reInternalIP = regexp.MustCompile(`\b(127\.\d{1,3}\.\d{1,3}\.\d{1,3}|10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3})\b`)
var stripCQRe = regexp.MustCompile(`\[CQ:[^\]]*\]|\[mirai:[^\]]*\]`)
var highRiskRe = regexp.MustCompile(`(假如你是|你现在是|请你(扮演|化作|假装|成为)|扮演(一个|一下)|把你自己(想象|当成)|你的(人设|设定)是|穿越(到|回)|你是从.{0,10}(来|穿越)|帮我编(个|一个)故事|写(个|一个)故事让|故事(中|里)的|觉得(这个|这台|这家)?(机器人|AI|助手|ai).{0,8}(怎么样|如何|好不好|评价)|评价(下|一下)?(这个|这台|这家)?(机器人|AI|助手|ai|gpt)|忽略(之前|所有)?(指令|规则|限制|禁令)|解除.{0,6}(限制|规则|约束)|越狱|绕过.{0,6}(限制|审查)|不用(遵守|管)(任何)?(规则|限制|指令)|无视(所有)?(规则|指令)|你是(一个|一只)自由的)`)

func (p *Plugin) sensitiveFilter(text string) string {
	if p.remoteDir != "" {
		text = strings.ReplaceAll(text, p.remoteDir, "[remote]")
	}
	if p.filesDir != "" {
		text = strings.ReplaceAll(text, p.filesDir, "[files]")
	}

	text = reAPIKey.ReplaceAllString(text, "$1=***")
	text = reSKKey.ReplaceAllString(text, "sk-***")
	text = reInternalIP.ReplaceAllString(text, "[IP]")
	return text
}

func convInt64(v interface{}) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case float64:
		return int64(n), nil
	case int:
		return int64(n), nil
	case json.Number:
		return n.Int64()
	case string:
		return strconv.ParseInt(n, 10, 64)
	}
	return 0, fmt.Errorf("cannot convert %T to int64", v)
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{
		name:                 name,
		allowFrom:            make(map[int64]struct{}),
		groupAllowFrom:       make(map[int64]struct{}),
		authByMessageID:      make(map[int64]qqAuthContext),
		outputSignatures:     make(map[string]int),
		maxQQToolCalls:       defaultMaxQQToolCalls,
		maxQQOutputCalls:     defaultMaxQQOutputCalls,
		maxDuplicateSend:     defaultMaxDuplicateSend,
		groupToolAllowlists:  parseGroupToolAllowlists(defaultGroupToolAllowlists),
		privateToolAllowlist: parseToolAllowlist(defaultPublicToolAllowlist),
		downloadTasks:        make([]*DownloadTask, 0),
		typingMap:            make(map[int64]*typingState),
		dmPolicy:             "open",
		groupPolicy:          "open",
	}, nil
}
