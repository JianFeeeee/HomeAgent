package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

// 记忆面：图记忆 / 文档记忆 / 文本记忆 / 知识库 / LLM 源 / 变更追踪。

func (h *Handler) handleMemory(w http.ResponseWriter, r *http.Request) {
	if h.memory == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "memory system not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		userInput := r.URL.Query().Get("q")
		keywords := strings.Split(userInput, ",")
		depth, _ := strconv.Atoi(r.URL.Query().Get("depth"))
		if depth <= 0 {
			depth = 2
		}
		entities, relations, err := h.memory.Recall(keywords, depth)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"entities": entities, "relations": relations})
	case http.MethodPost:
		var req struct {
			Triples []sdk.Triple `json:"triples"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		if err := h.memory.Commit(req.Triples); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]interface{}{"status": "committed", "committed": len(req.Triples)})
	case http.MethodDelete:
		var req struct {
			Criteria map[string]string `json:"criteria"`
			Mode     string            `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		deleted, err := h.memory.Purge(req.Criteria, req.Mode)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"deleted": deleted})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleMemoryContext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.indexer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "indexer not available"})
		return
	}
	userInput := r.URL.Query().Get("q")
	injected, err := h.indexer.BuildContext(userInput)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"context":        h.indexer.FormatContext(injected),
		"summary":        injected.Summary,
		"entities":       injected.Entities,
		"token_estimate": injected.TokenEstimate,
		"tool_prompt":    h.indexer.BuildToolPrompt(),
	})
}

func (h *Handler) handleMemoryTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.indexer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "indexer not available"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"tools":       h.indexer.GetToolDefinitions(),
		"tool_prompt": h.indexer.BuildToolPrompt(),
	})
}

func (h *Handler) handleMemoryGraph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.memory == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "memory system not available"})
		return
	}
	data, err := h.memory.GraphData()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": data})
}

// knowledgeWriteReq 是知识写入请求体（JSON 分支）。
type knowledgeWriteReq struct {
	Name    string                  `json:"name"`
	Content string                  `json:"content"`
	Media   []sdk.KnowledgeMediaRef `json:"media,omitempty"`
}

// knowledgeView 是返回给前端的知识条目视图。
//
// 为何不让前端直接吃 *knowledge.Knowledge：那个结构里有 Path（服务端绝对
// 路径，不该外泄）、Dense（几百 KB 浮点数组）。前端只需要 name/category/
// tags/size/updated_at/media 摘要。
type knowledgeView struct {
	Name      string               `json:"name"`
	Category  string               `json:"category,omitempty"`
	Preview   string               `json:"preview"`
	Size      int                  `json:"size"`
	Tags      []string             `json:"tags,omitempty"`
	UpdatedAt string               `json:"updated_at,omitempty"`
	Media     []knowledgeMediaView `json:"media,omitempty"`
}

type knowledgeMediaView struct {
	Digest string `json:"digest"`
	MIME   string `json:"mime"`
	Kind   string `json:"kind,omitempty"`
}

// knowledgeStatusFor 把内核的 ErrInvalidName / ErrNotFound 映射到正确状态码。
//
// 此前一律 500：把「名称非法」「不存在」这种**调用方能自己纠正**的错报成
// 服务器故障，前端无从区分该改请求还是该报服务器挂了。
func knowledgeStatusFor(err error) (int, string) {
	switch {
	case errors.Is(err, knowledge.ErrInvalidName):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, knowledge.ErrNotFound):
		return http.StatusNotFound, err.Error()
	case errors.Is(err, sdk.ErrMediaUnavailable):
		return http.StatusServiceUnavailable, err.Error()
	default:
		return http.StatusInternalServerError, err.Error()
	}
}

func toKnowledgeView(k *pubsdk.Knowledge, name string) knowledgeView {
	v := knowledgeView{Name: name}
	if k.Content == "" {
		return v
	}
	// 预览按 rune 截断，避免把多字节字符切成乱码
	r := []rune(k.Content)
	if len(r) > 200 {
		v.Preview = string(r[:200]) + "..."
	} else {
		v.Preview = k.Content
	}
	v.Size = len(k.Content)
	return v
}

func (h *Handler) handleKnowledge(w http.ResponseWriter, r *http.Request) {
	if h.knowledge == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		// category 为空 = 全库；非空则限定在该分类子树内（前缀匹配）
		category := q.Get("category")
		if query := strings.TrimSpace(q.Get("q")); query != "" {
			limit := 10
			if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 && v <= 100 {
				limit = v
			}
			var results []*pubsdk.Knowledge
			var err error
			if scoped, ok := h.knowledge.(interface {
				SearchIn(query, category string, topK int) ([]*pubsdk.Knowledge, error)
			}); ok {
				results, err = scoped.SearchIn(query, category, limit)
			} else {
				results, err = h.knowledge.Search(query, limit)
			}
			if err != nil {
				code, msg := knowledgeStatusFor(err)
				writeJSON(w, code, map[string]string{"error": msg})
				return
			}
			views := make([]knowledgeView, 0, len(results))
			for _, k := range results {
				views = append(views, toKnowledgeView(k, k.Name))
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{"results": views, "category": category})
			return
		}
		names, err := h.knowledge.List()
		if err != nil {
			code, msg := knowledgeStatusFor(err)
			writeJSON(w, code, map[string]string{"error": msg})
			return
		}
		if names == nil {
			names = []string{}
		}
		stats := h.knowledge.Stats()
		payload := map[string]interface{}{"names": names, "stats": stats}
		if ds, ok := h.knowledge.(interface {
			DenseStats() map[string]interface{}
		}); ok {
			payload["dense"] = ds.DenseStats()
		}
		writeJSON(w, http.StatusOK, payload)

	case http.MethodPost:
		// multipart 分支：**先看它到底是什么**。
		//
		// 旧实现无论传什么都把字节 utf-8 强转后当 Markdown 存进 content.md：
		// 上传一张 PNG 得到的是一份乱码文本知识，还会在 .index.json 里占一份
		// preview，且没有任何迹象表明出了问题。现在改为：
		//   - 文本类（text/* 或 JSON 字节）→ 走原路径存正文
		//   - 媒体类（image/audio/video）→ 入 media CAS，按 digest 挂到条目上
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "multipart/form-data") {
			h.handleKnowledgeUpload(w, r)
			return
		}

		var req knowledgeWriteReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
			return
		}
		if strings.TrimSpace(req.Content) == "" && len(req.Media) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "content or media is required"})
			return
		}
		if err := h.knowledge.AddWithMedia(req.Name, req.Content, req.Media); err != nil {
			code, msg := knowledgeStatusFor(err)
			writeJSON(w, code, map[string]string{"error": msg})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]interface{}{
			"status": "created", "name": req.Name, "media": len(req.Media),
		})

	case http.MethodDelete:
		name := r.URL.Query().Get("name")
		if name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name query param required"})
			return
		}
		if err := h.knowledge.Remove(name); err != nil {
			// 之前所有失败一律 404，包括名称非法（400 的事）与真实 IO 错误
			// （500 的事）。"啥都没删"和"服务器坏了"被混为一谈。
			code, msg := knowledgeStatusFor(err)
			writeJSON(w, code, map[string]string{"error": msg})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"status": "deleted", "name": name})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// maxKnowledgeUploadBytes 是知识库单次上传的体积上限。
// 与 media CAS 的定位一致：知识条目的媒体是引用，不该拖着一堆原始字节。
const maxKnowledgeUploadBytes = 32 << 20

// handleKnowledgeUpload 处理 multipart 上传，按实际类型分流：媒体入 CAS，
// 文本存正文。绝不把二进制当文本存。
func (h *Handler) handleKnowledgeUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxKnowledgeUploadBytes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart: " + err.Error()})
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}

	files := r.MultipartForm.File["file"]
	// 也接受通用字段名，避免前端只有 file 字段名不匹配时静默走成"无媒体"
	if len(files) == 0 {
		files = r.MultipartForm.File["media"]
	}
	if len(files) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file field required"})
		return
	}
	if len(files) > 16 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "最多 16 个文件"})
		return
	}

	var (
		media    []sdk.KnowledgeMediaRef
		texts    []string
		rejected []string
	)
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "open " + fh.Filename + ": " + err.Error()})
			return
		}
		data, err := io.ReadAll(io.LimitReader(f, maxKnowledgeUploadBytes+1))
		f.Close()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read " + fh.Filename + ": " + err.Error()})
			return
		}
		if len(data) == 0 {
			rejected = append(rejected, fh.Filename+": 空文件")
			continue
		}
		if len(data) > maxKnowledgeUploadBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge,
				map[string]string{"error": fmt.Sprintf("%s 超过 %d 上限", fh.Filename, maxKnowledgeUploadBytes)})
			return
		}

		mime := fh.Header.Get("Content-Type")
		if mime == "" {
			mime = contentTypeByExt(strings.ToLower(filepath.Ext(fh.Filename)))
		}
		// 按**探测到的真实类型**判定，而不是信客户端给的 Content-Type：
		// 声明 text/plain 的 PNG 曾是真实场景，光看头会把二进制当文本存。
		detected := http.DetectContentType(data)
		if isMediaMIME(mime) || isMediaMIME(detected) {
			useMIME := detected
			if detected == "application/octet-stream" {
				useMIME = mime
			}
			if h.mediaStore == nil {
				writeJSON(w, http.StatusServiceUnavailable,
					map[string]string{"error": "媒体存储未初始化，无法保存图片/音视频"})
				return
			}
			digest, err := h.mediaStore.Put(data, useMIME, "webui_knowledge")
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "store media: " + err.Error()})
				return
			}
			media = append(media, sdk.KnowledgeMediaRef{Digest: digest, MIME: useMIME, Kind: mediaKindOf(useMIME)})
			continue
		}
		// 文本类：确认是合法 UTF-8 才当正文，否则拒绝并说明原因
		if !utf8.Valid(data) {
			rejected = append(rejected, fmt.Sprintf("%s: 非文本内容且无法识别为媒体（type=%q）", fh.Filename, useMIMEOr(detected, mime)))
			continue
		}
		texts = append(texts, string(data))
	}

	content := strings.TrimSpace(r.FormValue("content"))
	if content == "" {
		content = strings.Join(texts, "\n\n")
	}
	if strings.TrimSpace(content) == "" && len(media) == 0 {
		msg := "没有可写入的内容"
		if len(rejected) > 0 {
			msg += "：" + strings.Join(rejected, "; ")
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}

	if err := h.knowledge.AddWithMedia(name, content, media); err != nil {
		code, msg := knowledgeStatusFor(err)
		writeJSON(w, code, map[string]string{"error": msg})
		return
	}
	resp := map[string]interface{}{"status": "created", "name": name, "media": media}
	if len(rejected) > 0 {
		resp["rejected"] = rejected
	}
	writeJSON(w, http.StatusCreated, resp)
}

func useMIMEOr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func isMediaMIME(m string) bool {
	return strings.HasPrefix(m, "image/") || strings.HasPrefix(m, "audio/") || strings.HasPrefix(m, "video/")
}

func mediaKindOf(m string) string {
	switch {
	case strings.HasPrefix(m, "image/"):
		return "image"
	case strings.HasPrefix(m, "audio/"):
		return "audio"
	case strings.HasPrefix(m, "video/"):
		return "video"
	}
	return "file"
}

func (h *Handler) handleTextMemory(w http.ResponseWriter, r *http.Request) {
	if h.textMem == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "text memory not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		recent, _ := h.textMem.RecentEvents(50)
		stats := h.textMem.Stats()
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"stats":  stats,
			"recent": recent,
		})
	case http.MethodDelete:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "not_implemented"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleAdapters(w http.ResponseWriter, r *http.Request) {
	if h.adapter == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "lua vm not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{"adapters": h.adapter.List()})
	case http.MethodPost:
		var req struct {
			Name string `json:"name"`
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		if err := h.adapter.Load(req.Name, req.Code); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"status": "loaded", "name": req.Name})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleAdapterByID(w http.ResponseWriter, r *http.Request) {
	if h.adapter == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "lua vm not available"})
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/v1/adapters/")
	if name == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		for _, a := range h.adapter.List() {
			if a.Name == name {
				writeJSON(w, http.StatusOK, a)
				return
			}
		}
		http.NotFound(w, r)
	case http.MethodDelete:
		if err := h.adapter.Remove(name); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "adapter not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "name": name})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleNetwork(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"network_status": "monitoring",
		"endpoints":      h.config.Get().Defaults.LLMEndpoints,
	})
}

func (h *Handler) handleTracker(w http.ResponseWriter, r *http.Request) {
	if h.tracker == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "tracker not available"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/tracker")
	path = strings.TrimPrefix(path, "/")

	switch {
	case path == "changesets" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"changesets": h.tracker.ChangeSets(),
			"count":      len(h.tracker.ChangeSets()),
		})
	case path == "rollback" && r.Method == http.MethodPost:
		if err := h.tracker.Rollback(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "rollback_complete"})
	case path == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"stats":       h.tracker.Stats(),
			"has_changes": h.tracker.HasChanges(),
			"changesets":  len(h.tracker.ChangeSets()),
		})
	case path == "" && r.Method == http.MethodDelete:
		if err := h.tracker.Rollback(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}
