//go:build linux || darwin

package plugins

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// manifest 声明的 capabilities 在真实内核加载路径上生效（Part 6.4）。
//
// capability_test.go 在 proc 包内验证判定逻辑；这里验证**接线**：
// manifest → readManifest → proc.New(caps...) → coreHandler.Handle 的强制。

// installPluginWithCaps 装插件并写入指定 capabilities 声明。
func installPluginWithCaps(t *testing.T, plgDir, name string, caps []string) {
	t.Helper()
	src := realPluginBinary(t, name)

	dst := filepath.Join(plgDir, name)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("建插件目录: %v", err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读产物: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dst, "plugin.bin"), data, 0o755); err != nil {
		t.Fatalf("写产物: %v", err)
	}

	capsJSON := ""
	if caps != nil {
		quoted := make([]string, len(caps))
		for i, c := range caps {
			quoted[i] = fmt.Sprintf("%q", c)
		}
		capsJSON = fmt.Sprintf(`,"capabilities":[%s]`, strings.Join(quoted, ","))
	}
	manifest := fmt.Sprintf(
		`{"name":%q,"name_zh":%q,"name_en":%q,"version":"1.0.0","entry":"plugin.so"%s}`,
		name, name, name, capsJSON)
	if err := os.WriteFile(filepath.Join(dst, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("写 manifest: %v", err)
	}
}

// 声明了受限能力集的插件仍能正常加载并注册工具。
//
// core 能力（注册工具/阶段/通道 + 读写自己的配置）无需声明，
// 否则插件根本无法启动——weather 在 Start 里就要读 Settings。
func TestCapability_RestrictedPluginStillLoads(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	plgDir := filepath.Join(env.tmpDir, "plugins")
	// 只声明 io：weather 用到的 Settings 属 core，应放行
	installPluginWithCaps(t, plgDir, "weather", []string{"io"})

	if err := env.pluginReg.Load(plgDir); err != nil {
		t.Fatalf("加载插件: %v", err)
	}
	if env.pluginReg.Get("weather") == nil {
		t.Fatal("声明受限能力后插件应仍能加载（core 能力无需声明）")
	}

	// 工具注册也属 core
	found := false
	for _, def := range env.stageHost.GetToolDefs() {
		if strings.Contains(def.Name, "weather") {
			found = true
			break
		}
	}
	if !found {
		t.Error("受限插件仍应能注册工具（tool.register 属 core）")
	}
}

// 未声明 capabilities 的插件不受限——存量插件向后兼容。
//
// 17 个存量插件的 plugin.json 都没有这个字段。若空声明当作最小权限，
// 它们会静默失去 IO 注入/记忆读写等能力，违反「外部插件零改动」。
func TestCapability_LegacyManifestUnrestricted(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	plgDir := filepath.Join(env.tmpDir, "plugins")
	installPluginWithCaps(t, plgDir, "weather", nil) // 无 capabilities 字段

	if err := env.pluginReg.Load(plgDir); err != nil {
		t.Fatalf("加载插件: %v", err)
	}
	if env.pluginReg.Get("weather") == nil {
		t.Fatal("未声明 capabilities 的存量插件必须能正常加载")
	}
}
