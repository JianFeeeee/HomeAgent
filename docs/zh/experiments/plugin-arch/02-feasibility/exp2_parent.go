//go:build ignore
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func main() {
	fmt.Println("=== 实验 2：跨进程 eventfd 通知 + 共享内存偏移解引用 ===")

	// eventfd 不带 CLOEXEC（需要被子进程继承）
	efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK)
	if err != nil { panic(err) }
	evtFile := os.NewFile(uintptr(efd), "evt")

	// shm: 用 memfd（匿名，无需 /dev/shm 清理）
	mfd, err := unix.MemfdCreate("stagectx", 0)
	if err != nil { panic(err) }
	if err := unix.Ftruncate(mfd, 4096); err != nil { panic(err) }
	shmFile := os.NewFile(uintptr(mfd), "shm")

	data, err := unix.Mmap(mfd, 0, 4096, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil { panic(err) }
	fmt.Printf("PARENT: mmap 基址 = %p\n", unsafe.Pointer(&data[0]))

	// 写 payload 到 arena(偏移 1024)，头部记描述符
	msg := "hello-from-parent-via-offset"
	copy(data[1024:], []byte(msg))
	binary.LittleEndian.PutUint32(data[0:4], 1024)
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(msg)))
	binary.LittleEndian.PutUint64(data[8:16], 42)
	fmt.Printf("PARENT: 数据已落地 arena@1024, 描述符 {off:1024, len:%d, seq:42}\n", len(msg))

	cmd := exec.Command("go", "run", "exp2_child.go")
	cmd.ExtraFiles = []*os.File{evtFile, shmFile} // → 子进程 fd 3, 4
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil { panic(err) }

	time.Sleep(3 * time.Second) // 等 go run 编译+启动
	fmt.Println("PARENT: 数据到位后 post eventfd（不等待消费者）")
	t0 := time.Now()
	evtFile.Write([]byte{1,0,0,0,0,0,0,0})
	fmt.Printf("PARENT: post 耗时 %v ← post-and-forget\n", time.Since(t0))

	cmd.Wait()

	// 读子进程回写
	off := binary.LittleEndian.Uint32(data[16:20])
	ln := binary.LittleEndian.Uint32(data[20:24])
	if ln > 0 {
		fmt.Printf("PARENT: 读到子进程回写 → %q ✅ 双向可见\n", string(data[off:off+ln]))
	}
}
