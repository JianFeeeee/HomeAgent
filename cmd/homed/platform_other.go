//go:build !windows

package main

// requireSupportedPlatform 在受支持的平台上不做任何事。
//
// 平台策略见 platform_windows.go：只有 homed 放弃 Windows 原生支持
// （插件体系依赖 fd 继承与共享内存段内偏移），Windows 用户走 WSL2。
func requireSupportedPlatform() {}
