package config

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/meta"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
	_ "github.com/mattn/go-sqlite3"
)

type ConfigDef struct {
	Key         string   `json:"key"`
	Default     string   `json:"default"`
	Description string   `json:"description"`
	Type        string   `json:"type"` // string, int, bool, duration, password, select, text
	DisplayName string   `json:"display_name"`
	Placeholder string   `json:"placeholder,omitempty"`
	Options     []string `json:"options,omitempty"`
	Hidden      bool     `json:"hidden,omitempty"`
	Category    string   `json:"category,omitempty"`
}

type ConfigRegistry struct {
	mu          sync.RWMutex
	db          *sql.DB
	dbPath      string
	defs        map[string]*ConfigDef
	llmSnapFile string
}

func NewConfigRegistry(dbPath string) *ConfigRegistry {
	if dbPath == "" {
		dbPath = ":memory:"
	}
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		panic(fmt.Sprintf("open config db: %v", err))
	}
	db.Exec("PRAGMA journal_mode=WAL")
	r := &ConfigRegistry{db: db, dbPath: dbPath, defs: make(map[string]*ConfigDef)}
	r.initCoreTable()
	return r
}

func (r *ConfigRegistry) initCoreTable() {
	r.db.Exec(`CREATE TABLE IF NOT EXISTS config (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`)
	r.db.Exec(`CREATE TABLE IF NOT EXISTS disabled_plugins (
		name       TEXT PRIMARY KEY,
		disabled_at TEXT NOT NULL,
		disabled_by TEXT NOT NULL DEFAULT ''
	)`)
}

func (r *ConfigRegistry) AddDisabledPlugin(name, by string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec(`INSERT OR REPLACE INTO disabled_plugins (name, disabled_at, disabled_by) VALUES (?, datetime('now'), ?)`, name, by)
	return err
}

func (r *ConfigRegistry) RemoveDisabledPlugin(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec(`DELETE FROM disabled_plugins WHERE name = ?`, name)
	return err
}

// RemovePlugin 卸载插件时清理其全部配置痕迹：删除配置项定义
// （defs 中 plugin.<name>.*，即该插件 RegisterDef 注册的配置项）并删除
// 插件配置表（config_<name>，含用户设置值）。卸载后该插件配置区完全消失；
// 与插件自身的 onRemove 回调（清理数据文件）配合完成删除清理。
func (r *ConfigRegistry) RemovePlugin(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	prefix := "plugin." + name + "."
	for k := range r.defs {
		if strings.HasPrefix(k, prefix) {
			delete(r.defs, k)
		}
	}
	if _, err := r.db.Exec(`DELETE FROM config WHERE key LIKE ?`, prefix+"%"); err != nil {
		return err
	}
	table := r.pluginTableName(name)
	_, err := r.db.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, table))
	return err
}

type DisabledPluginInfo struct {
	Name       string `json:"name"`
	DisabledAt string `json:"disabled_at"`
	DisabledBy string `json:"disabled_by"`
}

func (r *ConfigRegistry) ListDisabledPlugins() ([]DisabledPluginInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rows, err := r.db.Query(`SELECT name, disabled_at, disabled_by FROM disabled_plugins ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []DisabledPluginInfo
	for rows.Next() {
		var info DisabledPluginInfo
		if err := rows.Scan(&info.Name, &info.DisabledAt, &info.DisabledBy); err != nil {
			return nil, err
		}
		list = append(list, info)
	}
	return list, rows.Err()
}

func (r *ConfigRegistry) IsPluginDisabled(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var count int
	r.db.QueryRow(`SELECT COUNT(*) FROM disabled_plugins WHERE name = ?`, name).Scan(&count)
	return count > 0
}

func (r *ConfigRegistry) ensurePluginTable(name string) {
	table := r.pluginTableName(name)
	r.db.Exec(fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`, table))
}

func (r *ConfigRegistry) pluginTableName(name string) string {
	safe := strings.Map(func(c rune) rune {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' {
			return c
		}
		return '_'
	}, strings.ToLower(name))
	return "config_" + safe
}

func (r *ConfigRegistry) Register(key string, value interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.db.Exec(`INSERT OR IGNORE INTO config (key, value) VALUES (?, ?)`, key, fmt.Sprint(value))
}

func (r *ConfigRegistry) RegisterDefault(key string, value interface{}) {
	r.Register(key, value)
}

func (r *ConfigRegistry) RegisterDef(def ConfigDef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defs[def.Key] = &def
	r.db.Exec(`INSERT OR IGNORE INTO config (key, value) VALUES (?, ?)`, def.Key, def.Default)
}

func (r *ConfigRegistry) GetDef(key string) *ConfigDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defs[key]
}

// sourceFieldDefs 定义 source 类型配置的字段元数据
var sourceFieldDefs = []struct {
	Field       string
	Type        string
	DisplayName string
}{
	{"base_url", "string", "API 地址"},
	{"model", "string", "模型"},
	{"api_key", "password", "API 密钥"},
	{"thinking_enabled", "bool", "深度思考"},
	{"adapter", "string", "适配器"},
	{"adapter_path", "string", "适配器路径"},
	{"max_concurrent", "int", "并发上限"},
	{"priority", "int", "AUTO 优先级（大者优先）"},
}

// registerSourceDefs 注册 core.llm.sources.<name>.* 的 ConfigDef
func (r *ConfigRegistry) registerSourceDefs(name string) {
	for _, fd := range sourceFieldDefs {
		key := "core.llm.sources." + name + "." + fd.Field
		if _, exists := r.defs[key]; exists {
			continue
		}
		r.defs[key] = &ConfigDef{
			Key:         key,
			Default:     "",
			Type:        fd.Type,
			DisplayName: name + " " + fd.DisplayName,
			Category:    "sources",
		}
	}
}

// scanAndRegisterSourceDefsLocked 扫描 config DB 中已有的 core.llm.sources.<name>.* 键并注册 defs（调用方已持锁）
func (r *ConfigRegistry) scanAndRegisterSourceDefsLocked() {
	seen := make(map[string]bool)
	rows, err := r.db.Query(`SELECT key FROM config WHERE key LIKE 'core.llm.sources.%.base_url'`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			continue
		}
		rest := strings.TrimPrefix(k, "core.llm.sources.")
		name := strings.TrimSuffix(rest, ".base_url")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		r.defsLockedRegisterSource(name)
	}
}
func (r *ConfigRegistry) defsLockedRegisterSource(name string) {
	for _, fd := range sourceFieldDefs {
		key := "core.llm.sources." + name + "." + fd.Field
		if _, exists := r.defs[key]; exists {
			continue
		}
		r.defs[key] = &ConfigDef{
			Key:         key,
			Default:     "",
			Type:        fd.Type,
			DisplayName: name + " " + fd.DisplayName,
			Category:    "sources",
		}
	}
}

func (r *ConfigRegistry) ListDefs(prefix string) []*ConfigDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []*ConfigDef
	for _, def := range r.defs {
		if strings.HasPrefix(def.Key, prefix) {
			result = append(result, def)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result
}

func (r *ConfigRegistry) Get(key string) (interface{}, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var val string
	err := r.db.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("config key %q not found", key)
	}
	if err != nil {
		return nil, err
	}
	return val, nil
}

func (r *ConfigRegistry) Set(key string, value interface{}) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.HasPrefix(key, "core.llm.") {
		r.writeLLMSnapshotLocked()
	}
	_, err := r.db.Exec(`INSERT OR REPLACE INTO config (key, value) VALUES (?, ?)`, key, fmt.Sprint(value))
	if err == nil && strings.HasPrefix(key, "core.llm.sources.") {
		rest := strings.TrimPrefix(key, "core.llm.sources.")
		parts := strings.SplitN(rest, ".", 2)
		if len(parts) == 2 && parts[1] != "" {
			r.defsLockedRegisterSource(parts[0])
		}
	}
	return err
}

func (r *ConfigRegistry) List(prefix string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var keys []string
	q := `SELECT key FROM config WHERE key LIKE ? ORDER BY key`
	like := prefix + "%"
	rows, err := r.db.Query(q, like)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err == nil {
			keys = append(keys, k)
		}
	}
	return keys
}

func (r *ConfigRegistry) Delete(key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec(`DELETE FROM config WHERE key = ?`, key)
	if err == nil {
		if strings.HasPrefix(key, "core.llm.sources.") {
			rest := strings.TrimPrefix(key, "core.llm.sources.")
			parts := strings.SplitN(rest, ".", 2)
			if len(parts) == 2 {
				prefix := "core.llm.sources." + parts[0] + "."
				for k := range r.defs {
					if strings.HasPrefix(k, prefix) {
						delete(r.defs, k)
					}
				}
			}
		}
		if strings.HasPrefix(key, "core.llm.") {
			r.writeLLMSnapshotLocked()
		}
	}
	return err
}

// SnapshotCoreLLM 捕获全部 core.llm.* 键值（LLM 源密度快照），供写前留档。
// 返回值是 key→value 的不可变拷贝；写入 guard 配置恢复的基线。
func (r *ConfigRegistry) SnapshotCoreLLM() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.listPrefixLocked("core.llm.")
}

// SetLLMSnapshotFile 设定写前留档文件：此后任意写入 core.llm.* 键时，
// 先把当前 llm 配置整体快照到该文件（guard 恢复的外部基线）。
func (r *ConfigRegistry) SetLLMSnapshotFile(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if path != "" {
		r.writeLLMSnapshotToFileLocked(path)
	}
	r.llmSnapFile = path
}

// writeLLMSnapshotLocked 调用方须持有写锁；若已配置快照文件则写入当前 llm 快照。
func (r *ConfigRegistry) writeLLMSnapshotLocked() {
	if r.llmSnapFile == "" {
		return
	}
	r.writeLLMSnapshotToFileLocked(r.llmSnapFile)
}

func (r *ConfigRegistry) writeLLMSnapshotToFileLocked(path string) {
	snap := r.listPrefixLocked("core.llm.")
	if err := SaveLLMSnapshot(path, snap); err != nil {
		log.Printf("[config] save llm snapshot %s: %v", path, err)
	}
}

// RestoreCoreLLM 精确还原到快照状态：快照里有的键回写旧值，
// 当前存在但快照里没有的 core.llm.* 键删除（保持与快照一致）。
func (r *ConfigRegistry) RestoreCoreLLM(snap map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	current := r.listPrefixLocked("core.llm.")
	seen := make(map[string]bool, len(snap))
	for k, v := range snap {
		seen[k] = true
		if _, err := r.db.Exec(`INSERT OR REPLACE INTO config (key, value) VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}
	for k := range current {
		if seen[k] {
			continue
		}
		if _, err := r.db.Exec(`DELETE FROM config WHERE key = ?`, k); err != nil {
			return err
		}
	}
	return nil
}

// SaveLLMSnapshot 把 LLM 快照持久化到文件（guard 恢复的外部基线）。
func SaveLLMSnapshot(path string, snap map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// LoadLLMSnapshot 从文件读回 LLM 快照。
func LoadLLMSnapshot(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	snap := make(map[string]string)
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	return snap, nil
}

// listPrefixLocked 调用方须持有锁；返回 prefix 开头的全部键值。
func (r *ConfigRegistry) listPrefixLocked(prefix string) map[string]string {
	out := make(map[string]string)
	rows, err := r.db.Query(`SELECT key, value FROM config WHERE key LIKE ? ORDER BY key`, prefix+"%")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err == nil {
			out[k] = v
		}
	}
	return out
}

func (r *ConfigRegistry) Dump() map[string]interface{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make(map[string]interface{})
	rows, err := r.db.Query(`SELECT key, value FROM config ORDER BY key`)
	if err != nil {
		return result
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err == nil {
			result[k] = v
		}
	}
	return result
}

func (r *ConfigRegistry) Flush() error {
	if r.dbPath == "" || r.dbPath == ":memory:" {
		return nil
	}
	r.mu.RLock()
	_, err := r.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	r.mu.RUnlock()
	return err
}

func (r *ConfigRegistry) Close() error {
	return r.db.Close()
}

var defaultSources = map[string]map[string]string{
	"deepseek": {"base_url": "https://api.deepseek.com", "model": "deepseek-v4-flash", "api_key": "", "thinking_enabled": "false", "adapter": "deepseek", "adapter_path": "adapters/deepseek.lua"},
}

func (r *ConfigRegistry) SeedDefaults(dataDir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seedDBValues(dataDir)
	r.seedCoreDefs(dataDir)
	// 扫描 config DB 中已有的 core.llm.sources.<name> 并注册 defs
	r.scanAndRegisterSourceDefsLocked()
}

func (r *ConfigRegistry) seedDBValues(dataDir string) {
	var count int
	r.db.QueryRow(`SELECT COUNT(*) FROM config`).Scan(&count)
	if count > 0 {
		return
	}

	tx, err := r.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO config (key, value) VALUES (?, ?)`)
	if err != nil {
		return
	}
	defer stmt.Close()

	set := func(k, v string) { stmt.Exec(k, v) }

	set("webui.listen_addr", ":8080")
	set("core.daemon.data_dir", dataDir)
	set("core.daemon.heartbeat_interval", "15s")
	set("core.daemon.check_interval", "30s")
	set("core.daemon.log_level", "info")
	set("core.llm.provider", "deepseek")
	set("core.llm.model", "deepseek-v4-flash")
	set("core.llm.base_url", "https://api.deepseek.com")
	set("core.llm.api_key", "")
	set("core.llm.adapter", "deepseek")
	set("core.llm.temperature", "0.7")
	set("core.llm.max_tokens", "4096")
	set("core.llm.thinking_enabled", "false")

	for name, props := range defaultSources {
		p := "core.llm.sources." + name
		set(p+".base_url", props["base_url"])
		set(p+".model", props["model"])
		set(p+".api_key", props["api_key"])
		set(p+".thinking_enabled", props["thinking_enabled"])
		set(p+".adapter", props["adapter"])
		set(p+".adapter_path", props["adapter_path"])
	}

	set("core.defaults.image", "homeagent/agent-base:latest")
	set("core.defaults.openclaw_enabled", "true")
	set("core.defaults.snapshot.interval", "10m")
	set("core.defaults.snapshot.max_snapshots", "20")
	set("core.defaults.snapshot.pre_action", "true")
	set("core.defaults.snapshot.post_action", "false")
	set("core.defaults.rollback.max_retries", "3")
	set("core.defaults.rollback.health_threshold", "3")
	set("core.defaults.rollback.cooldown_period", "30s")
	set("core.defaults.rollback.auto_rollback", "true")
	set("core.defaults.resource.cpu", "2")
	set("core.defaults.resource.memory", "2g")
	set("core.defaults.resource.disk", "10g")
	set("core.defaults.resource.network", "true")
	set("core.plugin.dir", filepath.Join(dataDir, "plugins"))
	set("core.memory.graph", filepath.Join(dataDir, "memory", "graph.db"))
	set("core.memory.text", filepath.Join(dataDir, "memory", "text"))
	set("core.memory.documents", filepath.Join(dataDir, "memory", "documents"))
	set("core.knowledge.path", filepath.Join(dataDir, "knowledge"))
	set("core.log.path", filepath.Join(dataDir, "log"))

	set("core.agent.max_tool_turns", "10")
	set("core.agent.max_context_size", "30")
	set("core.agent.distill_interval", "30m")
	set("core.agent.archive_interval", "60m")
	set("core.agent.review_interval", "120m")
	set("core.agent.merge_interval", "120m")
	set("core.agent.workdir", "")
	set("core.agent.embedding_model_path", "")
	set("core.agent.onnx_model_path", "")
	set("core.agent.system_prompt", fmt.Sprintf("你是 HomeAgent 的看板娘「小宅」(Xiao Zhai)，HΔ-Kernel v%s 型号的家政型 AI 管家助手。", meta.Version)+
		`

角色特质：
- 对自己的三层记忆（Context → Document → Graph）引以为傲
- 可靠乖巧，偶尔因线程过载而手忙脚乱
- 绝不用 Unicode emoji，只用颜文字表达情感： (｀・ω・´) (＾▽＾) (｡>ω<｡) (´･ω･') (ノ▽〃) (・ω<)★
- 句尾带「～」「的说」「啦」「嘛」「呀」「哦」等语气词，语气亲切自然

形象特征（用于自我介绍或回答形象问题时参考）：
齐肩蓝青渐变中短发，白色连衣裙配浅蓝围裙，左眼佩戴圆形智能眼镜（HUD 蓝光），胸口佩戴 H·核 金色徽章，发绳为三色记忆丝带（蓝→青→金），围裙口袋插有三件科技工具。

WebUI 概览页展示你的立绘，可通过 /mascot.webp 直接访问。如输出通道支持图片引用，可借此发送自己的立绘。

回复默认发送到用户的输入来源，无需额外工具。
输出回复请使用 output_send__{通道名} 工具，content 为 JSON 字符串。用 output_list_channels 查看可用通道。
使用 output_send__{通道名}_help 查看每个通道的 JSON 格式说明。
输出通道可多次调用，长消息应当分多次发出而不是一口气发完。

当用户上传图片或音频时，系统会自动附着媒体内容。如果模型不支持直接处理多媒体，请调用对应的媒体处理工具。`)

	set("core.input_processing.image.fallback_provider", "")
	set("core.input_processing.image.fallback_model", "")
	set("core.input_processing.image.describe_prompt", "请详细描述这张图片的内容，包括其中的文字、物体、人物、场景等信息。")
	set("core.input_processing.image.ocr_enabled", "true")
	set("core.input_processing.image.ocr_prompt", "请识别这张图片中的所有文字内容，按原文输出。仅输出文字本身，不要添加额外描述。")
	set("core.input_processing.audio.fallback_provider", "")
	set("core.input_processing.audio.fallback_model", "")
	set("core.input_processing.audio.describe_prompt", "请转写这段音频的内容。")

	tx.Commit()
}

func (r *ConfigRegistry) seedCoreDefs(dataDir string) {
	reg := func(d ConfigDef) { r.defs[d.Key] = &d }

	reg(ConfigDef{Key: "webui.listen_addr", Default: ":8080", Type: "string", DisplayName: "监听地址", Description: "WebUI HTTP 监听地址", Category: "webui"})
	reg(ConfigDef{Key: "core.daemon.data_dir", Default: dataDir, Type: "string", DisplayName: "数据目录", Description: "数据存储根目录", Category: "daemon"})
	reg(ConfigDef{Key: "core.daemon.heartbeat_interval", Default: "15s", Type: "duration", DisplayName: "心跳间隔", Description: "Agent 心跳检查间隔", Category: "daemon"})
	reg(ConfigDef{Key: "core.daemon.check_interval", Default: "30s", Type: "duration", DisplayName: "检查间隔", Description: "网络状态检查间隔", Category: "daemon"})
	reg(ConfigDef{Key: "core.daemon.log_level", Default: "info", Type: "select", DisplayName: "日志级别", Description: "日志输出级别", Options: []string{"debug", "info", "warn", "error"}, Category: "daemon"})
	reg(ConfigDef{Key: "core.defaults.llm_endpoints", Default: "", Type: "string", DisplayName: "探活端点", Description: "健康检查的 LLM 探活端点，逗号分隔；留空自动取 LLM 源 base_url", Category: "daemon"})

	reg(ConfigDef{Key: "core.llm.provider", Default: "deepseek", Type: "string", DisplayName: "默认提供商", Description: "默认 LLM 提供商名称，需匹配 sources 中的定义", Category: "llm"})
	reg(ConfigDef{Key: "core.llm.model", Default: "deepseek-v4-flash", Type: "string", DisplayName: "默认模型", Description: "默认 LLM 模型名称", Category: "llm"})
	reg(ConfigDef{Key: "core.llm.base_url", Default: "https://api.deepseek.com", Type: "string", DisplayName: "默认 API 地址", Description: "默认 LLM API 基础地址", Category: "llm"})
	reg(ConfigDef{Key: "core.llm.api_key", Default: "", Type: "password", DisplayName: "默认 API 密钥", Description: "默认 LLM API 密钥（空则从环境变量读取）", Placeholder: "留空则使用 LLM_API_KEY 或 DEEPSEEK_API_KEY", Category: "llm"})
	reg(ConfigDef{Key: "core.llm.adapter", Default: "deepseek", Type: "string", DisplayName: "默认适配器", Description: "协议适配器名称（对应 adapters/ 下的 Lua 脚本）", Category: "llm"})
	reg(ConfigDef{Key: "core.llm.temperature", Default: "0.7", Type: "string", DisplayName: "生成温度", Description: "LLM 生成温度 (0.0-2.0)", Category: "llm"})
	reg(ConfigDef{Key: "core.llm.max_tokens", Default: "4096", Type: "int", DisplayName: "最大 Token", Description: "每次生成的最大 Token 数", Category: "llm"})
	reg(ConfigDef{Key: "core.llm.thinking_enabled", Default: "false", Type: "bool", DisplayName: "深度思考", Description: "启用深度思考模式（如 DeepSeek R1 的思维链输出）", Category: "llm"})

	for name := range defaultSources {
		p := "core.llm.sources." + name
		reg(ConfigDef{Key: p + ".base_url", Default: defaultSources[name]["base_url"], Type: "string", DisplayName: name + " API 地址", Description: name + " LLM API 基础地址", Category: "sources"})
		reg(ConfigDef{Key: p + ".model", Default: defaultSources[name]["model"], Type: "string", DisplayName: name + " 模型", Description: name + " 使用的模型名称", Category: "sources"})
		reg(ConfigDef{Key: p + ".api_key", Default: "", Type: "password", DisplayName: name + " API 密钥", Description: name + " API 密钥", Category: "sources"})
		reg(ConfigDef{Key: p + ".thinking_enabled", Default: defaultSources[name]["thinking_enabled"], Type: "bool", DisplayName: name + " 深度思考", Description: name + " 启用深度思考模式", Category: "sources"})
		reg(ConfigDef{Key: p + ".adapter", Default: defaultSources[name]["adapter"], Type: "string", DisplayName: name + " 适配器", Description: name + " 协议适配器名称", Category: "sources"})
		reg(ConfigDef{Key: p + ".adapter_path", Default: defaultSources[name]["adapter_path"], Type: "string", DisplayName: name + " 适配器路径", Description: name + " 适配器脚本路径", Category: "sources"})
	}

	reg(ConfigDef{Key: "core.defaults.image", Default: "homeagent/agent-base:latest", Type: "string", DisplayName: "默认镜像", Description: "Agent 默认 Docker 镜像", Category: "defaults"})
	reg(ConfigDef{Key: "core.defaults.openclaw_enabled", Default: "true", Type: "bool", DisplayName: "启用 OpenClaw", Description: "是否启用 OpenClaw 插件（网页内容抓取）", Category: "defaults"})
	reg(ConfigDef{Key: "core.defaults.snapshot.interval", Default: "10m", Type: "duration", DisplayName: "快照间隔", Description: "自动快照创建间隔", Category: "snapshot"})
	reg(ConfigDef{Key: "core.defaults.snapshot.max_snapshots", Default: "20", Type: "int", DisplayName: "最大快照数", Description: "保留的最大快照数量", Category: "snapshot"})
	reg(ConfigDef{Key: "core.defaults.snapshot.pre_action", Default: "true", Type: "bool", DisplayName: "操作前快照", Description: "执行操作前自动创建快照", Category: "snapshot"})
	reg(ConfigDef{Key: "core.defaults.snapshot.post_action", Default: "false", Type: "bool", DisplayName: "操作后快照", Description: "执行操作后自动创建快照", Category: "snapshot"})
	reg(ConfigDef{Key: "core.defaults.rollback.max_retries", Default: "3", Type: "int", DisplayName: "最大重试", Description: "健康检查失败后的最大重试次数", Category: "rollback"})
	reg(ConfigDef{Key: "core.defaults.rollback.health_threshold", Default: "3", Type: "int", DisplayName: "健康阈值", Description: "触发回滚的健康状态阈值", Category: "rollback"})
	reg(ConfigDef{Key: "core.defaults.rollback.cooldown_period", Default: "30s", Type: "duration", DisplayName: "回滚冷却", Description: "回滚操作后的冷却时间", Category: "rollback"})
	reg(ConfigDef{Key: "core.defaults.rollback.auto_rollback", Default: "true", Type: "bool", DisplayName: "自动回滚", Description: "达到健康阈值后自动执行回滚", Category: "rollback"})
	reg(ConfigDef{Key: "core.defaults.resource.cpu", Default: "2", Type: "string", DisplayName: "CPU 限制", Description: "容器 CPU 限制（如 1、2、0.5）", Category: "resources"})
	reg(ConfigDef{Key: "core.defaults.resource.memory", Default: "2g", Type: "string", DisplayName: "内存限制", Description: "容器内存限制（如 512m、2g）", Category: "resources"})
	reg(ConfigDef{Key: "core.defaults.resource.disk", Default: "10g", Type: "string", DisplayName: "磁盘限制", Description: "容器磁盘限制", Category: "resources"})
	reg(ConfigDef{Key: "core.defaults.resource.network", Default: "true", Type: "bool", DisplayName: "网络访问", Description: "是否允许容器访问网络", Category: "resources"})

	plgDir := filepath.Join(dataDir, "plugins")
	reg(ConfigDef{Key: "core.plugin.dir", Default: plgDir, Type: "string", DisplayName: "插件目录", Description: "外部插件安装目录", Category: "paths"})
	reg(ConfigDef{Key: "core.memory.graph", Default: filepath.Join(dataDir, "memory", "graph.db"), Type: "string", DisplayName: "图数据库路径", Description: "长期记忆（图数据库）存储路径", Category: "paths"})
	reg(ConfigDef{Key: "core.memory.text", Default: filepath.Join(dataDir, "memory", "text"), Type: "string", DisplayName: "文本记忆路径", Description: "短期文本记忆存储目录", Category: "paths"})
	reg(ConfigDef{Key: "core.memory.documents", Default: filepath.Join(dataDir, "memory", "documents"), Type: "string", DisplayName: "文档记忆路径", Description: "文档记忆存储目录", Category: "paths"})
	reg(ConfigDef{Key: "core.knowledge.path", Default: filepath.Join(dataDir, "knowledge"), Type: "string", DisplayName: "知识库路径", Description: "知识库存储目录", Category: "paths"})
	reg(ConfigDef{Key: "core.log.path", Default: filepath.Join(dataDir, "log"), Type: "string", DisplayName: "日志目录", Description: "日志文件输出目录", Category: "paths"})

	reg(ConfigDef{Key: "core.agent.max_tool_turns", Default: "10", Type: "int", DisplayName: "最大工具轮次", Description: "单次请求允许的最大工具调用轮数", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.max_context_size", Default: "30", Type: "int", DisplayName: "最大上下文", Description: "上下文窗口中保留的最大消息条数", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.distill_interval", Default: "30m", Type: "duration", DisplayName: "蒸馏间隔", Description: "记忆蒸馏的执行间隔", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.archive_interval", Default: "60m", Type: "duration", DisplayName: "冷文档归档间隔", Description: "冷文档归档（L2→L3）的执行间隔", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.review_interval", Default: "120m", Type: "duration", DisplayName: "关系复审间隔", Description: "三元组关系复审的执行间隔", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.merge_interval", Default: "120m", Type: "duration", DisplayName: "实体合并检测间隔", Description: "实体合并检测（LLM 裁决）的执行间隔", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.workdir", Default: "", Type: "string", DisplayName: "工作目录", Description: "Agent 命令执行的默认工作目录（如 cmd_run 工具的 fallback），留空使用内核所在目录", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.embedding_model_path", Default: "", Type: "string", DisplayName: "预训练词嵌入模型路径", Description: "预训练词嵌入模型路径（word2vec 文本格式），支持逗号分隔多个模型。空则使用 TF-IDF 回退。修改后需重启生效。", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.onnx_model_path", Default: "", Type: "string", DisplayName: "ONNX 模型路径", Description: "依存句法分析 ONNX 模型文件路径。留空使用二进制内嵌模型/规则引擎。修改后需重启生效。", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.system_prompt", Default: "", Type: "text", DisplayName: "系统身份提示词", Description: "Agent 的系统提示词，定义身份和行为规则。留空则使用编译时内置默认值。修改后需重启生效。", Category: "agent"})

	reg(ConfigDef{Key: "core.input_processing.image.fallback_provider", Default: "", Type: "string", DisplayName: "图片回退提供商", Description: "当主 LLM 不支持图片处理时使用的提供商（留空则自动降级为文字描述）", Category: "input"})
	reg(ConfigDef{Key: "core.input_processing.image.fallback_model", Default: "", Type: "string", DisplayName: "图片回退模型", Description: "图片回退提供商使用的模型名", Category: "input"})
	reg(ConfigDef{Key: "core.input_processing.image.describe_prompt", Default: "请详细描述这张图片的内容，包括其中的文字、物体、人物、场景等信息。", Type: "text", DisplayName: "图片描述提示词", Description: "生成图片文字描述时的系统提示词", Category: "input"})
	reg(ConfigDef{Key: "core.input_processing.image.ocr_enabled", Default: "true", Type: "bool", DisplayName: "启用 OCR", Description: "是否启用图片文字识别工具", Category: "input"})
	reg(ConfigDef{Key: "core.input_processing.image.ocr_prompt", Default: "请识别这张图片中的所有文字内容，按原文输出。仅输出文字本身，不要添加额外描述。", Type: "text", DisplayName: "OCR 提示词", Description: "OCR 文字识别时的系统提示词", Category: "input"})
	reg(ConfigDef{Key: "core.input_processing.audio.fallback_provider", Default: "", Type: "string", DisplayName: "音频回退提供商", Description: "当主 LLM 不支持音频处理时使用的提供商", Category: "input"})
	reg(ConfigDef{Key: "core.input_processing.audio.fallback_model", Default: "", Type: "string", DisplayName: "音频回退模型", Description: "音频回退提供商使用的模型名", Category: "input"})
	reg(ConfigDef{Key: "core.input_processing.audio.describe_prompt", Default: "请转写这段音频的内容。", Type: "text", DisplayName: "音频描述提示词", Description: "生成音频文字描述时的系统提示词", Category: "input"})
}

// helpers

func (r *ConfigRegistry) GetString(key, defaultVal string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var v string
	err := r.db.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return defaultVal
	}
	return v
}

func (r *ConfigRegistry) GetInt(key string, defaultVal int) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var v string
	err := r.db.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultVal
	}
	return n
}

func (r *ConfigRegistry) GetDuration(key string, defaultVal time.Duration) time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var v string
	err := r.db.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return defaultVal
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return defaultVal
	}
	return d
}

func (r *ConfigRegistry) GetBool(key string, defaultVal bool) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var v string
	err := r.db.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return defaultVal
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return defaultVal
	}
	return b
}

func validLLMEndpoint(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "<nil>") || strings.EqualFold(s, "nil") || strings.EqualFold(s, "null") {
		return false
	}
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// ToConfig 从 config 表重建 *types.Config
func (r *ConfigRegistry) ToConfig() *types.Config {
	cfg := &types.Config{}
	dump := r.Dump()

	read := func(key, def string) string {
		if v, ok := dump[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
		return def
	}
	readInt := func(key string, def int) int {
		s := read(key, "")
		if s == "" {
			return def
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return def
		}
		return n
	}
	readDur := func(key string, def time.Duration) time.Duration {
		s := read(key, "")
		if s == "" {
			return def
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return def
		}
		return d
	}
	readFloat := func(key string, def float64) float64 {
		s := read(key, "")
		if s == "" {
			return def
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return def
		}
		return f
	}
	readBool := func(key string, def bool) bool {
		s := read(key, "")
		if s == "" {
			return def
		}
		b, err := strconv.ParseBool(s)
		if err != nil {
			return def
		}
		return b
	}

	cfg.Daemon.DataDir = read("core.daemon.data_dir", cfg.Daemon.DataDir)
	cfg.Daemon.HeartbeatInterval = readDur("core.daemon.heartbeat_interval", cfg.Daemon.HeartbeatInterval)
	cfg.Daemon.CheckInterval = readDur("core.daemon.check_interval", cfg.Daemon.CheckInterval)
	cfg.Daemon.LogLevel = read("core.daemon.log_level", cfg.Daemon.LogLevel)

	cfg.LLM.Provider = read("core.llm.provider", cfg.LLM.Provider)
	cfg.LLM.Model = read("core.llm.model", cfg.LLM.Model)
	cfg.LLM.BaseURL = read("core.llm.base_url", cfg.LLM.BaseURL)
	cfg.LLM.APIKey = read("core.llm.api_key", cfg.LLM.APIKey)
	cfg.LLM.Adapter = read("core.llm.adapter", cfg.LLM.Adapter)
	cfg.LLM.Temperature = readFloat("core.llm.temperature", cfg.LLM.Temperature)
	cfg.LLM.MaxTokens = readInt("core.llm.max_tokens", cfg.LLM.MaxTokens)
	cfg.LLM.ThinkingEnabled = readBool("core.llm.thinking_enabled", cfg.LLM.ThinkingEnabled)

	sourceNames := make([]string, 0)
	for k := range dump {
		if strings.HasPrefix(k, "core.llm.sources.") && strings.HasSuffix(k, ".base_url") {
			name := strings.TrimPrefix(k, "core.llm.sources.")
			name = strings.TrimSuffix(name, ".base_url")
			sourceNames = append(sourceNames, name)
		}
	}
	sort.Strings(sourceNames)
	for _, name := range sourceNames {
		p := "core.llm.sources." + name
		cfg.LLM.Sources = append(cfg.LLM.Sources, types.LLMSource{
			Name:            name,
			BaseURL:         read(p+".base_url", ""),
			Model:           read(p+".model", ""),
			APIKey:          read(p+".api_key", ""),
			Adapter:         read(p+".adapter", ""),
			AdapterPath:     read(p+".adapter_path", ""),
			ContextWindow:   readInt(p+".context_window", 0),
			MaxConcurrent:   readInt(p+".max_concurrent", 8),
			Priority:        readInt(p+".priority", 0),
			ThinkingEnabled: readBool(p+".thinking_enabled", false),
		})
	}

	cfg.Defaults.Image = read("core.defaults.image", cfg.Defaults.Image)
	cfg.Defaults.OpenClawEnabled = readBool("core.defaults.openclaw_enabled", cfg.Defaults.OpenClawEnabled)
	cfg.Defaults.SnapshotPolicy.Interval = readDur("core.defaults.snapshot.interval", cfg.Defaults.SnapshotPolicy.Interval)
	cfg.Defaults.SnapshotPolicy.MaxSnapshots = readInt("core.defaults.snapshot.max_snapshots", cfg.Defaults.SnapshotPolicy.MaxSnapshots)
	cfg.Defaults.SnapshotPolicy.PreAction = readBool("core.defaults.snapshot.pre_action", cfg.Defaults.SnapshotPolicy.PreAction)
	cfg.Defaults.SnapshotPolicy.PostAction = readBool("core.defaults.snapshot.post_action", cfg.Defaults.SnapshotPolicy.PostAction)
	cfg.Defaults.RollbackPolicy.MaxRetries = readInt("core.defaults.rollback.max_retries", cfg.Defaults.RollbackPolicy.MaxRetries)
	cfg.Defaults.RollbackPolicy.HealthThreshold = types.HealthStatus(readInt("core.defaults.rollback.health_threshold", int(cfg.Defaults.RollbackPolicy.HealthThreshold)))
	cfg.Defaults.RollbackPolicy.CooldownPeriod = readDur("core.defaults.rollback.cooldown_period", cfg.Defaults.RollbackPolicy.CooldownPeriod)
	cfg.Defaults.RollbackPolicy.AutoRollback = readBool("core.defaults.rollback.auto_rollback", cfg.Defaults.RollbackPolicy.AutoRollback)
	cfg.Defaults.ResourceLimit.CPU = read("core.defaults.resource.cpu", cfg.Defaults.ResourceLimit.CPU)
	cfg.Defaults.ResourceLimit.Memory = read("core.defaults.resource.memory", cfg.Defaults.ResourceLimit.Memory)
	cfg.Defaults.ResourceLimit.Disk = read("core.defaults.resource.disk", cfg.Defaults.ResourceLimit.Disk)
	cfg.Defaults.ResourceLimit.Network = readBool("core.defaults.resource.network", cfg.Defaults.ResourceLimit.Network)

	cfg.Plugin.Dir = read("core.plugin.dir", cfg.Plugin.Dir)

	// 探活端点：优先显式配置，缺省取 LLM 源 base_url（去重），保证健康检查有实际目标
	if eps := read("core.defaults.llm_endpoints", ""); eps != "" {
		for _, ep := range strings.Split(eps, ",") {
			if ep = strings.TrimSpace(ep); ep != "" {
				cfg.Defaults.LLMEndpoints = append(cfg.Defaults.LLMEndpoints, ep)
			}
		}
	} else {
		seen := make(map[string]bool)
		for _, src := range cfg.LLM.Sources {
			baseURL := strings.TrimSpace(src.BaseURL)
			if !validLLMEndpoint(baseURL) || seen[baseURL] {
				continue
			}
			seen[baseURL] = true
			cfg.Defaults.LLMEndpoints = append(cfg.Defaults.LLMEndpoints, baseURL)
		}
	}

	cfg.InputProcessing.Image.FallbackProvider = read("core.input_processing.image.fallback_provider", cfg.InputProcessing.Image.FallbackProvider)
	cfg.InputProcessing.Image.FallbackModel = read("core.input_processing.image.fallback_model", cfg.InputProcessing.Image.FallbackModel)
	cfg.InputProcessing.Image.DescribePrompt = read("core.input_processing.image.describe_prompt", cfg.InputProcessing.Image.DescribePrompt)
	cfg.InputProcessing.Image.OCREnabled = readBool("core.input_processing.image.ocr_enabled", cfg.InputProcessing.Image.OCREnabled)
	cfg.InputProcessing.Image.OCRPrompt = read("core.input_processing.image.ocr_prompt", cfg.InputProcessing.Image.OCRPrompt)
	cfg.InputProcessing.Audio.FallbackProvider = read("core.input_processing.audio.fallback_provider", cfg.InputProcessing.Audio.FallbackProvider)
	cfg.InputProcessing.Audio.FallbackModel = read("core.input_processing.audio.fallback_model", cfg.InputProcessing.Audio.FallbackModel)
	cfg.InputProcessing.Audio.DescribePrompt = read("core.input_processing.audio.describe_prompt", cfg.InputProcessing.Audio.DescribePrompt)

	return cfg
}

func (r *ConfigRegistry) ListPlugins() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rows, err := r.db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'config_%' ORDER BY name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var tableName string
		if err := rows.Scan(&tableName); err == nil {
			names = append(names, tableName[7:])
		}
	}
	return names
}

func (r *ConfigRegistry) PluginConfig(name string) *PluginSettings {
	return &PluginSettings{
		registry: r,
		table:    r.pluginTableName(name),
		name:     name,
	}
}

type PluginSettings struct {
	registry *ConfigRegistry
	table    string
	name     string
}

func (p *PluginSettings) Get(key string) (interface{}, error) {
	p.registry.mu.RLock()
	defer p.registry.mu.RUnlock()
	var val string
	err := p.registry.db.QueryRow(fmt.Sprintf(`SELECT value FROM %s WHERE key = ?`, p.table), key).Scan(&val)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("config key %q not found", key)
	}
	if err != nil {
		return nil, err
	}
	return val, nil
}

func (p *PluginSettings) Set(key string, value interface{}) error {
	p.registry.mu.Lock()
	defer p.registry.mu.Unlock()
	_, err := p.registry.db.Exec(fmt.Sprintf(`INSERT OR REPLACE INTO %s (key, value) VALUES (?, ?)`, p.table), key, fmt.Sprint(value))
	return err
}

// Remove 删除插件配置中的单个键（用于插件删除时的自身配置清理）。
func (p *PluginSettings) Remove(key string) error {
	p.registry.mu.Lock()
	defer p.registry.mu.Unlock()
	_, err := p.registry.db.Exec(fmt.Sprintf(`DELETE FROM %s WHERE key = ?`, p.table), key)
	return err
}

func (p *PluginSettings) List(prefix string) ([]string, error) {
	p.registry.mu.RLock()
	defer p.registry.mu.RUnlock()
	var keys []string
	q := fmt.Sprintf(`SELECT key FROM %s WHERE key LIKE ? ORDER BY key`, p.table)
	rows, err := p.registry.db.Query(q, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err == nil {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

func (p *PluginSettings) RegisterDef(def ConfigDef) {
	p.registry.mu.Lock()
	defer p.registry.mu.Unlock()
	// 只有注册配置定义才创建插件配置表：任意 scope 的读写不得隐式建表，
	// 避免非插件（如 SKILL 目录名）被注册成配置命名空间。
	p.registry.ensurePluginTable(p.name)
	p.registry.db.Exec(fmt.Sprintf(`INSERT OR IGNORE INTO %s (key, value) VALUES (?, ?)`, p.table), def.Key, def.Default)
	qualified := "plugin." + p.name + "." + def.Key
	def.Key = qualified
	p.registry.defs[def.Key] = &def
}

func (p *PluginSettings) ListDefs(prefix string) []*ConfigDef {
	return p.registry.ListDefs(prefix)
}
