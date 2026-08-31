//go:build ignore
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// 模拟插件：拿锁 → 读 final_text → 追加自己的标记 → 写回 → 放锁
// 锁通过 stdio RPC 向内核申请（方案 3.7：锁仲裁回归内核，无 cgo）
func main() {
	tag := os.Args[1]
	shmf := os.NewFile(3, "shm")
	data, err := unix.Mmap(int(shmf.Fd()), 0, 65536, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil { fmt.Fprintln(os.Stderr, "mmap:", err); os.Exit(1) }

	dec := json.NewDecoder(bufio.NewReader(os.Stdin))
	w := bufio.NewWriter(os.Stdout)
	enc := json.NewEncoder(w)
	rpc := func(method string) {
		enc.Encode(map[string]string{"method": method}); w.Flush()
		var r map[string]interface{}; dec.Decode(&r)
	}

	const iters = 300
	for i := 0; i < iters; i++ {
		rpc("stage.lock")
		// --- 临界区：偏移解引用读写 final_text ---
		off := binary.LittleEndian.Uint32(data[0:4])
		ln := binary.LittleEndian.Uint32(data[4:8])
		cur := string(data[off : off+ln])
		add := tag
		newS := cur + add
		// append-only arena：写到新位置
		newOff := binary.LittleEndian.Uint32(data[8:12])
		if int(newOff)+len(newS) > 65536 { rpc("stage.unlock"); break }
		copy(data[newOff:], []byte(newS))
		binary.LittleEndian.PutUint32(data[0:4], newOff)
		binary.LittleEndian.PutUint32(data[4:8], uint32(len(newS)))
		binary.LittleEndian.PutUint32(data[8:12], newOff+uint32(len(newS)))
		rpc("stage.unlock")
	}
	fmt.Fprintln(os.Stderr, "worker "+tag+" done, iters="+strconv.Itoa(iters))
}
