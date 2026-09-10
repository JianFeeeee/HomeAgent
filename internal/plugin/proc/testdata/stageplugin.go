//go:build ignore

// stageplugin 是完整形态的测试插件：注册工具/阶段/输出通道，
// stage 处理经共享内存读改写（模拟 sanitizer 的清洗行为）。
//
// 它手写 RPC 与共享段访问，不依赖公开 SDK——因为 SDK 侧的 proc 支持
// 属于 Part 3（plugindev 工具链）的内容。这里只验证内核侧机制。
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
)

type request struct {
	ID     uint64          `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type response struct {
	ID     uint64      `json:"id"`
	Result interface{} `json:"result,omitempty"`
	Error  string      `json:"error,omitempty"`
}

var (
	out     = bufio.NewWriter(os.Stdout)
	writeMu sync.Mutex

	nextID  uint64
	pendMu  sync.Mutex
	pending = map[uint64]chan json.RawMessage{}

	shm     []byte
	shmSize int
	region  []byte // 完整统一区域 mmap
	arenaOff uint32
	arenaUsed uint32
)

func send(v interface{}) {
	b, _ := json.Marshal(v)
	writeMu.Lock()
	out.Write(b)
	out.WriteByte('\n')
	out.Flush()
	writeMu.Unlock()
}

func callKernel(method string, params interface{}) json.RawMessage {
	pendMu.Lock()
	nextID++
	id := nextID
	ch := make(chan json.RawMessage, 1)
	pending[id] = ch
	pendMu.Unlock()

	var raw json.RawMessage
	if params != nil {
		b, _ := json.Marshal(params)
		raw = b
	}
	send(request{ID: id, Method: method, Params: raw})
	return <-ch
}

// ---- 共享段访问（与内核 proc 包的布局一致）----

const (
	headerSize = 64
	// 与 proc 包保持一致：18 个字段 × 8 字节 + 8 字节标志位
	stageFieldCount = 18
	sliceSize       = 8
	flagCount       = 8
	ctxSize         = stageFieldCount*sliceSize + flagCount

	offArenaBase = 8
	offArenaCap  = 12
	offArenaUsed = 16
	offCtxBase   = 20
	offSeq       = 24

	// 字段索引（与 proc/shmcodec.go 的 stageField 枚举顺序一致）
	fToolResults = 10
)

func arenaBase() uint32 { return binary.LittleEndian.Uint32(shm[offArenaBase:]) }
func arenaCap() uint32  { return binary.LittleEndian.Uint32(shm[offArenaCap:]) }
func ctxBase() uint32   { return binary.LittleEndian.Uint32(shm[offCtxBase:]) }

func descOffset(field int) uint32 { return ctxBase() + uint32(field*sliceSize) }

func getDesc(field int) (off, ln uint32) {
	o := descOffset(field)
	return binary.LittleEndian.Uint32(shm[o:]), binary.LittleEndian.Uint32(shm[o+4:])
}

func setDesc(field int, off, ln uint32) {
	o := descOffset(field)
	binary.LittleEndian.PutUint32(shm[o:], off)
	binary.LittleEndian.PutUint32(shm[o+4:], ln)
}

func readField(field int) []byte {
	off, ln := getDesc(field)
	if off == 0 && ln == 0 {
		return nil
	}
	if ln == 0 {
		return []byte{}
	}
	base := arenaBase()
	return shm[base+off : base+off+ln]
}

func writeField(field int, data []byte) error {
	used := binary.LittleEndian.Uint32(shm[offArenaUsed:])
	if used == 0 {
		used = 1
	}
	end := used + uint32(len(data))
	if end > arenaCap() {
		return fmt.Errorf("arena 空间不足")
	}
	base := arenaBase()
	copy(shm[base+used:], data)
	binary.LittleEndian.PutUint32(shm[offArenaUsed:], end)
	setDesc(field, used, uint32(len(data)))
	return nil
}

func bumpSeq() {
	v := binary.LittleEndian.Uint64(shm[offSeq:])
	binary.LittleEndian.PutUint64(shm[offSeq:], v+1)
}

type toolResult struct {
	CallID  string      `json:"call_id"`
	Name    string      `json:"name"`
	Plugin  string      `json:"plugin,omitempty"`
	Success bool        `json:"success"`
	Result  interface{} `json:"result"`
}

// handleStage 模拟 sanitizer：拿锁 → 读 ToolResults → 剥 ANSI → 只写脏字段 → 放锁
func handleStage() (int, error) {
	callKernel("stage.lock", nil)
	defer callKernel("stage.unlock", nil)

	raw := readField(fToolResults)
	if len(raw) == 0 {
		return 0, nil
	}
	var results []toolResult
	if err := json.Unmarshal(raw, &results); err != nil {
		return 0, err
	}

	before := string(raw)
	for i := range results {
		if s, ok := results[i].Result.(string); ok {
			results[i].Result = strings.NewReplacer("\x1b[31m", "", "\x1b[0m", "").Replace(s)
		}
	}
	after, _ := json.Marshal(results)

	// 只有真变了才写回 —— 这是消除 lost update 的核心
	if string(after) == before {
		return 0, nil
	}
	if err := writeField(fToolResults, after); err != nil {
		return 0, err
	}
	bumpSeq()
	return 1, nil
}

func main() {
	in := bufio.NewScanner(bufio.NewReader(os.Stdin))
	in.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for in.Scan() {
		line := make([]byte, len(in.Bytes()))
		copy(line, in.Bytes())

		var probe struct {
			ID     uint64 `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(line, &probe) != nil {
			continue
		}

		// 内核对我们反向调用的应答
		if probe.Method == "" {
			var resp struct {
				ID     uint64          `json:"id"`
				Result json.RawMessage `json:"result"`
			}
			json.Unmarshal(line, &resp)
			pendMu.Lock()
			ch, ok := pending[resp.ID]
			delete(pending, resp.ID)
			pendMu.Unlock()
			if ok {
				ch <- resp.Result
			}
			continue
		}

		var req request
		json.Unmarshal(line, &req)

		switch req.Method {
		case "handshake":
			var hp struct {
				ShmSize int `json:"shm_size"`
			}
			json.Unmarshal(req.Params, &hp)
			shmSize = hp.ShmSize
			if shmSize > 0 {
				// fd 3 = 内核传入的统一共享内存区域
				m, err := syscall.Mmap(3, 0, shmSize,
					syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					send(response{ID: req.ID, Error: fmt.Sprintf("mmap 共享段失败: %v", err)})
					continue
				}
				// 统一区域：前 64B 是 SuperBlock，StageContext 段在其后
				ctxOff := binary.LittleEndian.Uint32(m[20:])
				ctxSize := binary.LittleEndian.Uint32(m[24:])
				region = m
				arenaOff = binary.LittleEndian.Uint32(m[36:])
				shm = m[ctxOff : ctxOff+ctxSize]
			}
			send(response{ID: req.ID, Result: map[string]interface{}{
				"protocol": 1, "sdk_version": "test", "plugin_name": "stage", "pid": os.Getpid(),
			}})

		case "plugin.init":
			send(response{ID: req.ID})

		case "plugin.start":
			go func(id uint64) {
				callKernel("lifecycle.autoRestart", map[string]interface{}{"enabled": true})
				callKernel("tool.register", map[string]interface{}{
					"name":        "demo_upper",
					"def":         map[string]interface{}{"name": "demo_upper", "description": "转大写"},
					"has_cleaner": true,
				})
				callKernel("stage.register", map[string]interface{}{
					"stage": "after_toolcall",
					"scope": "global",
				})
				callKernel("input.register", map[string]interface{}{
					"name": "demo_in", "def": map[string]interface{}{}, "has_cleaner": true,
				})
				callKernel("output.register", map[string]interface{}{
					"name": "demo_ch", "caps": 1, "desc": "测试通道",
					"def": map[string]interface{}{}, "has_cleaner": true,
				})
				send(response{ID: id})
			}(req.ID)

		case "plugin.stop":
			send(response{ID: req.ID})
			out.Flush()
			os.Exit(0)

		case "tool.invoke":
			var p struct {
				Name string                 `json:"name"`
				Args map[string]interface{} `json:"args"`
			}
			json.Unmarshal(req.Params, &p)
			text, _ := p.Args["text"].(string)
			send(response{ID: req.ID, Result: map[string]interface{}{
				"result": strings.ToUpper(text),
			}})

		case "cleaner.invoke":
			var p struct {
				Scope   string `json:"scope"`
				Name    string `json:"name"`
				TextRef struct {
					Offset uint32 `json:"offset"`
					Length uint32 `json:"length"`
				} `json:"text_ref"`
			}
			json.Unmarshal(req.Params, &p)
			valid := (p.Scope == "tool" && p.Name == "demo_upper") ||
				(p.Scope == "input" && p.Name == "demo_in") ||
				(p.Scope == "output" && p.Name == "demo_ch")
			if !valid {
				send(response{ID: req.ID, Error: "未注册 Cleaner"})
				continue
			}
			// 从 arena 读文本
			input := string(region[p.TextRef.Offset : p.TextRef.Offset+p.TextRef.Length])
			output := p.Scope + "-cleaned:" + input
			// 写回 arena
			off := arenaUsed
			if off == 0 {
				off = 1
			}
			end := off + uint32(len(output))
			copy(region[arenaOff+end-uint32(len(output)):arenaOff+end], output)
			arenaUsed = end
			send(response{ID: req.ID, Result: map[string]interface{}{
				"text_ref": map[string]interface{}{
					"offset": arenaOff + off,
					"length": uint32(len(output)),
				},
			}})

		case "stage.invoke":
			go func(id uint64) {
				if shm == nil {
					send(response{ID: id, Error: "共享段未挂载"})
					return
				}
				n, err := handleStage()
				if err != nil {
					send(response{ID: id, Error: err.Error()})
					return
				}
				send(response{ID: id, Result: map[string]interface{}{"dirty_fields": n}})
			}(req.ID)

		case "output.invoke":
			var p struct {
				Channel string                 `json:"channel"`
				Args    map[string]interface{} `json:"args"`
			}
			json.Unmarshal(req.Params, &p)
			payload, _ := p.Args["payload"].(string)
			if payload == "fail" {
				// 模拟现网 qq 插件的真实失败：meta 缺 user_id
				send(response{ID: req.ID, Error: "meta 中缺少 user_id 字段"})
				continue
			}
			send(response{ID: req.ID, Result: map[string]interface{}{"status": "sent"}})

		default:
			if req.ID != 0 {
				send(response{ID: req.ID})
			}
		}
	}
}
