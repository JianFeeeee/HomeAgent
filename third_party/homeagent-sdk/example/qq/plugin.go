package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

type SavedMessage struct {
	LocalID     int64  `json:"local_id"`
	MessageID   int64  `json:"message_id"`
	UserID      int64  `json:"user_id"`
	Nickname    string `json:"nickname"`
	GroupID     int64  `json:"group_id,omitempty"`
	GroupName   string `json:"group_name,omitempty"`
	MessageType string `json:"message_type"`
	Text        string `json:"text"`
	Time        int64  `json:"time"`
}

const maxMessages = 2000

type Plugin struct {
	name           string
	sdk            *sdk.PluginSDK
	mu             sync.RWMutex
	messages       []*SavedMessage
	nextID         int64
	listenAddr     string
	napcatURL      string
	remoteDir      string
	filesDir       string
	adminID        int64
	botID          int64
	botNickname    string
	dmPolicy       string
	groupPolicy    string
	allowFrom      map[int64]struct{}
	groupAllowFrom map[int64]struct{}
	srv            *http.Server
	groupNameCache map[int64]string
}

func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{
		name:           name,
		nextID:         1,
		messages:       make([]*SavedMessage, 0, maxMessages),
		groupNameCache: make(map[int64]string),
		allowFrom:      make(map[int64]struct{}),
		groupAllowFrom: make(map[int64]struct{}),
		dmPolicy:       "open",
		groupPolicy:    "open",
	}, nil
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s

	s.Settings().RegisterDef(sdk.ConfigDef{Key: "plugin.qq.listen", Default: "0.0.0.0:25580", Type: "string", DisplayName: "监听地址", Description: "Webhook HTTP 监听地址", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "plugin.qq.napcat_url", Default: "http://127.0.0.1:3000", Type: "string", DisplayName: "NapCat 地址", Description: "NapCat HTTP API 基础 URL", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "plugin.qq.admin", Default: "", Type: "string", DisplayName: "管理员 QQ", Description: "管理员 QQ 号，收到其消息时标记【重要！老大消息】", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "plugin.qq.dm_policy", Default: "open", Type: "string", DisplayName: "私聊策略", Description: "open / allowlist / disabled", Category: "qq", Options: []string{"open", "allowlist", "disabled"}})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "plugin.qq.allow_from", Default: "", Type: "string", DisplayName: "私聊白名单", Description: "允许私聊机器人的 QQ 号列表，逗号分隔", Category: "qq"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "plugin.qq.group_policy", Default: "open", Type: "string", DisplayName: "群聊策略", Description: "open / allowlist / disabled", Category: "qq", Options: []string{"open", "allowlist", "disabled"}})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "plugin.qq.group_allow_from", Default: "", Type: "string", DisplayName: "群聊白名单", Description: "允许接入的群号列表，逗号分隔", Category: "qq"})

	settings := s.Settings()

	p.listenAddr = getSetting[string](settings, "listen", "0.0.0.0:25580")
	p.napcatURL = strings.TrimRight(getSetting[string](settings, "napcat_url", "http://127.0.0.1:3000"), "/")
	p.adminID = getSetting[int64](settings, "admin", 0)
	p.dmPolicy = normalizePolicy(getSetting[string](settings, "dm_policy", "open"))
	p.groupPolicy = normalizePolicy(getSetting[string](settings, "group_policy", "open"))
	p.allowFrom = parseIDSet(getSetting[string](settings, "allow_from", ""))
	p.groupAllowFrom = parseIDSet(getSetting[string](settings, "group_allow_from", ""))

	// 从 NapCat 自动获取 Bot 身份
	p.fetchBotInfo()

	tp := p.name + "_"

	botInfo := ""
	if p.botNickname != "" {
		botInfo = fmt.Sprintf("你的QQ昵称是%s", p.botNickname)
		if p.botID > 0 {
			botInfo += fmt.Sprintf("，QQ号是%d", p.botID)
		}
		botInfo += "。"
	}

	// ---- 消息 ----
	p.regTool(s, tp+"get_message", botInfo+"获取通过中断通知的QQ消息正文。local_id来自中断文字中的id号。", map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{
			"local_id": map[string]interface{}{"type": "integer", "description": "本地消息ID"},
		}, "required": []string{"local_id"},
	}, p.handleGetMessage)

	p.regTool(s, tp+"send_private_msg", "发送QQ私聊消息", map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{
			"user_id": map[string]interface{}{"type": "integer", "description": "目标QQ号"},
			"message": map[string]interface{}{"type": "string", "description": "消息内容"},
		}, "required": []string{"user_id", "message"},
	}, p.handleSendPrivate)

	p.regTool(s, tp+"send_group_msg", "发送QQ群消息", map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{
			"group_id": map[string]interface{}{"type": "integer", "description": "目标群号"},
			"message":  map[string]interface{}{"type": "string", "description": "消息内容"},
		}, "required": []string{"group_id", "message"},
	}, p.handleSendGroup)

	p.regTool(s, tp+"send_file", "发送文件/图片到QQ（私聊或群聊）。文件先复制到remote目录供NapCat容器访问。", map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{
			"group_id": map[string]interface{}{"type": "integer", "description": "目标群号（与user_id二选一）"},
			"user_id":  map[string]interface{}{"type": "integer", "description": "目标QQ号（与group_id二选一）"},
			"file":     map[string]interface{}{"type": "string", "description": "本地文件路径"},
			"name":     map[string]interface{}{"type": "string", "description": "文件名（可选，默认取原文件名）"},
			"as_image": map[string]interface{}{"type": "boolean", "description": "作为图片发送（true）还是作为文件（false，默认）"},
		},
	}, p.handleSendFile)

	p.regTool(s, tp+"get_history", "获取QQ群聊/私聊历史消息，用于回顾之前的对话上下文", map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{
			"group_id": map[string]interface{}{"type": "integer", "description": "群号（与user_id二选一）"},
			"user_id":  map[string]interface{}{"type": "integer", "description": "QQ号私聊历史（与group_id二选一）"},
			"count":    map[string]interface{}{"type": "integer", "description": "拉取条数，默认10"},
		}, "required": []string{},
	}, p.handleGetHistory)

	// ---- 查询 ----
	p.regTool(s, tp+"get_groups", "获取QQ群列表，可按关键词搜索群名", map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{
			"keyword": map[string]interface{}{"type": "string", "description": "搜索关键词（可选）"},
		},
	}, p.handleGetGroups)

	p.regTool(s, tp+"get_friends", "获取QQ好友列表，可按昵称/备注关键词搜索", map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{
			"keyword": map[string]interface{}{"type": "string", "description": "搜索关键词（可选）"},
		},
	}, p.handleGetFriends)

	p.regTool(s, tp+"resolve_name", "将QQ号或群号解析为可读的用户昵称或群名称", map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{
			"user_id": map[string]interface{}{"type": "integer", "description": "QQ号（与group_id二选一）"},
			"group_id": map[string]interface{}{"type": "integer", "description": "群号（与user_id二选一）"},
		},
	}, p.handleResolveName)

	p.regTool(s, tp+"get_group_member_info", "获取QQ群成员详细信息", map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{
			"group_id": map[string]interface{}{"type": "integer", "description": "群号"},
			"user_id":  map[string]interface{}{"type": "integer", "description": "QQ号"},
		}, "required": []string{"group_id", "user_id"},
	}, p.handleGetGroupMemberInfo)

	// ---- 群管理 ----
	p.regTool(s, tp+"group_manage", "QQ群综合管理。通过command参数执行各种操作：leave退群, kick踢人, ban禁言, unban解禁, rename改名, mute-all全员禁言, set-card设名片, set-admin设管理, set-title设头衔, member-list成员列表, group-info群详情, member-info成员详情, at-all-remain@全体剩余, msg-history消息历史, recall撤回, pin-msg精华, list-files文件列表, pending-requests待处理请求, folder-create创建文件夹。注意：leave/kick/ban/unban/mute-all/set-admin等破坏性操作必须先请示管理员确认后再执行。", map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{
			"command":  map[string]interface{}{"type": "string", "description": "操作命令"},
			"group_id": map[string]interface{}{"type": "integer", "description": "群号"},
			"user_id":  map[string]interface{}{"type": "integer", "description": "QQ号（踢人/禁言/设名片等需要）"},
			"message_id": map[string]interface{}{"type": "integer", "description": "消息ID（撤回/精华）"},
			"name":     map[string]interface{}{"type": "string", "description": "群名称（rename）或文件夹名（folder-create）"},
			"card":     map[string]interface{}{"type": "string", "description": "群名片（set-card）"},
			"title":    map[string]interface{}{"type": "string", "description": "群头衔（set-title）"},
			"enable":   map[string]interface{}{"type": "boolean", "description": "启用/禁用（set-admin/mute-all）"},
			"minutes":  map[string]interface{}{"type": "integer", "description": "禁言分钟数（ban），0=解禁"},
			"count":    map[string]interface{}{"type": "integer", "description": "消息条数（msg-history），默认10"},
			"folder_id": map[string]interface{}{"type": "string", "description": "文件夹ID（list-files）"},
			"reject_add": map[string]interface{}{"type": "boolean", "description": "踢出时拒绝加群（kick）"},
			"confirm": map[string]interface{}{"type": "boolean", "description": "高风险操作确认标记。执行 leave/kick/ban/unban/rename/mute-all/set-card/set-admin/set-title/recall/pin-msg/folder-create 时必须传 true"},
		},
	}, p.handleGroupManage)

	p.regTool(s, tp+"friend_action", "QQ好友管理：delete删除好友, block拉黑（删好友+从所有群踢出+拒绝加群）, approve-friend同意好友请求, reject-friend拒绝好友请求, list-friends列出好友。注意：涉及删除/拉黑的操作必须请示管理员确认后再执行，未经授权不可操作。", map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{
			"command": map[string]interface{}{"type": "string", "description": "操作: delete|block|approve-friend|reject-friend|list-friends"},
			"user_id": map[string]interface{}{"type": "integer", "description": "目标QQ号"},
			"flag":    map[string]interface{}{"type": "string", "description": "好友请求flag（approve-friend/reject-friend需要）"},
			"remark":  map[string]interface{}{"type": "string", "description": "好友备注（approve-friend可选）"},
			"group_id": map[string]interface{}{"type": "integer", "description": "仅从指定群踢出（block配合）"},
			"confirm": map[string]interface{}{"type": "boolean", "description": "高风险操作确认标记。执行 delete/block/approve-friend/reject-friend 时必须传 true"},
		},
	}, p.handleFriendAction)

	// ---- 文件 ----
	p.regTool(s, tp+"get_group_files", "查询群文件列表、搜索文件、下载文件到本地。操作: list列出, search搜索, download下载", map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{
			"group_id": map[string]interface{}{"type": "integer", "description": "群号"},
			"command":  map[string]interface{}{"type": "string", "description": "操作: list|search|download"},
			"folder_id": map[string]interface{}{"type": "string", "description": "文件夹ID（list指定文件夹）"},
			"keyword":  map[string]interface{}{"type": "string", "description": "搜索关键词（search）"},
			"file_id":  map[string]interface{}{"type": "string", "description": "文件ID（download）"},
			"filename": map[string]interface{}{"type": "string", "description": "保存文件名（download可选）"},
		},
	}, p.handleGetGroupFiles)

	p.regTool(s, tp+"upload_group_file", "上传文件到QQ群（通过base64编码发送，同时出现在群消息和群文件柜）", map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{
			"group_id": map[string]interface{}{"type": "integer", "description": "目标群号"},
			"file":     map[string]interface{}{"type": "string", "description": "本地文件路径"},
			"name":     map[string]interface{}{"type": "string", "description": "文件名（可选，默认取原文件名）"},
		}, "required": []string{"group_id", "file"},
	}, p.handleUploadGroupFile)

	// ---- 附加 ----
	p.regTool(s, tp+"send_like", "给QQ好友点赞/戳一戳", map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{
			"user_id": map[string]interface{}{"type": "integer", "description": "目标QQ号"},
			"times":   map[string]interface{}{"type": "integer", "description": "点赞次数1-20，默认1"},
		}, "required": []string{"user_id"},
	}, p.handleSendLike)

	p.regTool(s, tp+"ocr_image", "对QQ图片进行文字识别（调用NapCat OCR / 本地Tesseract）", map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{
			"image": map[string]interface{}{"type": "string", "description": "图片路径（本地路径或URL）"},
			"lang":  map[string]interface{}{"type": "string", "description": "语言（chi_sim+eng默认, eng, chi_sim, chi_tra）"},
		}, "required": []string{"image"},
	}, p.handleOcrImage)

	s.RegisterStageOwnTools(sdk.StageBeforeToolcall, p.beforeOwnToolcall)

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

	log.Printf("[qq] plugin started: %s (%d tools)", p.name, 14)
	return nil
}

func (p *Plugin) Stop() error {
	if p.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.srv.Shutdown(ctx)
	}
	return nil
}

func (p *Plugin) regTool(s *sdk.PluginSDK, name, desc string, params map[string]interface{}, handler sdk.ToolHandler) {
	s.RegisterTool(name, sdk.ToolDef{Name: name, Description: desc, Parameters: params}, handler)
}

// ======== Bot Identity ========

func (p *Plugin) fetchBotInfo() {
	resp, err := p.rawNapcat("get_login_info", nil)
	if err != nil {
		log.Printf("[qq] fetch login info: %v", err)
		return
	}
	var info struct {
		Status   string `json:"status"`
		Data     *struct {
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
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
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

func (p *Plugin) beforeOwnToolcall(ctx *sdk.StageContext) error {
	ctx.Lock()
	defer ctx.Unlock()
	if len(ctx.ToolCalls) == 0 {
		return nil
	}
	tc := &ctx.ToolCalls[0]
	switch tc.Name {
	case p.name + "_send_private_msg", p.name + "_send_group_msg":
		if msg, ok := tc.Arguments["message"].(string); ok {
			tc.Arguments["message"] = p.sensitiveFilter(msg)
		}
	case p.name + "_send_file", p.name + "_upload_group_file":
		if file, ok := tc.Arguments["file"].(string); ok {
			tc.Arguments["file"] = p.sensitiveFilter(file)
		}
	}
	if tc.Name == p.name+"_group_manage" {
		cmd, _ := tc.Arguments["command"].(string)
		if requiresConfirmGroupCommand(cmd) {
			if ok, _ := tc.Arguments["confirm"].(bool); !ok {
				msg := fmt.Sprintf("QQ群管理命令 %s 属于高风险操作，必须显式传入 confirm=true 后才能执行", cmd)
				ctx.Response = &msg
				return nil
			}
		}
	}
	if tc.Name == p.name+"_friend_action" {
		cmd, _ := tc.Arguments["command"].(string)
		if requiresConfirmFriendCommand(cmd) {
			if ok, _ := tc.Arguments["confirm"].(bool); !ok {
				msg := fmt.Sprintf("QQ好友管理命令 %s 属于高风险操作，必须显式传入 confirm=true 后才能执行", cmd)
				ctx.Response = &msg
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

func (p *Plugin) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var evt struct {
		PostType    string      `json:"post_type"`
		MessageType string      `json:"message_type,omitempty"`
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

	text := evt.RawMessage
	if text == "" {
		if s, ok := evt.Message.(string); ok {
			text = s
		}
	}
	if text == "" {
		w.WriteHeader(http.StatusOK)
		return
	}

	if evt.MessageType == "private" {
		if !p.isDMAllowed(evt.UserID) {
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	if evt.MessageType == "group" {
		if !p.isGroupAllowed(evt.GroupID) {
			w.WriteHeader(http.StatusOK)
			return
		}
		// 群消息必须 @ 机器人才响应
		if p.botID > 0 && !p.isAtBot(evt.Message) {
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	nickname := ""
	if evt.Sender != nil {
		nickname = evt.Sender.Nickname
		if evt.Sender.Card != "" {
			nickname = evt.Sender.Card
		}
	}

	p.mu.Lock()
	localID := p.nextID
	p.nextID++

	msg := &SavedMessage{
		LocalID: localID, UserID: evt.UserID, Nickname: nickname,
		GroupID: evt.GroupID, MessageType: evt.MessageType, Text: text, Time: evt.Time,
	}
	groupName := ""
	if evt.MessageType == "group" {
		if n, ok := p.groupNameCache[evt.GroupID]; ok {
			groupName = n
		} else {
			groupName = fmt.Sprintf("%d", evt.GroupID)
		}
		msg.GroupName = groupName
	}
	p.messages = append(p.messages, msg)
	if len(p.messages) > maxMessages {
		p.messages = p.messages[1:]
	}

	tp := p.name + "_"
	var interrupt string
	if evt.MessageType == "group" {
		interrupt = fmt.Sprintf("来自%s的（%s）群聊消息，通过id%d使用%sget_message工具获取消息正文。获取后必须使用%ssend_group_msg工具回复该群聊", nickname, groupName, localID, tp, tp)
	} else {
		interrupt = fmt.Sprintf("来自%s的私聊消息，通过id%d使用%sget_message工具获取消息正文。获取后必须使用%ssend_private_msg工具回复对方", nickname, localID, tp, tp)
	}
	if p.adminID > 0 && evt.UserID == p.adminID {
		interrupt = "【重要！老大消息】" + interrupt
	}
	p.mu.Unlock()

	if p.sdk != nil {
		p.sdk.InjectInterruptText(p.name, p.name, interrupt)
	}
	w.WriteHeader(http.StatusOK)
}

// ======== Tool Handlers ========

func (p *Plugin) handleGetMessage(args map[string]interface{}) (interface{}, error) {
	id, err := convInt64(args["local_id"])
	if err != nil {
		return map[string]interface{}{
			"content": fmt.Sprintf("无效的 local_id 参数，请传入整数类型的消息ID"),
			"error":   err.Error(),
		}, nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, m := range p.messages {
		if m.LocalID == id {
			return m, nil
		}
	}
	return map[string]interface{}{
		"content":   fmt.Sprintf("消息 %d 未找到。可能的原因：消息已被处理过期，或插件重启后本地缓存已清空。请使用 qq_get_history 工具从 NapCat 拉取历史消息。", id),
		"local_id":  id,
		"not_found": true,
	}, nil
}

func (p *Plugin) handleSendPrivate(args map[string]interface{}) (interface{}, error) {
	uid, _ := convInt64(args["user_id"])
	msg := p.sensitiveFilter(args["message"].(string))
	return p.napcat("send_private_msg", map[string]interface{}{"user_id": uid, "message": msg})
}

func (p *Plugin) handleSendGroup(args map[string]interface{}) (interface{}, error) {
	gid, _ := convInt64(args["group_id"])
	msg := p.sensitiveFilter(args["message"].(string))
	return p.napcat("send_group_msg", map[string]interface{}{"group_id": gid, "message": msg})
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
	name = p.sensitiveFilter(name)
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

	data, err := p.napcat(endpoint, params)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (p *Plugin) handleGetGroups(args map[string]interface{}) (interface{}, error) {
	return p.napcat("get_group_list", map[string]interface{}{})
}

func (p *Plugin) handleGetFriends(args map[string]interface{}) (interface{}, error) {
	return p.napcat("get_friend_list", map[string]interface{}{})
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
		if gid, err := convInt64(args["group_id"]); err == nil {
			p.napcat("set_group_kick", map[string]interface{}{"group_id": gid, "user_id": uid, "reject_add_request": true})
		} else {
			grps, _ := p.napcat("get_group_list", map[string]interface{}{})
			if list, ok := grps.([]interface{}); ok {
				for _, g := range list {
					if m, ok := g.(map[string]interface{}); ok {
						if gid, ok := m["group_id"].(float64); ok {
							p.napcat("set_group_kick", map[string]interface{}{"group_id": int64(gid), "user_id": uid, "reject_add_request": true})
						}
					}
				}
			}
		}
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
		httpResp, err := http.Get(dlURL)
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

func (p *Plugin) handleUploadGroupFile(args map[string]interface{}) (interface{}, error) {
	gid, _ := convInt64(args["group_id"])
	filePath, _ := args["file"].(string)
	name, _ := args["name"].(string)
	if name == "" {
		name = filepath.Base(filePath)
	}
	name = p.sensitiveFilter(name)

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	b64 := fmt.Sprintf("base64://%s", string(data))

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

func (p *Plugin) handleOcrImage(args map[string]interface{}) (interface{}, error) {
	image, _ := args["image"].(string)
	lang, _ := args["lang"].(string)
	if lang == "" {
		lang = "chi_sim+eng"
	}

	// If local file, copy to remote dir for NapCat
	if !strings.HasPrefix(image, "http://") && !strings.HasPrefix(image, "https://") {
		dest := filepath.Join(p.remoteDir, filepath.Base(image))
		src, err := os.ReadFile(image)
		if err == nil {
			os.WriteFile(dest, src, 0644)
			image = fmt.Sprintf("file:///app/files/%s", filepath.Base(image))
		}
	}

	return p.napcat("ocr_image", map[string]interface{}{"image": image})
}

// ======== NapCat HTTP Client ========

func (p *Plugin) napcat(action string, params map[string]interface{}) (interface{}, error) {
	data, _ := json.Marshal(params)
	url := fmt.Sprintf("%s/%s", p.napcatURL, action)

	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("napcat %s: %w", action, err)
	}
	defer resp.Body.Close()

	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("napcat decode %s: %w", action, err)
	}
	return string(raw), nil
}

// ======== Helpers ========

func main() {}

var reAPIKey = regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password)\s*[=:]\s*\S+`)
var reSKKey = regexp.MustCompile(`sk-[a-zA-Z0-9]{20,}`)
var reInternalIP = regexp.MustCompile(`\b(127\.\d{1,3}\.\d{1,3}\.\d{1,3}|10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3})\b`)

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
