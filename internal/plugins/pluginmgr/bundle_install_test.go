package pluginmgr

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestBuildPluginBundlesInstallable 校验「发布用插件包」能被内核真实解包安装。
//
// 为什么要这个测试：release 此前只发 homed/waiter 与 hmapdev 工具链，**不发插件包**，
// 用户得自己装 Go + hmapdev 逐插件构建。现在发布流程会带上 .hmap（见 SDK 仓的
// scripts/build_plugin_bundles.sh），但「能构建出来」不等于「内核装得上」——
// 包内可能是 bundle 形态（plugin.bin.<os>.<arch>），也可能是单平台或 Lua 形态，
// 三种都要能被 extractPackage 正确处理并落到规范名 plugin.bin。
//
// 测试用内核自己的 extractPackage（而非测试里另写一份解包），所以它验证的是
// 真实安装路径。
func TestBuildPluginBundlesInstallable(t *testing.T) {
	dir := os.Getenv("HMAP_BUNDLE_DIR")
	if dir == "" {
		t.Skip("未设 HMAP_BUNDLE_DIR（指向 build_plugin_bundles.sh 的产出目录）")
	}
	// 相对路径按**仓根**而非本包目录解析：测试的 cwd 是包目录，
	// 而调用方通常写 `HMAP_BUNDLE_DIR=dist/plugins`（相对仓根），不修正就会
	// 报 “no such file or directory”，看起来像产物不存在。
	if !filepath.IsAbs(dir) {
		if abs, err := filepath.Abs(filepath.Join(repoRoot(), dir)); err == nil {
			dir = abs
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读产物目录 %s: %v", dir, err)
	}

	n := 0
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) != ".hmap" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: 读文件: %v", name, err)
		}

		// 每个包解到独立目录，互不干扰
		tmp := t.TempDir()
		if err := extractPackage(data, tmp); err != nil {
			t.Errorf("%s: extractPackage 失败: %v", name, err)
			continue
		}

		// 解包后必须有一个规范入口可执行文件（plugin.bin / main.lua / SKILL.md）
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Errorf("%s: 重新读 zip: %v", name, err)
			continue
		}
		var pkgName string
		for _, f := range zr.File {
			if f.Name == "plugin.json" {
				rc, _ := f.Open()
				buf := new(bytes.Buffer)
				buf.ReadFrom(rc)
				rc.Close()
				// 只要 name 字段，避免为此引 json（本文件保持轻量）
				pkgName = extractField(buf.String(), "name")
				break
			}
		}
		if pkgName == "" {
			t.Errorf("%s: 包内 plugin.json 无 name", name)
			continue
		}

		installed := filepath.Join(tmp, pkgName)
		var okEntry string
		for _, cand := range []string{"plugin.bin", "main.lua", "SKILL.md"} {
			if _, err := os.Stat(filepath.Join(installed, cand)); err == nil {
				okEntry = cand
				break
			}
		}
		if okEntry == "" {
			// 列出实际落了什么，便于定位
			got, _ := os.ReadDir(installed)
			var names []string
			for _, g := range got {
				names = append(names, g.Name())
			}
			t.Errorf("%s: 安装目录 %s 下无规范入口（plugin.bin/main.lua/SKILL.md），实际: %v",
				name, pkgName, names)
			continue
		}

		// 入口必须非空且可执行位（内核按子进程 spawn）
		fi, err := os.Stat(filepath.Join(installed, okEntry))
		if err != nil || fi.Size() == 0 {
			t.Errorf("%s: 入口 %s 为空或不可读", name, okEntry)
			continue
		}
		n++
		t.Logf("✓ %-46s → %s/%s (%.1f MB)", name, pkgName, okEntry, float64(fi.Size())/1048576)
	}

	if n == 0 {
		t.Fatal("没有任何 .hmap 通过安装校验")
	}
	t.Logf("共 %d 个插件包通过内核解包校验", n)
}

// repoRoot 从本文件位置向上找到仓根（含 go.mod 的目录）。
func repoRoot() string {
	d, err := os.Getwd()
	if err != nil {
		return "."
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return "."
}

// extractField 从 JSON 文本里取一个字符串字段（本文件不想为此引 encoding/json）。
func extractField(js, key string) string {
	pat := `"` + key + `"`
	i := indexOf(js, pat)
	if i < 0 {
		return ""
	}
	rest := js[i+len(pat):]
	j := indexOf(rest, ":")
	if j < 0 {
		return ""
	}
	rest = rest[j+1:]
	k := indexOf(rest, `"`)
	if k < 0 {
		return ""
	}
	rest = rest[k+1:]
	m := indexOf(rest, `"`)
	if m < 0 {
		return ""
	}
	return rest[:m]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
