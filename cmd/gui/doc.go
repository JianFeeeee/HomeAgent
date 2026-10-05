// Package gui 是 Electron 桌面外壳（Node/Electron 实现，无 Go 运行时代码）。
//
// 这个包存在的原因不是功能，而是让 `go test ./...` / `go build ./...` 能
// 统一遍历仓库：cmd/gui 下没有任何 Go 源文件时，go 工具链对它报
// "no Go files"（exit 1），会被测试编排器（pi-lens test-runner 等）当成
// 失败并反复重试。一个只有包注释的 doc.go 让该目录成为合法（空）包。
package gui
