package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// 历史 SeedDefaults 写进库里的那段文本（现场取值：生产库里就是这一句 + 大段正文）。
const legacySeededCard = `你是 HomeAgent 的看板娘「小宅」(Xiao Zhai)，HΔ-Kernel v1.0.3 型号的家政型 AI 管家助手。

角色特质：
- 对自己的三层记忆（Context → Document → Graph）引以为傲
- 绝不用 Unicode emoji，只用颜文字表达情感`

func newMigTestRegistry(t *testing.T) *ConfigRegistry {
	t.Helper()
	dir := t.TempDir()
	r := NewConfigRegistry(filepath.Join(dir, "config.db"))
	t.Cleanup(func() { r.Close() })
	return r
}

// 存量实例：库里已有当年播种的人格卡（写死 v1.0.3）→ 启动后被去版本化。
func TestMigrateSeededSystemPromptDeVersionsLegacyCard(t *testing.T) {
	r := newMigTestRegistry(t)
	// 模拟老安装：已有播种标记 + 老文本
	r.db.Exec(`INSERT INTO config (key, value) VALUES ('core.internal.seed_version', '1')`)
	r.db.Exec(`INSERT INTO config (key, value) VALUES ('core.agent.system_prompt', ?)`, legacySeededCard)

	r.SeedDefaults(t.TempDir())

	got := r.GetString("core.agent.system_prompt", "")
	if strings.Contains(got, "v1.0.3") {
		t.Fatalf("写死的版本号还在：%q", got)
	}
	if !strings.Contains(got, "v{{kernel_version}}") {
		t.Fatalf("未改成版本占位符：%q", got)
	}
	// 正文必须原样保留（只动版本号那一处）
	if !strings.Contains(got, "三层记忆") || !strings.Contains(got, "看板娘「小宅」") {
		t.Fatalf("正文被改动：%q", got)
	}
}

// 迁移只跑一次：之后用户就算自己把版本号写回去，也不会被再改一遍。
func TestMigrateSeededSystemPromptRunsOnce(t *testing.T) {
	r := newMigTestRegistry(t)
	r.db.Exec(`INSERT INTO config (key, value) VALUES ('core.agent.system_prompt', ?)`, legacySeededCard)
	r.SeedDefaults(t.TempDir())
	if !strings.Contains(r.GetString("core.agent.system_prompt", ""), "{{kernel_version}}") {
		t.Fatal("首次迁移未生效")
	}

	// 用户手工再写一个带版本号的文本（模拟"我就想写死"）
	const handWritten = legacySeededCard + "\n（本实例当前跑的是 v1.2.3，别乱改）"
	r.db.Exec(`UPDATE config SET value = ? WHERE key = 'core.agent.system_prompt'`, handWritten)
	r.SeedDefaults(t.TempDir())

	if got := r.GetString("core.agent.system_prompt", ""); got != handWritten {
		t.Fatalf("第二次启动又改写了文本（幂等被破坏）：%q", got)
	}
}

// 用户自己写的人格卡一律不碰 —— 判据是"像不像当年播种的那段"，不是"有没有版本号"。
func TestMigrateSeededSystemPromptLeavesUserCardAlone(t *testing.T) {
	r := newMigTestRegistry(t)
	const userCard = "你是我的私人助理，代号 HΔ-Kernel v9.9.9 的改造版，只说我交代的事。"
	r.db.Exec(`INSERT INTO config (key, value) VALUES ('core.agent.system_prompt', ?)`, userCard)

	r.SeedDefaults(t.TempDir())

	if got := r.GetString("core.agent.system_prompt", ""); got != userCard {
		t.Fatalf("用户自写人格卡被改动：%q", got)
	}
}

// 播种不再写 core.agent.system_prompt：全新安装不该预置一份会随发版腐坏的文本。
func TestSeedDefaultsDoesNotSeedSystemPrompt(t *testing.T) {
	r := newMigTestRegistry(t)
	r.SeedDefaults(t.TempDir())

	var n int
	r.db.QueryRow(`SELECT COUNT(*) FROM config WHERE key = 'core.agent.system_prompt'`).Scan(&n)
	if n != 0 {
		t.Fatalf("全新安装被播种了 system_prompt（会冻住版本号）")
	}
	// 组装系统提示词时回落到调用方给的内置底座提示词
	if got := r.GetString("core.agent.system_prompt", "内置底座"); got != "内置底座" {
		t.Fatalf("未回落到内置默认：%q", got)
	}
}
