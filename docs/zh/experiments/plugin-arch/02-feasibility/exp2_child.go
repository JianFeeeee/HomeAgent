//go:build ignore
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// 子进程：fd 3 = eventfd(通知), fd 4 = shm 文件
func main() {
	efd := os.NewFile(3, "evt")
	shmf := os.NewFile(4, "shm")

	data, err := unix.Mmap(int(shmf.Fd()), 0, 4096, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil { fmt.Println("CHILD mmap 失败:", err); os.Exit(1) }
	fmt.Printf("CHILD: mmap 基址 = %p\n", unsafe.Pointer(&data[0]))

	buf := make([]byte, 8)
	if _, err := efd.Read(buf); err != nil {
		fmt.Println("CHILD read err:", err); os.Exit(1)
	}
	n := binary.LittleEndian.Uint64(buf)
	fmt.Printf("CHILD: 被 eventfd 唤醒, 计数=%d\n", n)

	// 按偏移读：头部 16 字节 = {off uint32, len uint32, seq uint64}
	off := binary.LittleEndian.Uint32(data[0:4])
	ln := binary.LittleEndian.Uint32(data[4:8])
	seq := binary.LittleEndian.Uint64(data[8:16])
	payload := string(data[off : off+ln])
	fmt.Printf("CHILD: 偏移解引用 off=%d len=%d seq=%d → %q\n", off, ln, seq, payload)

	// 子进程回写（验证双向可见）
	copy(data[2048:], []byte("CHILD-ACK"))
	binary.LittleEndian.PutUint32(data[16:20], 2048)
	binary.LittleEndian.PutUint32(data[20:24], uint32(len("CHILD-ACK")))
	fmt.Println("CHILD: 已回写 ACK")
}
