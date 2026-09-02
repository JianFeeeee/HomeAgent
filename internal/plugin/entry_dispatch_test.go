package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

// entry 分派骨架（docs/zh/plugin-migration-plan.md Part 1）：
// 外部插件多进程化期间 .so/.dll（cabi）与 .bin（proc）双通道共存，
// 按 plugin.json 的 entry 字段分派，使迁移可逐插件推进、随时回退。

func TestClassifyEntry(t *testing.T) {
	cases := []struct {
		entry string
		want  entryKind
	}{
		{"plugin.so", entryCABI},
		{"plugin.dll", entryCABI},
		{"plugin.dylib", entryCABI},
		{"plugin.bin", entryProc},
		{"main.lua", entryLua},
		{"SKILL.md", entrySkill},
		{"", entryUnknown},
		{"plugin.wasm", entryUnknown},
	}
	for _, c := range cases {
		if got := classifyEntry(c.entry); got != c.want {
			t.Errorf("classifyEntry(%q) = %v, want %v", c.entry, got, c.want)
		}
	}
}

// manifest 显式声明的 entry 优先级最高。
func TestDetectEntryKind_ManifestWins(t *testing.T) {
	dir := t.TempDir()
	// 目录里放 .so，但 manifest 声明 .bin → 应走 proc
	mustWrite(t, filepath.Join(dir, "plugin.so"), "fake so")
	mustWrite(t, filepath.Join(dir, "plugin.bin"), "fake bin")
	mustWrite(t, filepath.Join(dir, metaEntry), `{"name":"x","entry":"plugin.bin"}`)

	if got := detectEntryKind(dir); got != entryProc {
		t.Fatalf("manifest 声明 plugin.bin 应走 proc，实际 %v", got)
	}
}

// manifest 声明 .so 时即便存在 .bin 也走 cabi —— 这是回退路径的保证。
func TestDetectEntryKind_ManifestCanForceRollback(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "plugin.so"), "fake so")
	mustWrite(t, filepath.Join(dir, "plugin.bin"), "fake bin")
	mustWrite(t, filepath.Join(dir, metaEntry), `{"name":"x","entry":"plugin.so"}`)

	if got := detectEntryKind(dir); got != entryCABI {
		t.Fatalf("manifest 声明 plugin.so 应回退到 cabi，实际 %v", got)
	}
}

// 无 manifest（或 entry 为空）时按目录探测，.bin 优先于 .so：
// 迁移期间同目录可能两种产物共存（升级未清理），此时应走新通道。
func TestDetectEntryKind_ProbeOrderPrefersBin(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "plugin.so"), "fake so")
	mustWrite(t, filepath.Join(dir, "plugin.bin"), "fake bin")

	if got := detectEntryKind(dir); got != entryProc {
		t.Fatalf("无 manifest 时应优先 plugin.bin，实际 %v", got)
	}
}

func TestDetectEntryKind_ProbeFallbacks(t *testing.T) {
	t.Run("only so", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "plugin.so"), "x")
		if got := detectEntryKind(dir); got != entryCABI {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("only lua", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "main.lua"), "x")
		if got := detectEntryKind(dir); got != entryLua {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("only skill", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "SKILL.md"), "x")
		if got := detectEntryKind(dir); got != entrySkill {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("empty dir", func(t *testing.T) {
		if got := detectEntryKind(t.TempDir()); got != entryUnknown {
			t.Fatalf("空目录应为 unknown，实际 %v", got)
		}
	})
}

// entry 声明 plugin.bin 但二进制缺失时必须报明确错误，
// 不得静默回退到 cabi —— 否则"已迁移插件跑回旧通道"极难排查。
func TestTryLoadProc_MissingBinaryReturnsNil(t *testing.T) {
	dir := t.TempDir()
	plg, err := tryLoadProc(dir, "demo", nil)
	if plg != nil || err != nil {
		t.Fatalf("无 plugin.bin 应返回 nil,nil（交由后续探测），实际 plg=%v err=%v", plg, err)
	}
}

func TestTryLoadProc_NonExecutableRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, binEntry)
	mustWrite(t, path, "not executable")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := tryLoadProc(dir, "demo", nil)
	if err == nil {
		t.Fatal("缺少可执行权限应报错")
	}
}

// pluginEntryHash 的候选顺序须与 detectEntryKind 一致（plugin.bin 优先），
// 否则增量重载会用错文件算 hash，导致"换了 .bin 但内核以为没变"。
func TestPluginEntryHash_PrefersBin(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "plugin.so"), "so content")
	mustWrite(t, filepath.Join(dir, "plugin.bin"), "bin content")

	h1 := pluginEntryHash(dir)
	if h1 == "" {
		t.Fatal("应算出 hash")
	}

	// 改 .so 不应影响 hash（因为以 .bin 为准）
	mustWrite(t, filepath.Join(dir, "plugin.so"), "so content CHANGED")
	if h2 := pluginEntryHash(dir); h2 != h1 {
		t.Error("plugin.bin 存在时 hash 不应受 plugin.so 变化影响")
	}

	// 改 .bin 必须改变 hash
	mustWrite(t, filepath.Join(dir, "plugin.bin"), "bin content CHANGED")
	if h3 := pluginEntryHash(dir); h3 == h1 {
		t.Error("plugin.bin 变化必须反映到 hash（否则增量重载失效）")
	}
}

func TestPluginEntryHash_EmptyForFactoryOnlyPlugin(t *testing.T) {
	if h := pluginEntryHash(t.TempDir()); h != "" {
		t.Errorf("无入口文件应返回空串（内置纯工厂插件），实际 %q", h)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("写 %s: %v", path, err)
	}
}
