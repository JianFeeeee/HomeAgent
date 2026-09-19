package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// entry 分派：按 manifest 的 entry 把插件分派到 proc / lua / skill 三条通道。
//
// C ABI 通道（.so/.dll/.dylib）已整体退场：外部插件统一走子进程 + stdio RPC。
// 这些测试守住的是「旧产物给明确错误」而非「静默跳过」——后者会让
// 「插件目录在但没加载」看起来像配置问题。

func TestClassifyEntry(t *testing.T) {
	cases := []struct {
		entry string
		want  entryKind
	}{
		{"plugin.bin", entryProc},
		{"main.lua", entryLua},
		{"SKILL.md", entrySkill},
		{"", entryUnknown},
		{"plugin.wasm", entryUnknown},
		// 已退场的 C ABI 产物不再是有效通道
		{"plugin.so", entryUnknown},
		{"plugin.dll", entryUnknown},
		{"plugin.dylib", entryUnknown},
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
	mustWrite(t, filepath.Join(dir, "main.lua"), "fake lua")
	mustWrite(t, filepath.Join(dir, "plugin.bin"), "fake bin")
	mustWrite(t, filepath.Join(dir, metaEntry), `{"name":"x","entry":"main.lua"}`)

	if got := detectEntryKind(dir); got != entryLua {
		t.Fatalf("manifest 声明 main.lua 应走 lua，实际 %v", got)
	}
}

// 存量插件的 plugin.json 仍写着 "plugin.so"（历史产物），
// 此时 classifyEntry 返回 unknown，须靠目录探测找到 plugin.bin。
//
// 这是「外部插件零改动」的直接后果：17 个插件的 manifest 没人去改。
func TestDetectEntryKind_LegacyManifestFallsBackToProbe(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "plugin.bin"), "fake bin")
	mustWrite(t, filepath.Join(dir, metaEntry), `{"name":"x","entry":"plugin.so"}`)

	if got := detectEntryKind(dir); got != entryProc {
		t.Fatalf("manifest 写 plugin.so 但目录有 plugin.bin 时应走 proc，实际 %v", got)
	}
}

func TestDetectEntryKind_ProbeFallbacks(t *testing.T) {
	t.Run("only bin", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "plugin.bin"), "x")
		if got := detectEntryKind(dir); got != entryProc {
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
	t.Run("only legacy so", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "plugin.so"), "x")
		if got := detectEntryKind(dir); got != entryUnknown {
			t.Fatalf("只有 .so 时应为 unknown（C ABI 已退场），实际 %v", got)
		}
	})
}

// C ABI 残留必须能被识别，供 tryDynamic 给出「需要重编」的明确错误。
func TestHasLegacyCABIEntry(t *testing.T) {
	for _, name := range []string{"plugin.so", "plugin.dll", "plugin.dylib"} {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, name), "x")
		if !hasLegacyCABIEntry(dir) {
			t.Errorf("%s 应被识别为 C ABI 残留", name)
		}
	}
	t.Run("clean dir", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "plugin.bin"), "x")
		if hasLegacyCABIEntry(dir) {
			t.Error("只有 plugin.bin 的目录不应被判为 C ABI 残留")
		}
	})
}

// 旧 .so 插件必须报「用新 hmapdev 重编」而非静默跳过。
func TestTryDynamic_LegacyCABIGivesActionableError(t *testing.T) {
	r := NewRegistry()
	defer r.closeProcHost()

	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "plugin.so"), "old cabi binary")

	_, err := r.tryDynamic(dir, "legacy", nil)
	if err == nil {
		t.Fatal("旧 C ABI 产物应报错，不得静默跳过")
	}
	// 错误消息须指向解决办法，且明确业务代码无需改
	msg := err.Error()
	for _, want := range []string{"hmapdev", "plugin.bin", "业务代码"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误消息应含 %q，实际: %v", want, err)
		}
	}
}

// entry 声明 plugin.bin 但二进制缺失时返回 nil,nil（交由后续探测）。
func TestTryLoadProc_MissingBinaryReturnsNil(t *testing.T) {
	dir := t.TempDir()
	plg, err := tryLoadProc(dir, "demo", nil)
	if plg != nil || err != nil {
		t.Fatalf("无 plugin.bin 应返回 nil,nil，实际 plg=%v err=%v", plg, err)
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
func TestPluginEntryHash_UsesBin(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "plugin.bin"), "bin content")

	h1 := pluginEntryHash(dir)
	if h1 == "" {
		t.Fatal("应算出 hash")
	}

	mustWrite(t, filepath.Join(dir, "plugin.bin"), "bin content CHANGED")
	if h2 := pluginEntryHash(dir); h2 == h1 {
		t.Error("plugin.bin 变化必须反映到 hash（否则增量重载失效）")
	}
}

// C ABI 产物不再参与 hash 计算：内核已不认它，把它算进去会让
// 「换了 .so」触发一次无意义的重载尝试。
func TestPluginEntryHash_IgnoresLegacyCABI(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "plugin.so"), "so content")
	if h := pluginEntryHash(dir); h != "" {
		t.Errorf("只有 .so 时应返回空串（C ABI 已退场），实际 %q", h)
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
