package config

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
	_ "github.com/mattn/go-sqlite3"
)

type ConfigRegistry struct {
	mu     sync.RWMutex
	db     *sql.DB
	dbPath string
}

func NewConfigRegistry(dbPath string) *ConfigRegistry {
	if dbPath == "" {
		dbPath = ":memory:"
	}
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		panic(fmt.Sprintf("open config db: %v", err))
	}
	// WAL 模式提升并发
	db.Exec("PRAGMA journal_mode=WAL")
	r := &ConfigRegistry{db: db, dbPath: dbPath}
	r.initCoreTable()
	return r
}

func (r *ConfigRegistry) initCoreTable() {
	r.db.Exec(`CREATE TABLE IF NOT EXISTS config (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`)
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
	_, err := r.db.Exec(`INSERT OR REPLACE INTO config (key, value) VALUES (?, ?)`, key, fmt.Sprint(value))
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
	return err
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
	// SQLite 自动持久化；显式 checkpoint 确保一致性
	r.mu.RLock()
	_, err := r.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	r.mu.RUnlock()
	return err
}

func (r *ConfigRegistry) Close() error {
	return r.db.Close()
}

// SeedFrom 从 *types.Config 批量导入默认值到 config 表（仅空表时写入）
func (r *ConfigRegistry) SeedFrom(cfg *types.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 检查是否已有数据
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

	// daemon
	set("core.daemon.listen_addr", cfg.Daemon.ListenAddr)
	set("core.daemon.data_dir", cfg.Daemon.DataDir)
	set("core.daemon.heartbeat_interval", cfg.Daemon.HeartbeatInterval.String())
	set("core.daemon.check_interval", cfg.Daemon.CheckInterval.String())
	set("core.daemon.log_level", cfg.Daemon.LogLevel)

	// llm
	set("core.llm.provider", cfg.LLM.Provider)
	set("core.llm.model", cfg.LLM.Model)
	set("core.llm.base_url", cfg.LLM.BaseURL)
	set("core.llm.adapter", cfg.LLM.Adapter)
	set("core.llm.temperature", strconv.FormatFloat(cfg.LLM.Temperature, 'f', 2, 64))
	set("core.llm.max_tokens", strconv.Itoa(cfg.LLM.MaxTokens))

	// llm sources
	for _, src := range cfg.LLM.Sources {
		p := "core.llm.sources." + src.Name
		set(p+".base_url", src.BaseURL)
		set(p+".model", src.Model)
		set(p+".adapter", src.Adapter)
		set(p+".adapter_path", src.AdapterPath)
	}

	// defaults
	set("core.defaults.image", cfg.Defaults.Image)
	set("core.defaults.openclaw_enabled", strconv.FormatBool(cfg.Defaults.OpenClawEnabled))
	set("core.defaults.snapshot.interval", cfg.Defaults.SnapshotPolicy.Interval.String())
	set("core.defaults.snapshot.max_snapshots", strconv.Itoa(cfg.Defaults.SnapshotPolicy.MaxSnapshots))
	set("core.defaults.snapshot.pre_action", strconv.FormatBool(cfg.Defaults.SnapshotPolicy.PreAction))
	set("core.defaults.snapshot.post_action", strconv.FormatBool(cfg.Defaults.SnapshotPolicy.PostAction))
	set("core.defaults.rollback.max_retries", strconv.Itoa(cfg.Defaults.RollbackPolicy.MaxRetries))
	set("core.defaults.rollback.health_threshold", strconv.Itoa(int(cfg.Defaults.RollbackPolicy.HealthThreshold)))
	set("core.defaults.rollback.cooldown_period", cfg.Defaults.RollbackPolicy.CooldownPeriod.String())
	set("core.defaults.rollback.auto_rollback", strconv.FormatBool(cfg.Defaults.RollbackPolicy.AutoRollback))
	set("core.defaults.resource.cpu", cfg.Defaults.ResourceLimit.CPU)
	set("core.defaults.resource.memory", cfg.Defaults.ResourceLimit.Memory)
	set("core.defaults.resource.disk", cfg.Defaults.ResourceLimit.Disk)
	set("core.defaults.resource.network", strconv.FormatBool(cfg.Defaults.ResourceLimit.Network))
	set("core.agent.max_tool_turns", "10")
	set("core.agent.max_context_size", "30")
	set("core.agent.distill_interval", "30m")

	tx.Commit()
}

// helpers  — 所有值存为 TEXT，解析时自动转换

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

// ToConfig 从 config 表重建 *types.Config（数据库为真实源，YAML 仅作初始 seed）
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

	cfg.Daemon.ListenAddr = read("core.daemon.listen_addr", cfg.Daemon.ListenAddr)
	cfg.Daemon.DataDir = read("core.daemon.data_dir", cfg.Daemon.DataDir)
	cfg.Daemon.HeartbeatInterval = readDur("core.daemon.heartbeat_interval", cfg.Daemon.HeartbeatInterval)
	cfg.Daemon.CheckInterval = readDur("core.daemon.check_interval", cfg.Daemon.CheckInterval)
	cfg.Daemon.LogLevel = read("core.daemon.log_level", cfg.Daemon.LogLevel)

	cfg.LLM.Provider = read("core.llm.provider", cfg.LLM.Provider)
	cfg.LLM.Model = read("core.llm.model", cfg.LLM.Model)
	cfg.LLM.BaseURL = read("core.llm.base_url", cfg.LLM.BaseURL)
	cfg.LLM.Adapter = read("core.llm.adapter", cfg.LLM.Adapter)
	cfg.LLM.Temperature = float64(readInt("core.llm.temperature", int(cfg.LLM.Temperature*100))) / 100
	cfg.LLM.MaxTokens = readInt("core.llm.max_tokens", cfg.LLM.MaxTokens)

	// 重建 sources —— 从 DB 中按前缀扫描，按名称排序保证确定性
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
			Name:        name,
			BaseURL:     read(p+".base_url", ""),
			Model:       read(p+".model", ""),
			Adapter:     read(p+".adapter", ""),
			AdapterPath: read(p+".adapter_path", ""),
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

	return cfg
}
func (r *ConfigRegistry) PluginConfig(name string) *PluginSettings {
	r.ensurePluginTable(name)
	return &PluginSettings{
		registry: r,
		table:    r.pluginTableName(name),
	}
}

// PluginSettings 实现 sdk.SettingsAPI，作用域为单个插件表
type PluginSettings struct {
	registry *ConfigRegistry
	table    string
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
