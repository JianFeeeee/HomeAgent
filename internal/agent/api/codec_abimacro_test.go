//go:build cgo

package api

// codec_abimacro_test.go —— 堵住「Go 常量与 C 函数一起错成一样」的盲区。
//
// TestABIVersionMatches 比的是「Go 常量 vs C 函数返回值」。若有人同时把
// Go 常量和 C 函数一起改成 2（而忘了改 ha_abi.h 的宏），那条测试照样通过
// —— **两边一起错成一样**是它的盲区。本测试直接问 C 侧宏。
//
// （C 侧宏的取法在 codec_cgo.go：Go 不允许在 _test.go 里用 cgo，
//   故 const 桥接只能写在非测试文件。）

import "testing"

func TestABIMacroMatchesRuntimeAndGo(t *testing.T) {
	macro := codecABIVersionMacroValue()
	runtime := codecABIVersion()

	if macro != runtime {
		t.Fatalf("C 侧宏与运行期值不一致：\n"+
			"  HA_CODEC_ABI_VERSION（宏展开）= %d\n"+
			"  ha_codec_abi_version()          = %d\n"+
			"  说明：改了 ha_abi.h 的宏但没同步改 ha_codec_abi_version()，或反之。",
			macro, runtime)
	}

	wantGo := codecABIMajorExpected*1000 + codecABIMinorExpected
	if macro != wantGo {
		t.Fatalf("C 侧宏与 Go 侧常量不一致：\n"+
			"  C 宏 HA_CODEC_ABI_VERSION = %d (major=%d minor=%d)\n"+
			"  Go 常量期望               = %d (major=%d minor=%d)\n"+
			"  改法：同步更新 ha_abi.h 与 codec_cgo.go 的 codecABIMajor/MinorExpected。",
			macro, macro/1000, macro%1000,
			wantGo, wantGo/1000, wantGo%1000)
	}
}
