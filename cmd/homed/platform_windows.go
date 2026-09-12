//go:build windows

package main

import (
	"fmt"
	"os"
)

// requireSupportedPlatform 在原生 Windows 上直接拒绝启动 homed。
//
// 为什么不做原生支持（不是「还没来得及做」，是设计上不做）：
//
// homed 的插件体系建立在两个原语上——**继承的 fd**（Single memfd: 统一共享
// 内存区 + eventfd 通知）与**同段内相对偏移解引用**（各进程 mmap 到不同虚拟
// 基址，段内一律用偏移互相读写，这样插件回调才能就地改写内核看到的那份数据）。
//
// Windows 的等价物是命名内核对象（CreateFileMappingW / OpenEventW）＋句柄表，
// 没有 fd 继承语义（os/exec 的 ExtraFiles 在 Windows 上直接不被支持），
// 生命周期与权限模型也按句柄而非进程继承来组织。要在其上重建这套语义，
// 等于再维护一套平台专属 ABI 与安全边界——而 C ABI 时代正是「三套 ABI 并存
// 导致改写型插件在某个平台上静默失效」的教训（§9.2）。
//
// 所以选择：**原生 Windows 不提供 homed**。Windows 用户跑 WSL2——
// WSL2 里就是普通 linux/amd64，走与我们测试矩阵完全相同的那条路径。
//
// 注意范围：只有 homed 如此。hmapdev 工具链仍可在 Windows 上运行
// （在 Windows 上开发、为 WSL 构建 linux 插件是合理工作流）。
func requireSupportedPlatform() {
	fmt.Fprintln(os.Stderr, "homed 不支持 Windows 原生运行。")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "原因：子进程插件依赖 fd 继承 + 统一共享内存区的段内偏移解引用，")
	fmt.Fprintln(os.Stderr, "而 Windows 的句柄模型无法表达这两者；强行适配等于再维护一套平台专属")
	fmt.Fprintln(os.Stderr, "ABI——C ABI 时代三套 ABI 并存曾导致改写型插件在某个平台上静默失效。")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "请改用 WSL2：")
	fmt.Fprintln(os.Stderr, "  1. wsl --install -d Ubuntu        # 安装 WSL2")
	fmt.Fprintln(os.Stderr, "  2. 在 WSL 内下载 linux/amd64 的 homed 与插件（.hmap）")
	fmt.Fprintln(os.Stderr, "  3. 在 WSL 内运行 homed：与 Linux 主机完全相同，无需额外配置")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "数据目录可放在 /mnt/c/... 下以便与 Windows 侧共享，")
	fmt.Fprintln(os.Stderr, "但不建议（跨文件系统 IO 慢、inotify 语义受限）；推荐放在 WSL 内部路径。")
	os.Exit(2)
}
