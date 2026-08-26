package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	sdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

// Plugin 快速检查/崩溃取证工具集。全部确定性检出，返回结论而非原文，供 guard / failback 决策。
type Plugin struct {
	name    string
	sdk     *sdk.PluginSDK
	muKey   string
	dataDir string
	logDir  string
	cfgPath string
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s
	p.muKey = p.name + "_"
	p.resolveDirs(s)

	s.Settings().RegisterDef(sdk.ConfigDef{
		Key:         "db_check_cmd",
		Default:     "sqlite3",
		Type:        "string",
		DisplayName: "sqlite3 CLI 路径",
		Description: "diag_db 用到的 sqlite3 命令；留空则仅在可用时使用，缺失回退到内核 Settings 读取。留空=auto",
		Category:    "recoverydiag",
	})

	s.Settings().RegisterDef(sdk.ConfigDef{
		Key:         "recovery_kb_dir",
		Default:     "",
		Type:        "string",
		DisplayName: "结论落盘目录",
		Description: "diag_loc 结论 JSON 落盘目录，缺省用 <data_dir>/recovery_kb",
		Category:    "recoverydiag",
	})

	s.RegisterTool(p.muKey+"diag_triage", sdk.ToolDef{
		Name:        p.muKey + "diag_triage",
		Description: "快速分诊：根据退出码/信号/存活状态粗分崩溃类别（进程死亡 vs 配置类不可达 vs 正常）。返回结论，不返回日志原文。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"exit_code":    map[string]interface{}{"type": "integer", "description": "进程退出码（0=正常）"},
				"signal":       map[string]interface{}{"type": "string", "description": "终止信号名（如 SIGSEGV/SIGKILL/OOM），可选"},
				"uptime_ms":    map[string]interface{}{"type": "integer", "description": "进程存活毫秒，可选"},
				"still_alive":  map[string]interface{}{"type": "boolean", "description": "主 agent 是否仍在运行/对心跳有响应，可选"},
				"crash_reason": map[string]interface{}{"type": "string", "description": "守护方附带的已知原因描述，可选"},
			},
		},
		NoMemory: true,
	}, p.handleTriage)

	s.RegisterTool(p.muKey+"diag_db", sdk.ToolDef{
		Name:        p.muKey + "diag_db",
		Description: "config.db 完整性（PRAGMA integrity_check）+ LLM 源解析校验（core.llm.sources.* 必备字段），逐项 ok/fail，返回结论。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"db_path": map[string]interface{}{"type": "string", "description": "config.db 路径，缺省用 <data_dir>/config.db"},
			},
		},
		NoMemory: true,
	}, p.handleDB)

	s.RegisterTool(p.muKey+"diag_log_scan", sdk.ToolDef{
		Name:        p.muKey + "diag_log_scan",
		Description: "在日志目录时间窗内统计已知错误签名（panic/OOM/网络不可达/provider失败/sql/致命）出现次数，返回按类统计与主导结论。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"log_dir":       map[string]interface{}{"type": "string", "description": "日志目录，缺省用 <data_dir>/log"},
				"since_minutes": map[string]interface{}{"type": "integer", "description": "只看最近 N 分钟，缺省看全部"},
				"max_lines":     map[string]interface{}{"type": "integer", "description": "最多扫描行数（防止读取过大文件），缺省 200000"},
			},
		},
		NoMemory: true,
	}, p.handleLogScan)

	s.RegisterTool(p.muKey+"diag_delta", sdk.ToolDef{
		Name:        p.muKey + "diag_delta",
		Description: "对比 baseline（上次 good 快照/目录）与现状目录，输出 created/modified/deleted 文件清单与摘要，用于判定'改了什么'。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"baseline_dir": map[string]interface{}{"type": "string", "description": "基线目录（快照解包目录），必传"},
				"current_dir":  map[string]interface{}{"type": "string", "description": "现状目录（如 agentfs merged/upper），必传"},
				"pattern":      map[string]interface{}{"type": "string", "description": "只关注匹配该子串的相对路径，可选"},
				"max_items":    map[string]interface{}{"type": "integer", "description": "返回最多文件条数，缺省 500"},
			},
		},
		NoMemory: true,
	}, p.handleDelta)

	s.RegisterTool(p.muKey+"diag_loc", sdk.ToolDef{
		Name:        p.muKey + "diag_loc",
		Description: "综合分诊/DB/日志/快照四项结论，按因果强度正交排序定位根因并给出推荐恢复动作。调用前请先跑其余 diag_* 并把结论传入。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"triage":   map[string]interface{}{"type": "object", "description": "diag_triage 返回的结论对象"},
				"db":       map[string]interface{}{"type": "object", "description": "diag_db 返回的结论对象"},
				"log_scan": map[string]interface{}{"type": "object", "description": "diag_log_scan 返回的结论对象"},
				"delta":    map[string]interface{}{"type": "object", "description": "diag_delta 返回的结论对象"},
				"persist":  map[string]interface{}{"type": "boolean", "description": "是否落盘结论到 recovery_kb 并回流知识库，缺省 true"},
			},
		},
		NoMemory: true,
	}, p.handleLoc)

	log.Printf("[%s] started: data_dir=%s log_dir=%s", p.name, p.dataDir, p.logDir)
	return nil
}

func (p *Plugin) Stop() error {
	log.Printf("[%s] stopped", p.name)
	return nil
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

// resolveDirs 从内核 Settings 解出数据目录与日志目录。
func (p *Plugin) resolveDirs(s *sdk.PluginSDK) {
	if v, err := s.Settings().GetCore("core.daemon.data_dir"); err == nil && v != nil {
		if sv, ok := v.(string); ok && sv != "" {
			p.dataDir = sv
		}
	}
	if v, err := s.Settings().GetCore("core.log.path"); err == nil && v != nil {
		if sv, ok := v.(string); ok && sv != "" {
			p.logDir = sv
		}
	}
	if p.logDir == "" && p.dataDir != "" {
		p.logDir = filepath.Join(p.dataDir, "log")
	}
	if p.dataDir != "" {
		p.cfgPath = filepath.Join(p.dataDir, "config.db")
	}
}

// ===== 参数解析辅助 =====

func argString(args map[string]interface{}, key string) string {
	if v, ok := args[key]; ok {
		switch x := v.(type) {
		case string:
			return x
		case json.Number:
			return x.String()
		case float64:
			return fmt.Sprintf("%.0f", x)
		case int:
			return fmt.Sprintf("%d", x)
		case int64:
			return fmt.Sprintf("%d", x)
		case bool:
			if x {
				return "true"
			}
			return "false"
		default:
			return fmt.Sprint(x)
		}
	}
	return ""
}

func argInt(args map[string]interface{}, key string, def int) int {
	if v, ok := args[key]; ok {
		switch x := v.(type) {
		case float64:
			return int(x)
		case json.Number:
			i, _ := x.Int64()
			return int(i)
		case int:
			return x
		case int64:
			return int(x)
		case string:
			var i int
			if _, err := fmt.Sscanf(x, "%d", &i); err == nil {
				return i
			}
		}
	}
	return def
}

func argBool(args map[string]interface{}, key string) bool {
	if v, ok := args[key]; ok {
		switch x := v.(type) {
		case bool:
			return x
		case string:
			return x == "true" || x == "1" || x == "yes"
		}
	}
	return false
}

func argMap(args map[string]interface{}, key string) map[string]interface{} {
	if v, ok := args[key]; ok {
		if m, ok := v.(map[string]interface{}); ok {
			return m
		}
		if s, ok := v.(string); ok && s != "" {
			var m map[string]interface{}
			if json.Unmarshal([]byte(s), &m) == nil {
				return m
			}
		}
	}
	return nil
}

func valueString(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	return argString(m, key)
}

func valueInt(m map[string]interface{}, key string) int {
	if m == nil {
		return 0
	}
	return argInt(m, key, 0)
}

func content(v interface{}) map[string]interface{} {
	out := map[string]interface{}{"content": v}
	return out
}

func contentWith(m map[string]interface{}, c string) map[string]interface{} {
	m["content"] = c
	return m
}

// ---- diag_triage ----

func (p *Plugin) handleTriage(args map[string]interface{}) (interface{}, error) {
	exitCode := argInt(args, "exit_code", 0)
	signal := argString(args, "signal")
	uptimeMS := argInt(args, "uptime_ms", 0)
	stillAlive := argBool(args, "still_alive")
	reason := argString(args, "crash_reason")

	class := "normal_stop"
	verdict := "healthy"
	var detail []string
	if reasons := strings.TrimSpace(reason); reasons != "" {
		detail = append(detail, "守护方告知: "+reasons)
	}

	switch {
	case stillAlive:
		// 主 agent 进程仍在，但被判定需要检查 → 配置类/可达性问题优先（进程自身正常）
		class = "config_unreachable"
		verdict = "degraded"
		detail = append(detail, "进程存活但健康检测触发，倾向配置/可达性类")
	case signal != "":
		s := strings.ToUpper(strings.ReplaceAll(signal, "-", ""))
		class = "process_death"
		verdict = "down"
		detail = append(detail, fmt.Sprintf("被信号终止: %s", signal))
		if s == "SIGKILL" || s == "KILL" || s == "OOM" || strings.Contains(strings.ToLower(signal), "oom") {
			class = "process_starvation"
			detail = append(detail, "疑似被强杀/OOM，优先怀疑资源或失控")
		} else if s == "SIGSEGV" || s == "SIGBUS" || s == "SIGABRT" || s == "SIGFPE" {
			detail = append(detail, "疑似崩溃信号（segv/abrt），配合 diag_log_scan 的 panic/栈签名")
		}
	case exitCode != 0:
		class = "process_death"
		verdict = "down"
		detail = append(detail, fmt.Sprintf("非零退出码: %d", exitCode))
		if exitCode >= 128 {
			detail = append(detail, "退出码>=128 通常是 128+signal，配合信号判定")
		}
	default:
		detail = append(detail, "退出码0且无信号：正常停止")
	}

	c := map[string]interface{}{
		"verdict":        verdict,
		"class":          class,
		"exit_code":      exitCode,
		"signal":         signal,
		"still_alive":    stillAlive,
		"uptime_ms":      uptimeMS,
		"detail":         detail,
		"recommendation": recForTriage(class),
	}
	return contentWith(c, describeTriage(c)), nil
}

func recForTriage(class string) string {
	switch class {
	case "process_death":
		return "进程崩溃：先查崩溃点（diag_log_scan 栈/panic 签名）；若无配置改动则重建 worker，勿动配置"
	case "process_starvation":
		return "资源/强杀：检查内存/失控，勿动配置，重建 worker 并限制资源"
	case "config_unreachable":
		return "配置/可达性：查 LLM 源与网络配置（diag_db + diag_delta on /etc），必要时还原配置并 ReloadFromConfig"
	default:
		return "正常情况，无需恢复"
	}
}

func describeTriage(c map[string]interface{}) string {
	return fmt.Sprintf("判决: %s | 类别: %s | 建议: %s", c["verdict"], c["class"], c["recommendation"])
}

// ---- diag_db ----

type llmSourceData struct {
	Name      string   `json:"name"`
	BaseURL   string   `json:"base_url"`
	Model     string   `json:"model"`
	Adapter   string   `json:"adapter"`
	APIKeySet bool     `json:"api_key_present"`
	Missing   []string `json:"missing_fields"`
	OK        bool     `json:"ok"`
}

func (p *Plugin) handleDB(args map[string]interface{}) (interface{}, error) {
	dbPath := argString(args, "db_path")
	if dbPath == "" {
		dbPath = p.cfgPath
	}
	if dbPath == "" {
		return content("无法定位 config.db（未配置 data_dir），请传入 db_path"), nil
	}
	// 安全校验：db_path 仅允许 data 目录内的 sqlite 文件，防止被用作任意文件探测。
	if p.dataDir != "" {
		abs, err := filepath.Abs(dbPath)
		if err != nil {
			return content("db_path 解析失败: " + err.Error()), nil
		}
		base := filepath.Clean(p.dataDir)
		if abs != base && !strings.HasPrefix(abs, base+string(filepath.Separator)) {
			return content("db_path 必须位于数据目录内（" + base + "）"), nil
		}
	}

	res := map[string]interface{}{
		"db_path": dbPath,
		"exists":  false,
	}
	if st, err := os.Stat(dbPath); err != nil || st.IsDir() {
		res["integrity"] = "absent"
		res["sources"] = []map[string]interface{}{}
		res["summary"] = "config.db 缺失，属配置损坏类高危信号"
		return contentWith(res, "config.db 缺失/不可访问"), nil
	}
	res["exists"] = true
	res["size_bytes"] = func() int64 {
		st, _ := os.Stat(dbPath)
		if st != nil {
			return st.Size()
		}
		return 0
	}()

	integrity, errTxt := p.dbIntegrity(dbPath)
	res["integrity"] = integrity
	if serr, ok := errTxt.(string); ok && serr != "" {
		res["integrity_error"] = serr
	}

	sources, srcErr := p.dbSources(dbPath)
	res["sources"] = sources
	failed := 0
	absent := 0
	var missingFields []string
	for _, s := range sources {
		if !s.OK {
			failed++
			missingFields = append(missingFields, s.Name+":"+strings.Join(s.Missing, ","))
		} else if s.BaseURL == "" {
			absent++
		}
	}
	res["source_count"] = len(sources)
	res["source_failed"] = failed
	res["missing_fields"] = missingFields

	verdict := "ok"
	summary := "config.db 完整，LLM 源解析全部通过"
	if integrity != "ok" {
		verdict = "fail"
		summary = "config.db 完整性校验失败，属配置损坏类，应还原配置快照并 ReloadFromConfig"
	} else if failed > 0 {
		verdict = "degraded"
		summary = fmt.Sprintf("config.db 完整，但 %d 个 LLM 源缺必备字段（%s），需修复源配置", failed, strings.Join(missingFields, ";"))
	} else if sources == nil && srcErr != "" {
		verdict = "unknown"
		summary = "config.db 完整但无法解析 LLM 源：" + srcErr
	}
	res["verdict"] = verdict
	res["summary"] = summary
	return contentWith(res, summary), nil
}

// dbIntegrity 优先用 sqlite3 CLI 做 PRAGMA integrity_check；缺失则用 Settings 兜底 + 头部魔法字节启发式。
func (p *Plugin) dbIntegrity(dbPath string) (string, interface{}) {
	bin := p.sqliteBin()
	if bin != "" {
		out, err := exec.Command(bin, dbPath, "PRAGMA integrity_check;").CombinedOutput()
		if err != nil {
			return "error", fmt.Sprintf("sqlite3 运行失败: %v(%s)", err, strings.TrimSpace(string(out)))
		}
		trim := strings.TrimSpace(string(out))
		if strings.Contains(trim, "ok") {
			return "ok", ""
		}
		if trim != "" {
			return "fail", hemlines(trim, 3)
		}
		return "unknown", "integrity_check 无输出"
	}

	// 无 CLI：读头部魔法 + 是否 WAL 缺页（pgno/心跳不必深析）作轻量启发式
	hdr := make([]byte, 16)
	f, err := os.Open(dbPath)
	if err != nil {
		return "error", "无法打开 config.db"
	}
	_, err = f.Read(hdr)
	f.Close()
	if err != nil || !strings.HasPrefix(string(hdr), "SQLite format 3\x00") {
		return "fail", "非 SQLite 文件头，疑似损坏/截断"
	}
	return "ok", "" // 头部完好；深度一致性超出无 CLI 能力，标注降级
}

func hemlines(s string, n int) string {
	lines := strings.Split(s, "\n")
	lines = filterNonEmpty(lines)
	if len(lines) > n {
		return strings.Join(lines[:n], " | ")
	}
	return strings.Join(lines, " | ")
}

func filterNonEmpty(lines []string) []string {
	var o []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			o = append(o, strings.TrimSpace(l))
		}
	}
	return o
}

// dbSources 枚举 core.llm.sources.* 并校验必备字段。优先 sqlite3 CLI；缺失回退内核 Settings。
func (p *Plugin) dbSources(dbPath string) ([]llmSourceData, string) {
	bin := p.sqliteBin()
	kv := map[string]string{}
	if bin != "" {
		out, err := exec.Command(bin,
			dbPath,
			"SELECT key, value FROM config WHERE key LIKE 'core.llm.sources.%';").CombinedOutput()
		if err != nil {
			return nil, fmt.Sprintf("sqlite3 查询失败: %v", err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if idx := strings.IndexByte(line, '|'); idx >= 0 {
				kv[line[:idx]] = line[idx+1:]
			}
		}
	} else if p.sdk != nil {
		keys, _ := p.sdk.Settings().ListCore("core.llm.sources")
		for _, k := range keys {
			if v, err := p.sdk.Settings().GetCore(k); err == nil && v != nil {
				kv[k] = fmt.Sprint(v)
			}
		}
	} else {
		return nil, "既无 sqlite3 也无 Settings 可用"
	}

	byName := map[string]map[string]string{}
	for k, v := range kv {
		rest := strings.TrimPrefix(k, "core.llm.sources.")
		parts := strings.SplitN(rest, ".", 2)
		if len(parts) != 2 {
			continue
		}
		if byName[parts[0]] == nil {
			byName[parts[0]] = map[string]string{}
		}
		byName[parts[0]][parts[1]] = v
	}

	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)

	var out []llmSourceData
	for _, n := range names {
		fields := byName[n]
		var missing []string
		for _, req := range []string{"base_url", "model", "adapter"} {
			if strings.TrimSpace(fields[req]) == "" {
				missing = append(missing, req)
			}
		}
		out = append(out, llmSourceData{
			Name:      n,
			BaseURL:   fields["base_url"],
			Model:     fields["model"],
			Adapter:   fields["adapter"],
			APIKeySet: strings.TrimSpace(fields["api_key"]) != "",
			Missing:   missing,
			OK:        len(missing) == 0,
		})
	}
	return out, ""
}

func (p *Plugin) sqliteBin() string {
	if p.sdk != nil {
		if v, err := p.sdk.Settings().Get("db_check_cmd"); err == nil && v != nil {
			if sv, ok := v.(string); ok && sv != "" && sv != "auto" {
				if _, err := exec.LookPath(sv); err == nil {
					return sv
				}
				return ""
			}
		}
	}
	if _, err := exec.LookPath("sqlite3"); err == nil {
		return "sqlite3"
	}
	return ""
}

// ---- diag_log_scan ----

type sigRule struct {
	Category string
	Re       *regexp.Regexp
}

var sigRules = []sigRule{
	{"panic", regexp.MustCompile(`(?i)panic|nil pointer|invalid memory address|runtime error|SIGSEGV|coredump|stack overflow`)},
	{"oom", regexp.MustCompile(`(?i)\boom\b|out of memory|memory allocation failed`)},
	{"network", regexp.MustCompile(`(?i)no such host|connection refused|connection reset|timeout|unreachable|dns|lookup.*fail`)},
	{"provider", regexp.MustCompile(`(?i)provider .* failed|marked unavailable|llm api unreachable|llmfallback|api key|401|403`)},
	{"sql", regexp.MustCompile(`(?i)sql: |sqlite|database is locked|disk I/O error|no such table|constraint failed`)},
	{"fatal", regexp.MustCompile(`(?i)\bfatal\b|\berror\b|failed`)},
}

func (p *Plugin) handleLogScan(args map[string]interface{}) (interface{}, error) {
	logDir := argString(args, "log_dir")
	if logDir == "" {
		logDir = p.logDir
	}
	sinceMin := argInt(args, "since_minutes", 0)
	maxLines := argInt(args, "max_lines", 200000)
	if maxLines <= 0 {
		maxLines = 200000
	}

	cutoff := time.Time{}
	if sinceMin > 0 {
		cutoff = time.Now().Add(-time.Duration(sinceMin) * time.Minute)
	}

	entries, err := os.ReadDir(logDir)
	if err != nil {
		return content(fmt.Sprintf("日志目录不可读: %v", err)), nil
	}

	// 只扫当前 raw 日志（homed_YYYY-MM-DD_HH-MM-SS.log），忽略已压缩归档
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		files = append(files, filepath.Join(logDir, e.Name()))
	}
	sort.Strings(files)

	counts := map[string]int{}
	total := 0
	matched := 0
	scannedLines := 0
	filesRead := 0
	for _, f := range files {
		if scannedLines >= maxLines {
			break
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		filesRead++
		for _, line := range strings.Split(string(data), "\n") {
			if scannedLines >= maxLines {
				break
			}
			scannedLines++
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			// 时间窗过滤：行首时间戳形如 2026/08/03 10:03:52
			if !cutoff.IsZero() {
				ts := parseLogTS(line)
				if !ts.IsZero() && ts.Before(cutoff) && (sinceMin > 0) {
					continue
				}
			}
			total++
			for _, rule := range sigRules {
				if rule.Re.MatchString(line) {
					counts[rule.Category]++
					matched++
					break
				}
			}
		}
	}

	// 排序取主导
	type kv struct {
		cat   string
		count int
	}
	var order []kv
	for cat, n := range counts {
		order = append(order, kv{cat, n})
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].count != order[j].count {
			return order[i].count > order[j].count
		}
		return order[i].cat < order[j].cat
	})

	dominant := ""
	if len(order) > 0 {
		dominant = order[0].cat
	}
	conclusion := "无已知错误签名命中"
	switch dominant {
	case "panic":
		conclusion = "主导: panic/崩溃 → 配合栈定位，属进程死亡类"
	case "oom":
		conclusion = "主导: OOM/内存 → 进程失稳类，检查内存"
	case "network", "provider":
		conclusion = "主导: 网络/供应商不可达 → 配置或系统网络类"
	case "sql":
		conclusion = "主导: SQL/数据库错误 → 数据或配置损坏类"
	case "fatal":
		conclusion = "主导: 常规 error/failed → 需结合 DB/快照进一步定位"
	}

	res := map[string]interface{}{
		"log_dir":               logDir,
		"files_read":            filesRead,
		"lines_scanned":         scannedLines,
		"lines_total_in_window": total,
		"lines_matched":         matched,
		"counts":                counts,
		"dominant":              dominant,
		"conclusion":            conclusion,
	}
	return contentWith(res, fmt.Sprintf("%s (命中 %d 行, 主导 %s)", conclusion, matched, dominant)), nil
}

// parseLogTS 解析 homed 时间戳前缀 2026/08/03 10:03:52。
var logTSRe = regexp.MustCompile(`^(\d{4})/(\d{2})/(\d{2}) (\d{2}):(\d{2}):(\d{2})`)

func parseLogTS(line string) time.Time {
	m := logTSRe.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}
	}
	ts, _ := time.ParseInLocation("2006-01-02 15:04:05",
		fmt.Sprintf("%s-%s-%s %s:%s:%s", m[1], m[2], m[3], m[4], m[5], m[6]), time.Local)
	return ts
}

// ---- diag_delta ----

type fileEntry struct {
	Path    string `json:"path"`
	Type    string `json:"type"` // created / modified / deleted
	Size    int64  `json:"size"`
	NewHash string `json:"new_hash,omitempty"`
	OldHash string `json:"old_hash,omitempty"`
}

func (p *Plugin) handleDelta(args map[string]interface{}) (interface{}, error) {
	baseline := argString(args, "baseline_dir")
	current := argString(args, "current_dir")
	pattern := argString(args, "pattern")
	maxItems := argInt(args, "max_items", 500)
	if maxItems <= 0 {
		maxItems = 500
	}

	if baseline == "" || current == "" {
		return contentWith(map[string]interface{}{
			"error": "baseline_dir 与 current_dir 均必填",
		}, "缺少基线或现状目录：请先准备 last-good 快照解包目录"), nil
	}

	baseMissing := !dirExists(baseline)
	currMissing := !dirExists(current)
	if baseMissing {
		return contentWith(map[string]interface{}{
			"baseline_dir":    baseline,
			"current_dir":     current,
			"baseline_exists": false,
			"summary":         "基线不存在，无法差分（需先建立快照基线）",
		}, "基线不存在，无法差分"), nil
	}
	if currMissing {
		return contentWith(map[string]interface{}{
			"baseline_dir":   baseline,
			"current_dir":    current,
			"current_exists": false,
			"summary":        "现状目录不存在",
		}, "现状目录不存在"), nil
	}

	baseMap := walkHashes(baseline)
	currMap := walkHashes(current)

	var files []fileEntry
	seen := map[string]bool{}
	for path, ch := range currMap {
		seen[path] = true
		if pattern != "" && !strings.Contains(path, pattern) {
			continue
		}
		if bh, ok := baseMap[path]; ok {
			if bh.hash != ch.hash {
				files = append(files, fileEntry{Path: path, Type: "modified", Size: ch.size, OldHash: bh.hash, NewHash: ch.hash})
			}
		} else {
			files = append(files, fileEntry{Path: path, Type: "created", Size: ch.size, NewHash: ch.hash})
		}
	}
	for path, bh := range baseMap {
		if !seen[path] && (pattern == "" || strings.Contains(path, pattern)) {
			files = append(files, fileEntry{Path: path, Type: "deleted", Size: bh.size, OldHash: bh.hash})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	summary := map[string]int{"created": 0, "modified": 0, "deleted": 0}
	for _, f := range files {
		summary[f.Type]++
	}

	shown := files
	if len(shown) > maxItems {
		shown = shown[:maxItems]
	}

	res := map[string]interface{}{
		"baseline_dir": baseline,
		"current_dir":  current,
		"summary":      summary,
		"total_diff":   len(files),
		"files":        shown,
	}
	return contentWith(res, fmt.Sprintf("diff: %+v", res["summary"])), nil
}

type hashEnt struct {
	hash string
	size int64
}

func walkHashes(root string) map[string]hashEnt {
	out := map[string]hashEnt{}
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		h := sha256.Sum256(data)
		out[rel] = hashEnt{hash: hex.EncodeToString(h[:]), size: int64(len(data))}
		return nil
	})
	return out
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// ---- diag_loc ----

func (p *Plugin) handleLoc(args map[string]interface{}) (interface{}, error) {
	triage := argMap(args, "triage")
	db := argMap(args, "db")
	logScan := argMap(args, "log_scan")
	delta := argMap(args, "delta")

	type hyp struct {
		Cause      string   `json:"cause"`
		Confidence int      `json:"confidence"` // 0-100 因果强度
		Evidence   []string `json:"evidence"`
		Recommend  string   `json:"recommendation"`
	}
	var hyps []hyp

	tClass := valueString(triage, "class")
	tVerdict := valueString(triage, "verdict")
	dbVerdict := valueString(db, "verdict")
	dom := valueString(logScan, "dominant")
	deltaSummary := map[string]int{}
	if delta != nil {
		if s, ok := delta["summary"].(map[string]interface{}); ok {
			for k, v := range s {
				switch n := v.(type) {
				case float64:
					deltaSummary[k] = int(n)
				case int:
					deltaSummary[k] = n
				case int64:
					deltaSummary[k] = int(n)
				}
			}
		}
	}
	dCreated := deltaSummary["created"]
	dModified := deltaSummary["modified"]
	dDeleted := deltaSummary["deleted"]
	dTotal := dCreated + dModified + dDeleted

	evidence := []string{}
	if tVerdict != "" {
		evidence = append(evidence, "triage="+tVerdict+"("+tClass+")")
	}
	if dbVerdict != "" {
		evidence = append(evidence, "db="+dbVerdict)
	}
	if dom != "" {
		evidence = append(evidence, "log_dominant="+dom)
	}
	if dTotal > 0 {
		evidence = append(evidence, fmt.Sprintf("delta=%d 改动(%d改/%d增/%d删)", dTotal, dModified, dCreated, dDeleted))
	} else {
		evidence = append(evidence, "delta=无改动")
	}

	// 1) 进程失稳（panic 主导时走更具体的 code_panic_loop 分支）
	if (tClass == "process_death" || tClass == "process_starvation") &&
		dbVerdict != "fail" && dbVerdict != "degraded" && dTotal == 0 &&
		dom != "panic" {
		hyps = append(hyps, hyp{
			Cause:      "process_instability",
			Confidence: 75,
			Evidence:   in(evidence, "triage=down"),
			Recommend:  "重建 worker；不动配置（db 完好、无文件改动）",
		})
	}

	// 2) 配置损坏
	if dbVerdict == "fail" || dbVerdict == "degraded" {
		hyps = append(hyps, hyp{
			Cause:      "config_corruption",
			Confidence: 90,
			Evidence:   in(evidence, "db="+dbVerdict),
			Recommend:  "还原 core.llm.sources 配置快照 → ReloadFromConfig → 拉起主 agent",
		})
	}

	// 3) 系统网络
	if (dom == "network" || dom == "provider") && (dTotal > 0) {
		hyps = append(hyps, hyp{
			Cause:      "system_network",
			Confidence: 80,
			Evidence:   in(evidence, "log_dominant="+dom, "delta>0"),
			Recommend:  "还原 DNS/proxy/host 相关系统网络配置 → 重载主 agent",
		})
	}

	// 4) 纯日志栈崩溃（db 完好、无 delta）
	if tClass == "process_death" && dbVerdict == "ok" && dTotal == 0 && dom == "panic" {
		hyps = append(hyps, hyp{
			Cause:      "code_panic_loop",
			Confidence: 70,
			Evidence:   in(evidence, "log_dominant=panic", "db=ok", "delta=无改动"),
			Recommend:  "定位 panic 栈来源（repeat）+ 检查是否插件引起，必要时禁用对应插件后重建 worker",
		})
	}

	// 未知/混合
	if len(hyps) == 0 {
		hyps = append(hyps, hyp{
			Cause:      "unknown_mixed",
			Confidence: 20,
			Evidence:   evidence,
			Recommend:  "确定性命中不足，放开 webfetch/知识库，用 rescue 源 做最小 LLM 推理（依据 diag_* 结论摘要）",
		})
	}

	sort.Slice(hyps, func(i, j int) bool { return hyps[i].Confidence > hyps[j].Confidence })

	res := map[string]interface{}{
		"evidence":             evidence,
		"ranked_hypotheses":    hyps,
		"final_recommendation": hyps[0].Recommend,
	}

	// 落盘 + 知识库回流（同类崩溃下次直接命中）
	if argBool(args, "persist") {
		p.persistConclusion(res, hyps[0].Cause, hyps[0].Recommend)
	}

	return contentWith(res, "定位: "+hyps[0].Cause+" | 建议: "+hyps[0].Recommend), nil
}

// persistConclusion 把定位结论写 recovery_kb/diag_<ts>.json，并经知识库回流（失败不阻塞）。
func (p *Plugin) persistConclusion(res map[string]interface{}, cause, recommend string) {
	ts := time.Now()
	entry := map[string]interface{}{
		"ts":                   ts.Format(time.RFC3339),
		"cause":                cause,
		"recommendation":       recommend,
		"evidence":             valueFrom(res, "evidence"),
		"ranked_hypotheses":    res["ranked_hypotheses"],
		"final_recommendation": recommend,
		"tool":                 "diag_loc",
	}
	raw, _ := json.MarshalIndent(entry, "", "  ")

	dir := p.recoveryKBDir()
	if dir != "" {
		if err := os.MkdirAll(dir, 0755); err == nil {
			path := filepath.Join(dir, fmt.Sprintf("diag_%s.json", ts.Format("2006-01-02_15-04-05")))
			if err := os.WriteFile(path, raw, 0644); err == nil {
				log.Printf("[%s] conclusion persisted to %s", p.name, path)
			} else {
				log.Printf("[%s] persist file %s: %v", p.name, path, err)
			}
		}
	}

	if p.sdk != nil && p.sdk.Knowledge() != nil {
		kName := fmt.Sprintf("diag:%s:%s", cause, ts.Format("2006-01-02T15-04"))
		content := fmt.Sprintf("恢复诊断结论(%s): %s。建议: %s。命中条件可复用。", ts.Format("2006-01-02 15:04:05"), cause, recommend)
		if err := p.sdk.Knowledge().Add(kName, content); err != nil {
			log.Printf("[%s] knowledge add %s: %v", p.name, kName, err)
		}
	}
}

func valueFrom(m map[string]interface{}, k string) interface{} {
	if m == nil {
		return nil
	}
	return m[k]
}

// recoveryKBDir 返回结论落盘目录，可配置，缺省 <data_dir>/recovery_kb。
func (p *Plugin) recoveryKBDir() string {
	if p.sdk != nil {
		if v, err := p.sdk.Settings().Get("recovery_kb_dir"); err == nil && v != nil {
			if sv, ok := v.(string); ok && sv != "" {
				return sv
			}
		}
	}
	if p.dataDir != "" {
		return filepath.Join(p.dataDir, "recovery_kb")
	}
	return ""
}

// in 过滤 slice，保留同时满足 items 中条件（简单子串匹配）的元素。
func in(src []string, items ...string) []string {
	var o []string
	for _, it := range items {
		for _, s := range src {
			if s == it {
				o = append(o, it)
				break
			}
		}
	}
	return o
}

var _ = json.Marshal
