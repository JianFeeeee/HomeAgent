//go:build cgo

package api

// codec_cgo.go —— 编解码层的 C 实现绑定（CGO_ENABLED=1 时参与编译）。
//
// ============================ 架构：包内符号链接 ============================
// C 源是 `ha_codec.c` / `ha_codec.h`，它们是**指向 csrc/ 的符号链接**
// （`ln -s ../../../csrc/src/ha_codec.c`）：
//
//	internal/agent/api/ha_codec.c -> ../../../csrc/src/ha_codec.c
//	internal/agent/api/ha_codec.h -> ../../../csrc/include/ha_codec.h
//
// 权威源只有一份（csrc/），Go 侧看到的是包目录内的链接。
//
// ============================ 为什么不用另外两种做法 ============================
//
// **不能链接预构建静态库**（`LDFLAGS: .../csrc/build/libha_codec.a`）：
//   - .a 是构建产物、不入库（.gitignore 的 build/ 命中 csrc/build/），
//     而发布脚本原先并不产出它 ⇒「不入库 + 不生成」两头空，链接必然失败
//     （实测：cannot find csrc/build/libha_codec.a）
//   - 交叉编译 linux/arm64（homed 的真实发布目标）时，宿主 x86-64 的 .a
//     被链进目标产物，报 `file in wrong format`（实测）。
//
// **不能用 `#include "../../../csrc/src/ha_codec.c"`（包外相对包含）**：
//   ★ Go 构建缓存**不跟踪包外被 #include 的 C 文件**。实测：在包外源里把
//   返回值从 7 改成 8，`go test` 依然通过（缓存命中，静默沿用旧代码）；
//   而同样改动落在包内文件时立刻判红。这对「逐步推进 C 化」是致命的——
//   改 C 源码却不生效，且无任何报错。
//   （包内 shim `#include` 包外源同样漏跟踪，已实测排除。）
//
// 包内符号链接同时满足两点：文件在包目录内 ⇒ 缓存按内容正确跟踪；
// 只有一份权威源 ⇒ 无副本漂移，也不需要「同步 C 源」的 make 目标。
//
// ============================ 为什么不需要额外 build tag ============================
// 与 onnxruntime（internal/nlp/onnx.go，需运行期 libonnxruntime.so）不同：
// ha_codec 是**零依赖纯 C99 源码内联编译**，不需要任何外部库或工具链前提。
// 而 homed 本就强制 cgo（mattn/go-sqlite3 + gojieba），故 C 路径自然生效。
// 因此只用 `cgo` / `!cgo` 一组约束，不引入 hacodec tag。
//
// ============================ C 侧契约 ============================
// `#include "ha_codec.h"` 只声明原型；实现在同包的 ha_codec.c，由 cgo 自动编译。
// 只含 libc 头，不引入第三方符号。
//
// 语义必须与 codec_pure.go 逐值等价，由 codec_golden_test.go 钉死。

/*
#cgo CFLAGS: -std=c99
#include <stdlib.h>
#include "ha_codec.h"
*/
import "C"

import "unsafe"

// modelContextWindowC 经 C 实现推断上下文窗口。
func modelContextWindowC(model string) int {
	cModel := C.CString(model)
	defer C.free(unsafe.Pointer(cModel))
	return int(C.ha_codec_model_context_window(cModel))
}

// estimateTokensC 经 C 实现估算 token 数。
func estimateTokensC(text string) int {
	cText := C.CString(text)
	defer C.free(unsafe.Pointer(cText))
	return int(C.ha_codec_estimate_tokens(cText))
}

// truncateByTokensC 经 C 实现按 token 截断。
//
// 缓冲区策略：按 rune 数上界分配（每个 rune 最多 4 字节）+ 1 字节 NUL，
// 保证 C 侧不会因容量不足而截短——否则 C 与 Go 的逐值对照会假失败。
// 若字符串无 rune（纯 ASCII 也至少 len 字节），取 len(text)+1 兜底。
func truncateByTokensC(s string, maxTokens int) string {
	if maxTokens <= 0 || s == "" {
		return ""
	}
	// []rune 的长度即 rune 数；每个 rune 最坏 4 字节，+1 给 NUL。
	runeCount := len([]rune(s))
	bufSize := runeCount*4 + 1
	if bufSize < len(s)+1 {
		bufSize = len(s) + 1
	}
	buf := (*C.char)(C.malloc(C.size_t(bufSize)))
	if buf == nil {
		// 分配失败：回退纯 Go 实现，不让整个调用失败。
		return truncateByTokensPure(s, maxTokens)
	}
	defer C.free(unsafe.Pointer(buf))

	cText := C.CString(s)
	defer C.free(unsafe.Pointer(cText))

	n := C.ha_codec_truncate_by_tokens(cText, C.int(maxTokens), buf, C.size_t(bufSize))
	return C.GoStringN(buf, C.int(n))
}
