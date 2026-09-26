//go:build cgo

package api

// codec_abiversion_test.go —— Go 侧与 C 侧 ABI 版本必须对得上。
//
// ============================ 为什么这是必需的 ============================
// ha_codec.h 声明「签名一经发布即冻结」，但注释不参与编译 —— 两侧对
// 「我以为的版本」不一致时，没有任何机制会报错。典型事故：
//   某人在 C 侧给 openAIToolCall 之类的结构体加了字段并把 MAJOR 提到 2，
//   Go 侧没改 —— 产出的二进制「看起来能跑」，但字段错位，
//   表现为插件行为诡异 / 记忆内容错乱，极难定位。
//
// 这条测试把「两侧版本一致」变成**会失败的事实**。
//
// 判定：Go 侧常量（codecABIMajorExpected / codecABIMinorExpected）必须等于
// C 侧 ha_codec_abi_version() 运行期返回值，也必须等于 C 侧宏展开值
// （后者由 codec_abi_macro_test.go 单独验证，避免「两边都错成一样」）。

import "testing"

func TestABIVersionMatches(t *testing.T) {
	got := codecABIVersion()
	want := codecABIMajorExpected*1000 + codecABIMinorExpected

	if got != want {
		t.Fatalf("ABI 版本不一致：\n"+
			"  C 侧 ha_codec_abi_version() = %d (major=%d minor=%d)\n"+
			"  Go 侧 codecABIVersion 期望   = %d (major=%d minor=%d)\n"+
			"  改法：若 C 侧新增了函数/字段（纯追加），把本文件两个常量各 +1；\n"+
			"        若改了签名/删了函数/改了结构体布局，那是 MAJOR 变更，\n"+
			"        所有调用方必须同步重编，不能只改版本号。",
			got, got/1000, got%1000, want, want/1000, want%1000)
	}
}

// TestABIVersionSane 防止「两边一起写成荒谬值」也能通过上面的测试。
func TestABIVersionSane(t *testing.T) {
	got := codecABIVersion()
	if got < 1000 || got > 99999 {
		t.Fatalf("ABI 版本荒谬：%d（major 必须在 1-9，minor 必须在 0-99）", got)
	}
	if codecABIMajorExpected < 1 || codecABIMajorExpected > 9 {
		t.Errorf("Go 侧 major 常量越界：%d", codecABIMajorExpected)
	}
	if codecABIMinorExpected < 0 || codecABIMinorExpected > 99 {
		t.Errorf("Go 侧 minor 常量越界：%d", codecABIMinorExpected)
	}
}
