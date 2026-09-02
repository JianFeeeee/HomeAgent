package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const (
	soEntry    = "plugin.so"
	dllEntry   = "plugin.dll"
	binEntry   = "plugin.bin" // 子进程插件（纯 Go 二进制，stdio JSON-RPC）
	luaEntry   = "main.lua"
	skillEntry = "SKILL.md"
	metaEntry  = "plugin.json"
)

// entryKind 描述插件入口归属的加载通道。
// 外部插件多进程化期间 .so/.dll（cabi）与 .bin（proc）**双通道共存**，
// 按 plugin.json 的 entry 字段分派，使迁移可逐插件推进、随时回退。
type entryKind int

const (
	entryUnknown entryKind = iota
	entryCABI              // plugin.so / plugin.dll / plugin.dylib —— C ABI 动态库
	entryProc              // plugin.bin —— 子进程 + stdio JSON-RPC
	entryLua               // main.lua
	entrySkill             // SKILL.md
)

func (k entryKind) String() string {
	switch k {
	case entryCABI:
		return "cabi"
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
// entry 为空时返回 entryUnknown，由调用方回退到目录探测（兼容无 manifest 的旧插件）。
func classifyEntry(entry string) entryKind {
	switch entry {
	case soEntry, dllEntry, "plugin.dylib":
		return entryCABI
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
// 推断顺序：.bin 优先于 .so——迁移期间同一插件目录可能两个产物共存（升级未清理），
// 此时应走新通道；manifest 显式声明优先级最高。
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
		{soEntry, entryCABI},
		{"plugin.dylib", entryCABI},
		{dllEntry, entryCABI},
		{luaEntry, entryLua},
		{skillEntry, entrySkill},
	} {
		if st, err := os.Stat(filepath.Join(plgDir, probe.file)); err == nil && !st.IsDir() {
			return probe.kind
		}
	}
	return entryUnknown
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

var _ = json.Marshal
