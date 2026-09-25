package api

// codec_bench_test.go —— 编解码层的跨语言开销基线。
//
// 存在的理由：`codec.go` 的 EstimateTokens 注释写着「是否该留在 C 侧由
// codec_bench_test.go 的实测数据决定，不要凭直觉断言」。本文件就是那份数据。
//
// ============================ 为什么必须有 ============================
// C 化不是免费的：每次调用要走 cgo 边界（~50-100ns 固定开销）+ C.CString
// 分配/释放（O(n) 拷贝）。对**高频热路径**（上下文裁剪对每个事件都调），
// 短文本上这笔开销可能超过 C 实现省下的算术时间。
//
// 因此判据不是「C 比 Go 快」，而是「在真实输入分布下 C 是否更快」。
// 本基准跑 cgo 下的 EstimateTokens（走 C）与直调纯 Go 实现，给出分界点。
//
// 运行：go test -run XXX -bench BenchmarkEstimate -benchmem ./internal/agent/api/
// 注意：CGO_ENABLED=0 时 cgo 与纯 Go 是同一实现，对比无意义（差异应为 0）。

import (
	"strings"
	"testing"
)

// benchInputs 覆盖真实分布：短中文（裁剪查询）、长文本（预算计算）、ASCII。
var benchInputs = map[string]string{
	"empty":       "",
	"ascii_short": "hello world",
	"zh_short":    "用户询问了系统状态",
	"zh_200":      strings.Repeat("这是一段中文文本。", 20),
	"ascii_1k":    strings.Repeat("x", 1024),
	"zh_1k":       strings.Repeat("中", 1024),
}

// BenchmarkEstimateTokensC 走 C 实现（经 cgo 边界 + CString 分配）。
func BenchmarkEstimateTokensC(b *testing.B) {
	for name, in := range benchInputs {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(in)))
			for i := 0; i < b.N; i++ {
				_ = estimateTokensC(in)
			}
		})
	}
}

// BenchmarkEstimateTokensPure 直调纯 Go 实现（同进程，无边界开销）。
// 与 C 版的差值即「跨语言开销 − C 实现省下的时间」。
func BenchmarkEstimateTokensPure(b *testing.B) {
	for name, in := range benchInputs {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(in)))
			for i := 0; i < b.N; i++ {
				_ = estimateTokensPure(in)
			}
		})
	}
}

// BenchmarkTruncateByTokensC 走 C（含 malloc/free 与结果拷贝）。
func BenchmarkTruncateByTokensC(b *testing.B) {
	for name, in := range benchInputs {
		if in == "" {
			continue
		}
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(in)))
			for i := 0; i < b.N; i++ {
				_ = truncateByTokensC(in, 64)
			}
		})
	}
}

// BenchmarkTruncateByTokensPure 直调纯 Go 实现。
func BenchmarkTruncateByTokensPure(b *testing.B) {
	for name, in := range benchInputs {
		if in == "" {
			continue
		}
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(in)))
			for i := 0; i < b.N; i++ {
				_ = truncateByTokensPure(in, 64)
			}
		})
	}
}

// BenchmarkModelContextWindowC 模型名映射（典型高频：每次预算计算）。
// 输入是短 ASCII，cgo 固定开销占比最高，是 C 化最可能「不划算」的场景。
func BenchmarkModelContextWindowC(b *testing.B) {
	models := []string{"deepseek-v4.1-flash", "gpt-4-turbo", "qwen-max", "AUTO"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = modelContextWindowC(models[i%len(models)])
	}
}

func BenchmarkModelContextWindowPure(b *testing.B) {
	models := []string{"deepseek-v4.1-flash", "gpt-4-turbo", "qwen-max", "AUTO"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = modelContextWindowPure(models[i%len(models)])
	}
}
