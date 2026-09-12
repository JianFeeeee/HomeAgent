package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const (
	binEntry   = "plugin.bin" // 子进程插件（纯 Go 二进制，stdio JSON-RPC + 共享内存）
	luaEntry   = "main.lua"
	skillEntry = "SKILL.md"
	metaEntry  = "plugin.json"
)

// legacyCABIEntries 是已退场的 C ABI 产物名。
//
// 保留这张表只为**给出明确错误**：插件目录里躺着 plugin.so 而内核不再认它时，
// 静默跳过会让「目录在但插件没加载」看起来像配置问题，而实际原因是需要用
// 新版 hmapdev 重编。
var legacyCABIEntries = []string{"plugin.so", "plugin.dll", "plugin.dylib"}

// entryKind 描述插件入口归属的加载通道。
//
// C ABI 通道（.so/.dll/.dylib）已整体退场：外部插件统一走子进程 + stdio RPC，
// 三套独立 ABI 实现收敛为单一 RPC 实现（§9.2）。
type entryKind int

const (
	entryUnknown entryKind = iota
	entryProc              // plugin.bin —— 子进程 + stdio JSON-RPC + 共享内存
	entryLua               // main.lua
	entrySkill             // SKILL.md
)

func (k entryKind) String() string {
	switch k {
	case entryProc:
		return "proc"
	case entryLua:
		return "lua"
	case entrySkill:
		return "skill"
	}
	return "unknown"
}

// classifyEntry 把 manifest 的 entry 字段映射到加载通道。
//
// entry 为空或声明已退场的 C ABI 产物时返回 entryUnknown，
// 由调用方回退到目录探测（兼容无 manifest 的旧插件），
// 并在探测到 C ABI 残留时给出明确的重编提示。
func classifyEntry(entry string) entryKind {
	switch entry {
	case binEntry:
		return entryProc
	case luaEntry:
		return entryLua
	case skillEntry:
		return entrySkill
	}
	return entryUnknown
}

// detectEntryKind 先读 manifest 的 entry，读不到则按目录内存在的入口文件推断。
//
// 注意：存量插件的 plugin.json 可能仍写着 "plugin.so"（工具链已不再据此分派，
// 但历史产物里有），此时 classifyEntry 返回 unknown，靠目录探测找到 plugin.bin。
func detectEntryKind(plgDir string) entryKind {
	if mft := readManifest(plgDir); mft != nil {
		if k := classifyEntry(mft.Entry); k != entryUnknown {
			return k
		}
	}
	for _, probe := range []struct {
		file string
		kind entryKind
	}{
		{binEntry, entryProc},
		{luaEntry, entryLua},
		{skillEntry, entrySkill},
	} {
		if st, err := os.Stat(filepath.Join(plgDir, probe.file)); err == nil && !st.IsDir() {
			return probe.kind
		}
	}
	return entryUnknown
}

// hasLegacyCABIEntry 判断插件目录里是否只剩已退场的 C ABI 产物。
//
// 用于给出「需要重编」而非「插件不存在」的错误。
func hasLegacyCABIEntry(plgDir string) bool {
	for _, name := range legacyCABIEntries {
		if st, err := os.Stat(filepath.Join(plgDir, name)); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

func readManifest(dir string) *PluginManifest {
	data, err := os.ReadFile(filepath.Join(dir, metaEntry))
	if err != nil {
		return nil
	}
	var m PluginManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return &m
}
