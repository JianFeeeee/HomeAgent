package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 设备命令白名单的配置化。
//
// ## 为什么要改
//
// 白名单原本是源码里硬编码的正则（cmd/waiter/device.go 的
// homeagentAllowCmd，18 个命令：ls/pwd/cat/df/...）。它的后果是：
// **无论怎么配 waiter.yaml 都跑不了 find / grep / sed / sort / tr**，
// 而这些正是排查问题最常用的只读命令。生产日志里的实际报错：
//
//	device_ctl_cmdrun  device_id:waiter-fnnas  error: command not in whitelist
//
// 而 waiter.yaml 里当时只有 3 个键（device_gateway / device_token /
// device_authorized），**没有任何键能改这个白名单**。
//
// ## 改动
//
// 白名单从"编译期常量"变成"运行期配置"：waiter.yaml 可加
// `device_cmd_allowlist:`（字符串数组，留空则用内置默认集）。
// 匹配函数本身是包级变量，由 main 从配置赋值 —— 与同文件既有的
// sendBridgeResult 同一模式（那里也是包级函数变量）。
//
// ## 为什么要留默认集
//
// 配置缺失/写错时**不能变成"全放行"**：那等于静默拆掉这道闸。
// 判定顺序是「配置非空 → 用配置；否则 → 用默认集」，任一分支都仍有闸。

// TestDefaultAllowlistStillBlocksDestructive 门禁：默认集必须挡住破坏性命令。
//
// 这是这道闸存在的**唯一理由**。若某天有人把默认集改成"什么都不拦"，
// 这条判据必须失败。
func TestDefaultAllowlistStillBlocksDestructive(t *testing.T) {
	// 明确危险的：写文件、删文件、改权限、任意解释器
	for _, cmd := range []string{
		"rm -rf /",
		"dd if=/dev/zero of=/dev/sda",
		"chmod -R 777 /",
		"mkfs.ext4 /dev/sda1",
		"shutdown now",
		"reboot",
		":(){ :|:& };:", // fork 炸弹
	} {
		if defaultCmdAllowed(cmd) {
			t.Errorf("默认白名单放过了破坏性命令 %q —— 这道闸的唯一作用就是挡它", cmd)
		}
	}
	// 常规运维命令应当放行
	for _, cmd := range []string{"ls", "pwd", "uname -a", "df -h", "ps aux", "uptime"} {
		if !defaultCmdAllowed(cmd) {
			t.Errorf("默认白名单挡住了常规命令 %q —— 默认集被改窄了", cmd)
		}
	}
}

// TestConfigAllowlistExtends 判：配置可扩展只读分析命令。
func TestConfigAllowlistExtends(t *testing.T) {
	// 场景：配置里加了 find/grep/sed/sort/tr
	cfg := []string{"ls", "find", "grep", "sed", "sort", "tr"}
	save := deviceCmdAllowed
	defer func() { deviceCmdAllowed = save }()

	deviceCmdAllowed = buildCmdMatcher(cfg)
	for _, cmd := range []string{
		"find . -name plugin.go", // 这次的核心诉求
		"grep -rn authorized .",
		"sed -n 1,20p file",
		"sort -u list",
		"tr a-z A-Z",
	} {
		if !cmdAllowed(cmd) {
			t.Errorf("配置里已声明的命令仍被拒: %q", cmd)
		}
	}
	// 配置里没写的仍应被拒（配置是"替换默认集"而非"追加"）
	if cmdAllowed("rm -rf /") {
		t.Error("配置未包含 rm 却放行了 —— 配置必须替换而非叠加默认集")
	}
}

// TestEmptyConfigFallsBackToDefault 判：配置缺失时回退默认集，且**不是**全放行。
func TestEmptyConfigFallsBackToDefault(t *testing.T) {
	save := deviceCmdAllowed
	defer func() { deviceCmdAllowed = save }()

	deviceCmdAllowed = buildCmdMatcher(nil) // 配置为空
	if !cmdAllowed("ls") {
		t.Error("配置为空时连 ls 都不放行 —— 回退逻辑坏了")
	}
	if cmdAllowed("rm -rf /") {
		t.Error("配置为空时放行了 rm -rf —— 空配置绝不能等于全放行")
	}
}

// TestCmdAllowlistFromYAML 判：waiter.yaml 的 device_cmd_allowlist 真能读出来。
func TestCmdAllowlistFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "waiter.yaml")
	body := "device_gateway: \"ws://127.0.0.1:9890/api/v1/device/ws\"\n" +
		"device_authorized: true\n" +
		"device_cmd_allowlist:\n  - ls\n  - find\n  - grep\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	// 走**真实**加载路径（readFile），不另写一份解析 ——
	// 两处会漂移，而漂移本身就是漏洞。
	cfg := readFile(path)
	if cfg == nil {
		t.Fatalf("readFile 读不出配置: %s", path)
	}
	if len(cfg.DeviceCmdAllowlist) != 3 {
		t.Fatalf("device_cmd_allowlist 解析出 %d 项，期望 3: %v",
			len(cfg.DeviceCmdAllowlist), cfg.DeviceCmdAllowlist)
	}
	joined := strings.Join(cfg.DeviceCmdAllowlist, ",")
	for _, want := range []string{"ls", "find", "grep"} {
		if !strings.Contains(joined, want) {
			t.Errorf("device_cmd_allowlist 缺 %q：%v", want, cfg.DeviceCmdAllowlist)
		}
	}
}

// TestDaemonPathAppliesAllowlist 守住「daemon 模式也必须应用配置」。
//
// ★ 这条判据来自一次真实的疏漏。
//
// waiter 有两条设备桥启动路径：
//
//	· main.go 的 startDeviceBridge      —— 交互/一次性模式
//	· daemon.go 的 startDaemonDeviceBridge —— `waiter --daemon`（生产两台都这么跑）
//
// 我最初只在 main.go 里赋值 deviceCmdAllowed。daemon 路径不经过那里，
// 于是配置**完全不生效** —— 而症状是"配置写了、启动也打了招呼、命令照样被拒"，
// 极难定位（看起来像配置没读到，其实是那条路径没接线）。
//
// startDaemonDeviceBridge 会起 goroutine 连网关，测试里不能真连；
// 所以这里验证它**读了** cfg.DeviceCmdAllowlist 并改了包级匹配函数：
// 先让它在缺网关地址时提前返回，确认那条路径的判据逻辑。
func TestDaemonPathAppliesAllowlist(t *testing.T) {
	save := deviceCmdAllowed
	defer func() { deviceCmdAllowed = save }()

	// 先设成"拒绝一切"，若 daemon 路径没有应用配置，它会保持不变
	deviceCmdAllowed = func(string) bool { return false }

	// 缺 device_gateway ⇒ 提前 return，不会走到白名单赋值。
	// 这条断言锁住"提前返回"是有意为之（无网关就不该起桥）。
	cfg := &Config{DeviceToken: "t"}
	startDaemonDeviceBridge(cfg)
	if cmdAllowed("find .") {
		t.Error("无网关时 startDaemonDeviceBridge 不应改动白名单")
	}

	// ★ 关键：把网关路径走到赋值那一步。
	// 真实函数会在 dg==""||dt=="" 时返回，所以这里必须给出网关地址；
	// 而它随后会起 goroutine 连真实网关 —— 用一个不可达地址即可，
	// goroutine 连不上会自行退出，不影响本断言。
	cfg2 := &Config{
		DeviceGateway:      "ws://127.0.0.1:1/api/v1/device/ws", // 不可达
		DeviceToken:        "t",
		DeviceCmdAllowlist: []string{"find", "grep"},
	}
	startDaemonDeviceBridge(cfg2)
	// 赋值发生在 goroutine 之前 ⇒ 同步可见
	if !cmdAllowed("find . -name x") {
		t.Error("daemon 路径没有应用 waiter.yaml 的 device_cmd_allowlist —— " +
			"配置在 `waiter --daemon` 下会完全不生效")
	}
	if cmdAllowed("rm -rf /") {
		t.Error("daemon 路径应用配置后仍放行破坏性命令")
	}
}
