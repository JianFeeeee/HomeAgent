//go:build !cgo

package api

// codec_nocgo.go —— CGO_ENABLED=0 时把 C 路径的符号指向纯 Go 实现。
//
// 为什么需要这层转发而不是直接调 *Pure：让 codec.go 无论编译开关如何都能引用
// 同一组符号名，避免调用方到处写 build tag 分支。
//
// 谁会走到这里（CGO_ENABLED=0）：
//   - waiter 等刻意 CGO-free 的跨平台目标（Makefile build-cli）
//   - 交叉编译到无 cgo 工具链的场景
//
// homed 不会走到这里——它强制 cgo（sqlite3 + gojieba）。
// 两条路径的语义等价由 codec_golden_test.go 钉死，Makefile 的
// check-codec-paths 目标同时跑两条。

func modelContextWindowC(model string) int { return modelContextWindowPure(model) }

func estimateTokensC(text string) int { return estimateTokensPure(text) }

func truncateByTokensC(s string, maxTokens int) string { return truncateByTokensPure(s, maxTokens) }
