package lua

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteBundledAdaptersSkipsUserModified 钉住自动更新的**安全边界**。
//
// 背景：writeBundledAdapters 原本是 `if 文件已存在 { continue }`，
// 于是「修好适配器 → 升级二进制 → 已部署实例上的文件不更新」。
// 这就是仓库 openai.lua 缺 stream_index 透传、而生产早有（2026-08-26 15:46
// 手工补上，比入库早 32 分钟）却长期没人发现的机制性原因。
//
// 改成按内容判断后，**必须**守住两条边界，否则会吞掉用户的改动：
//
//	① 用户**改过**的文件（内容与上次内嵌的 embed 不同）⇒ 不动它
//	② 与上次内嵌**一致**的文件（只是没跟上新版本）⇒ 用新的覆盖
//
// 为什么不能无条件覆盖：adapter_path 是可配置项，用户可以把 adapter_path
// 指向自己维护的适配器。内核内置那 10 个文件虽在 DataDir 下，但"用户改了
// 内置适配器"是现实存在的用法 —— 无条件覆盖等于静默丢弃他们的修改。
func TestWriteBundledAdaptersSkipsUserModified(t *testing.T) {
	dir := t.TempDir()
	vm := NewVM(dir)

	// 场景 A：用户把 openai.lua 改成了自己的版本
	// 假适配器必须**功能完整**（含 transform_response 等钩子），
	// 否则后面"用户改过的仍能加载"那条断言会因为缺函数而失败 ——
	// 那是 fixture 的问题，不是保护逻辑的问题。
	custom := `local adapter = {}
adapter.name = "openai"
function adapter.transform_request(r) return r end
function adapter.transform_response(r) return r end
function adapter.transform_stream_chunk(r) return r end
return adapter
`
	if err := os.WriteFile(filepath.Join(dir, "openai.lua"), []byte(custom), 0644); err != nil {
		t.Fatal(err)
	}
	// 场景 B：anthropic.lua 内容是"上次内嵌的版本"（没跟上新版）
	prev, err := bundledAdapters.ReadFile("adapters/anthropic.lua")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "anthropic.lua"), prev, 0644); err != nil {
		t.Fatal(err)
	}

	// Start 会 mkdir + writeBundledAdapters + 逐个 LoadAdapter（vm.go:173）
	if err := vm.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// ① 用户改过的 openai.lua 必须原样保留
	got, err := os.ReadFile(filepath.Join(dir, "openai.lua"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != custom {
		t.Errorf("用户改过的 openai.lua 被覆盖了\n  期望：%q\n  实际：%q",
			custom, string(got))
	}
	// 而且它应该仍被加载成功（用户改的能用）
	if _, err := vm.CallTransformResponse("openai", "{}"); err != nil {
		t.Errorf("用户改过的 openai.lua 加载失败：%v", err)
	}

	// ② 与上次内嵌一致的 anthropic.lua 应保持一致（幂等，不反复改写）
	gotA, err := os.ReadFile(filepath.Join(dir, "anthropic.lua"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotA) != string(prev) {
		t.Error("anthropic.lua 内容被改动了 —— 与上次内嵌一致的文件应保持不变（幂等）")
	}
}

// TestBundledAdapterMatchesEmbedded 确认内嵌内容与源文件一致。
//
// 这是"用户改过"的判据基准：VM 必须拿**当前内嵌**的版本做比对。
func TestBundledAdapterMatchesEmbedded(t *testing.T) {
	for _, name := range bundledAdapterNames {
		b, err := bundledAdapters.ReadFile("adapters/" + name + ".lua")
		if err != nil {
			t.Errorf("%s: 内嵌读取失败 %v", name, err)
			continue
		}
		if len(b) == 0 {
			t.Errorf("%s: 内嵌内容为空", name)
		}
	}
}

// TestWriteBundledAdaptersUpdatesStale 验证**正向**路径：没跟上新版本的文件
// 确实被覆盖。
//
// 只测"用户改过的不被覆盖"是不够的 —— 那样一个"永远不覆盖任何文件"的
// 实现也能全绿，而那正是我们要修的病。
func TestWriteBundledAdaptersUpdatesStale(t *testing.T) {
	dir := t.TempDir()

	// 首跑：解包 + 写清单
	vm1 := NewVM(dir)
	if err := vm1.Start(); err != nil {
		t.Fatalf("首跑 Start: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".bundled")); err != nil {
		t.Fatalf("首跑应写出 .bundled 清单：%v", err)
	}

	// 场景：把 groq.lua 换成"用户版本"（与首跑内嵌不同）
	groqPath := filepath.Join(dir, "groq.lua")
	cur, err := bundledAdapters.ReadFile("adapters/groq.lua")
	if err != nil {
		t.Fatal(err)
	}
	if string(cur) == "" {
		t.Fatal("groq.lua 内嵌为空")
	}
	// 模拟"内核版本变了"：改清单里的哈希，让它认为 groq 落后于新内嵌
	man, err := os.ReadFile(filepath.Join(dir, ".bundled"))
	if err != nil {
		t.Fatal(err)
	}
	stale := strings.ReplaceAll(string(man), groqLineHash(man), "deadbeef")
	if err := os.WriteFile(filepath.Join(dir, ".bundled"), []byte(stale), 0644); err != nil {
		t.Fatal(err)
	}

	// 二跑：应把 groq.lua 更新回内嵌版本
	vm2 := NewVM(dir)
	if err := vm2.Start(); err != nil {
		t.Fatalf("二跑 Start: %v", err)
	}
	got, err := os.ReadFile(groqPath)
	if err != nil {
		t.Fatal(err)
	}
	if sha256Hex(got) != sha256Hex(cur) {
		t.Errorf("落后的 groq.lua 未被更新回内嵌版本（这是本次要修的病）\n"+
			"  盘上 %d 字节 / 内嵌 %d 字节", len(got), len(cur))
	}

	// 幂等：三跑不应再改任何文件
	before, _ := os.ReadFile(groqPath)
	vm3 := NewVM(dir)
	if err := vm3.Start(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(groqPath)
	if string(before) != string(after) {
		t.Error("已是最新版本却被再次改写 —— 更新不幂等")
	}
}

// groqLineHash 从清单里取出 groq 那行的哈希。
func groqLineHash(manifest []byte) string {
	for _, line := range strings.Split(string(manifest), "\n") {
		if strings.HasPrefix(line, "groq\t") {
			return strings.TrimPrefix(line, "groq\t")
		}
	}
	return ""
}
