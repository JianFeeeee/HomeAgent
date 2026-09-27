package lua

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestBundledAdaptersMatchDeployedOnes **报告**仓库与部署目录的适配器差异。
//
// ★ 刻意**不作为失败判据**（只 t.Log）。
//
//	实测结果：生产部署的适配器普遍比仓库旧（anthropic 2537 vs 5144 字节），
//	而 openai 那份反而领先（仓库曾缺 stream_index、生产早就有）。
//	这是**正常的**——生产实例是长期运行的部署，适配器在它首次创建时就解包落地，
//	之后仓库一直在演进。
//
//	若把"必须一致"写成失败判据，这条判据会在任何老部署上恒红，
//	而恒红的判据会被无视——那等于没有判据，甚至更糟（它会掩盖真正的漂移）。
//	所以这里只负责**把差异摆出来**，由人判断哪一侧才是想要的。
//
// 真正能自动抓 bug 的契约检查在 TestAdapterEmitsOnlyKnownFields：它查的是
// "适配器输出的键名是否在 agentAPI.ToolCall 的契约内"，与部署状态无关。
//
// ★ 这条漂移的具体内容与时间线（解释了为什么仓库长期缺它却没人发现）：
//
//	生产 /home/newqqagent/adapters/openai.lua   4853 字节  含 stream_index
//	仓库 ddef195 时的 openai.lua                4709 字节  不含
//	生产文件时间 2026-08-26 15:46
//	ddef195 提交时间 2026-08-26 16:10
//
// 生产落地比入库**早 32 分钟** —— 该提交的说明里写着「openai.lua 输出
// stream_index 字段」，但 `--stat` 显示它没改 openai.lua。说明修复先在生产
// 环境生效、随后提交入库时漏了这个文件。
//
// 于是"生产能跑多工具、仓库跑不了"这个状态持续了一个月，而两端都没人发现：
// 生产不报问题（它有），仓库的判据也测不到（它直接构造 Go 结构体，
// 绕过适配器）。
//
// ★ 这条判据是被一次误判逼出来的。
//
// 我曾断言「openai.lua 缺 stream_index 透传、批内并发在生产走不通」，并据此
// 写了实现与提交。后来核对生产实例才发现：
//
//	生产 /home/newqqagent/adapters/openai.lua   130 行  含 stream_index
//	仓库（修复前）                              128 行  无 stream_index
//
// 生产**早就有**那个透传 —— 仓库版本落后于生产。而当时没有任何判据能发现这个
// 漂移：判据要么只看仓库（自证），要么只看内核累积逻辑（绕过适配器）。
//
// 漂移为什么能长期存在：vm.go 的 writeBundledAdapters 是
// `if 文件已存在 { continue }` —— 升级二进制**不会更新已部署的适配器文件**。
// 于是「仓库改了适配器但老实例上不生效」与「仓库没改」在现象上完全一样。
//
// ★ 本判据只在**部署目录确实存在时**才检查（本机跑单测的 CI 上没有它），
// 找不到就跳过 —— 不能让判据因为环境差异而恒绿，那等于没有判据。
func TestBundledAdaptersMatchDeployedOnes(t *testing.T) {
	deployDirs := deployedAdapterDirs()
	if len(deployDirs) == 0 {
		t.Skip("本机没有部署目录（CI 常态），跳过漂移检查")
	}
	for _, dir := range deployDirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			ents, err := os.ReadDir(dir)
			if err != nil {
				t.Skipf("读 %s 失败：%v", dir, err)
			}
			checked := 0
			var diffs []string
			for _, e := range ents {
				if filepath.Ext(e.Name()) != ".lua" {
					continue
				}
				want, err := bundledAdapters.ReadFile("adapters/" + e.Name())
				if err != nil {
					continue // 部署目录里有仓库没有的适配器，跳过
				}
				got, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					continue
				}
				checked++
				if string(got) == string(want) {
					continue
				}
				diffs = append(diffs, fmt.Sprintf("%s（部署 %d / 仓库 %d 字节）",
					e.Name(), len(got), len(want)))
			}
			if checked == 0 {
				t.Skipf("%s 里没有可比对的适配器", dir)
			}
			if len(diffs) > 0 {
				t.Logf("⚠ %d/%d 个适配器与仓库不同：%v", len(diffs), checked, diffs)
				t.Logf("  部署目录不会被新二进制覆盖（writeBundledAdapters 文件已存在即跳过）")
				t.Logf("  ⇒ 仓库的适配器改动在已部署实例上不生效。需要时直接改，或删掉该文件让内核重新解包。")
			} else {
				t.Logf("✓ %d 个适配器与仓库一致", checked)
			}
		})
	}
}

// deployedAdapterDirs 找本机上已部署的适配器目录。
func deployedAdapterDirs() []string {
	var out []string
	for _, root := range []string{"/home", "/var/tmp", "/opt"} {
		ents, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			cand := filepath.Join(root, e.Name(), "adapters")
			if st, err := os.Stat(cand); err == nil && st.IsDir() {
				out = append(out, cand)
			}
		}
	}
	return out
}

// TestAdapterEmitsOnlyKnownFields 验证适配器输出的 tool_call 键都在**契约内**。
//
// 契约 = internal/agent/api/provider.go 里 ToolCall / StreamChunk 的 json tag：
//
//	id, type, name, arguments, raw_arguments, stream_index
//
// ★ 这条才是能自动抓 bug 的判据。漂移检查要看部署状态（环境相关），
//
//	而这条只看"适配器吐出来的键名对不对"——拼错一个字母（streamindex、
//	rawArgs）就会静默失效：Go 侧按 json tag 反序列化，键不匹配 ⇒ 字段取不到
//	⇒ 零值，而**没有任何报错**。
//
//	这正是本次 stream_index 缺失的形态：键名不对时表现和"没写这行"完全一样。
func TestAdapterEmitsOnlyKnownFields(t *testing.T) {
	// 允许的键：ToolCall 的 json tag + StreamChunk 的顶层键
	known := map[string]bool{
		"id": true, "type": true, "name": true,
		"arguments": true, "raw_arguments": true, "stream_index": true,
		// 上游原样透传（部分适配器直接转发 delta.tool_calls）
		"index": true, "function": true,
	}
	chunk := `{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[` +
		`{"index":1,"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]}}]}`

	checked := 0
	for _, name := range bundledAdapterNames(t) {
		vm := NewVM(t.TempDir())
		loadBundled(t, vm, name)
		out, err := vm.CallTransformStreamChunk(name, chunk)
		if err != nil || out == "" {
			continue // 协议不同，不适用该 chunk
		}
		var u struct {
			ToolCalls []map[string]interface{} `json:"tool_calls"`
		}
		if json.Unmarshal([]byte(out), &u) != nil || len(u.ToolCalls) == 0 {
			continue
		}
		checked++
		for i, tc := range u.ToolCalls {
			for k := range tc {
				if !known[k] {
					t.Errorf("%s tool_calls[%d] 输出了契约外的键 %q —— Go 侧按 json tag "+
						"反序列化，键名对不上会**静默取零值**（与没写这行完全一样）\\n  完整：%s",
						name, i, k, out)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("没有任何适配器产出 tool_calls —— 测试前提不成立")
	}
	t.Logf("检查了 %d 个适配器", checked)
}
