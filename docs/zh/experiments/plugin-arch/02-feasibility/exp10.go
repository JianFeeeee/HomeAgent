//go:build ignore
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

func main() {
	fmt.Println("=== 实验 10：多媒体 payload —— 共享内存零拷贝 vs JSON base64 ===")
	sizes := []int{100 * 1024, 1024 * 1024, 5 * 1024 * 1024}
	for _, sz := range sizes {
		img := make([]byte, sz)
		for i := range img { img[i] = byte(i % 251) }

		// A. JSON + base64（当前 ContentBlock 的做法）
		t0 := time.Now()
		b64 := base64.StdEncoding.EncodeToString(img)
		blob, _ := json.Marshal(map[string]string{"type": "image_url", "url": "data:image/png;base64," + b64})
		var back map[string]string
		json.Unmarshal(blob, &back)
		dec, _ := base64.StdEncoding.DecodeString(back["url"][22:])
		jsonDur := time.Since(t0)

		// B. 共享内存 arena（写入 + 偏移解引用，零拷贝读）
		mfd, _ := unix.MemfdCreate("arena", 0)
		unix.Ftruncate(mfd, int64(sz+4096))
		data, _ := unix.Mmap(mfd, 0, sz+4096, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		t0 = time.Now()
		copy(data[4096:], img)              // 写 arena
		view := data[4096 : 4096+sz]        // 偏移解引用 = 零拷贝切片
		_ = view[sz-1]
		shmDur := time.Since(t0)
		unix.Munmap(data)
		unix.Close(mfd)

		fmt.Printf("\n%s payload:\n", map[int]string{100*1024:"100KB", 1024*1024:"1MB", 5*1024*1024:"5MB"}[sz])
		fmt.Printf("  A JSON+base64: %8v  传输体积 %d B (+%.0f%%)  解出 %d B %s\n",
			jsonDur, len(blob), float64(len(blob)-sz)/float64(sz)*100, len(dec),
			map[bool]string{true:"✓",false:"✗"}[len(dec)==sz])
		fmt.Printf("  B 共享内存:    %8v  传输体积 8 B (描述符)      零拷贝视图 %d B\n", shmDur, len(view))
		fmt.Printf("  → 加速 %.0fx, 体积节省 %.0f%%\n",
			float64(jsonDur)/float64(shmDur), float64(len(blob)-8)/float64(len(blob))*100)
	}
}
