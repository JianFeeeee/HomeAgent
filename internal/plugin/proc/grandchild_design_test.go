package proc

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 三个测试设计缺陷的判据。全部从**源码文本**提取 —— 这里要防的是
// 「判据与源码漂移」，而这几个缺陷恰恰是漂移造成的。
//
// ## 缺陷 1：名字说 Survives，实际测的是「被杀」
//
//	TestKillReturnsEvenWhenGrandchildSurvives   spawn grandchildPluginSource
//	                                           → sleep 400，**不**设 Setpgid
//	                                           → 留在插件进程组内
//	                                           → kill(-pgid) **能**杀掉它
//
//	grandchildPluginSource 的注释自己写着：
//	    「孙进程**不**设 Setpgid：它要留在插件的进程组里，才代表真实场景」
//
//	所以这个测试里孙进程**不会活下来**，与名字里的 Survives 相反。
//	真正测脱组（孙进程活下来）的是
//	TestKillReturnsWhenGrandchildEscapesProcessGroup，它用
//	escapingGrandchildSource（sleep 401 + Setsid: true）。
//
//	⇒ 两个测试不是「一个多余」，而是**名字与语义对不上**。
//	   保留两个可以，但名字必须说清各自测什么。
//
// ## 缺陷 2：判据数的是**全系统**进程数
//
//	countShimGrandchildren() 扫 /proc 找 "sleep 400"，
//	countEscapingGrandchildren() 扫 "sleep 401"。
//	两者都不是「只数自己拉起的」⇒
//	同一台机器上任何其它进程/测试/容器命中同样的 cmdline 就会串味。
//	注释里已经记过一次前车之鉴（「我第一版就踩了」），但只修了 base
//	快照，**没解决全局匹配**这个根因。
//
// ## 缺陷 3：defer 清理依赖 pluginPid，失败就跳过
//
//	gc4 的 defer：
//	    if pid := pluginPid(p); pid > 0 { syscall.Kill(-pid, SIGKILL) }
//	若 pluginPid 拿不到 pid（进程已退出 / 时序未到），清理**整个跳过**
//	⇒ 残留进程污染后续测试。
//
// 运行：go test ./internal/plugin/proc/ -run TestGrandchildTestDesign -v

const testFile = "grandchild_test.go"

func srcText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("读 %s: %v", testFile, err)
	}
	return string(b)
}

func TestGrandchildTestDesign_SurvivesTestUsesEscapingSource(t *testing.T) {
	src := srcText(t)

	// ★ 判据只问一件事：**那个用普通源的测试，名字是否还说「Survives」**。
	//
	// 不去解函数体、不去比对 spawn 参数 —— 那些都会随重构变，
	// 而「名字 vs 语义」这层矛盾才是真正要防的回归。
	//
	// 修复前：func TestKillReturnsEvenWhenGrandchildSurvives → 用普通源 ⇒ 红
	// 修复后：改名 DiesWithProcessGroup ⇒ 绿
	const oldName = "func TestKillReturnsEvenWhenGrandchildSurvives("
	const newName = "func TestKillReturnsWhenGrandchildDiesWithProcessGroup("

	hasOld := strings.Contains(src, oldName)
	hasNew := strings.Contains(src, newName)

	switch {
	case hasOld && hasNew:
		t.Errorf("两个名字同时存在（%s 与 %s）：改名没删干净，go vet 也会报重定义",
			oldName, newName)
	case hasOld:
		// ★ 旧名还在：它 spawn 的是 grandchildPluginSource（sleep 400、
		//   **不**设 Setpgid、留在插件进程组内 ⇒ kill(-pgid) **能**杀掉它）
		//   ⇒ 孙进程不会「Survive」，与名字矛盾。
		//   真正测脱组存活的是 TestKillReturnsWhenGrandchildEscapesProcessGroup
		//   （escapingGrandchildSource：sleep 401 + Setsid）。
		t.Errorf("TestKillReturnsEvenWhenGrandchildSurvives 用了普通源 " +
			"grandchildPluginSource（sleep 400、**不**设 Setpgid、留在进程组内、" +
			"kill(-pgid) **能**杀掉它）⇒ 孙进程不会「Survive」，与测试名矛盾。\n" +
			"  真正测脱组存活的是 TestKillReturnsWhenGrandchildEscapesProcessGroup" +
			"（escapingGrandchildSource：sleep 401 + Setsid）。\n" +
			"  修法：改名成 …WhenGrandchildDiesWithProcessGroup（名字与实现一致），" +
			"或改用 escaping 源。")
	}
}

func TestGrandchildTestDesign_CountersAreGlobalNotOwn(t *testing.T) {
	src := srcText(t)

	for _, c := range []struct{ fn, marker string }{
		// 认新旧两个名字：修复后新增了带 ppid 限定的 …Under 变体
		{"countShimGrandchildrenUnder", `"sleep 400"`},
		{"countEscapingGrandchildren", `"sleep 401"`},
	} {
		i := strings.Index(src, "func "+c.fn+"(")
		if i < 0 {
			t.Errorf("找不到 %s", c.fn)
			continue
		}
		j := strings.Index(src[i:], "\n}\n")
		if j < 0 {
			continue
		}
		body := src[i : i+j]
		// 扫全系统 /proc 而不看父子关系 ⇒ 会数到别人的进程。
		// 修复形态：函数体里有 procPPid(pid) 限定（或名字带 Under）。
		hasPPidGate := strings.Contains(body, "procPPid(") ||
			strings.Contains(body, "PPid")
		if strings.Contains(body, "os.ReadDir(\"/proc\")") && !hasPPidGate {
			t.Errorf("%s 扫全系统 /proc 找 %s，不区分父子关系。\n"+
				"  同机任何命中同样 cmdline 的进程/容器都会串味，表现为"+
				"「合跑红、单跑绿」。\n"+
				"  修法：按 PPid 限定为**自己拉起的那几个**，或让插件把自己的孙进程 pid 报上来。",
				c.fn, c.marker)
		}
	}
}

func TestGrandchildTestDesign_CleanupSkippedWhenPidMissing(t *testing.T) {
	src := srcText(t)

	i := strings.Index(src, "func TestKillReturnsWhenGrandchildDiesWithProcessGroup(")
	if i < 0 {
		i = strings.Index(src, "func TestKillReturnsEvenWhenGrandchildSurvives(")
	}
	if i < 0 {
		t.Fatal("找不到该测试（新旧名都试过）")
	}
	j := strings.Index(src[i:], "\n}\n")
	body := src[i : i+j]

	// defer 里 `if pid := pluginPid(p); pid > 0 { Kill }` ⇒ 拿不到 pid 就整个跳过。
	// 修复形态：body 里出现 cleanupSleepMarkers（兜底按 cmdline 清理）。
	if strings.Contains(body, "cleanupSleepMarkers(") {
		return
	}
	if strings.Contains(body, "pid > 0") &&
		!strings.Contains(body, "else") && !strings.Contains(body, "fallback") {
		t.Errorf("defer 清理是「pluginPid(p) > 0 才杀」，pid 拿不到就**整个跳过**清理" +
			"⇒ 残留进程污染后续测试。\n" +
			"  修法：pid 拿不到时也要兜底（如按唯一 cmdline 标记清理），" +
			"或让插件启动时把孙进程 pid 报给宿主。")
	}
}

// TestGrandchildTestDesign_CountersHaveSeparateNamespaces 记一条事实，
// 免得以后有人以为两个计数器是同一个。
func TestGrandchildTestDesign_CountersHaveSeparateNamespaces(t *testing.T) {
	src := srcText(t)
	if !strings.Contains(src, `"sleep 400"`) || !strings.Contains(src, `"sleep 401"`) {
		t.Fatal("两个计数器的 sleep 标记应当不同（400 / 401），否则会互相数进去")
	}
	_ = filepath.Join // 保持 import 有用（若上面某条判据被删也不至于编译失败）
	_ = strconv.Itoa
}
