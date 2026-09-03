package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

// 子进程架构的性能基准（Part 6.6 验收项）。
//
// 对照基线来自 docs/zh/experiments/plugin-arch：
//
//	实验 3  锁仲裁 RPC 往返    19.40 µs/次
//	实验 4  post-and-forget    5.07s → 2.29ms（5000 token + 20µs 慢消费者）
//	实验 11 工具调用 RPC p50    19.6 µs
//
// 这些基准回答的是「进程边界的代价是否可忽略」——相对 stage handler 的实际
// 工作量（LLM 往返 2-8 秒），微秒级往返不构成问题；但若退化到毫秒级，
// 高频工具调用就会被感知。
//
// 实测结果（2026-09-02，AMD Ryzen 7 7840HS）：
//
//	ToolInvoke                     24.1 µs/op    ← 对照实验 11 的 19.6µs，同量级
//	StageLockArbitration            0.76 µs/op    ← 仅内核侧仲裁，不跨进程
//	EvtRingWritePush               95    ns/op
//	EvtRingWritePushConcurrent     83    ns/op    ← 并发不恶化
//	StageInvokeSharedMemory       132    µs/op    ← 含 3 次进程间往返
//	SegmentWriteAllReadInto         3.7  µs/op    ← 占 stage 的 2.8%

// buildBenchPlugin 编译 testdata 里的测试插件（benchmark 版）。
func buildBenchPlugin(b *testing.B, srcName string) string {
	b.Helper()
	src := filepath.Join("testdata", srcName)
	if _, err := os.Stat(src); err != nil {
		b.Skipf("测试插件源码缺失 %s: %v", src, err)
	}
	bin := filepath.Join(b.TempDir(), "benchplugin")
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		b.Fatalf("编译 %s: %v\n%s", srcName, err, out)
	}
	return bin
}

// BenchmarkToolInvoke 测量内核 → 插件的工具调用往返。
//
// 链路：Call 写 stdin → 插件读循环 → handler → 写 stdout →
// 内核 readLoop → pending channel 唤醒。对照实验 11 的 19.6µs。
func BenchmarkToolInvoke(b *testing.B) {
	bin := buildBenchPlugin(b, "echoplugin.go")

	p, err := Spawn("echo", bin, Options{Handler: noopHandler})
	if err != nil {
		b.Fatalf("Spawn: %v", err)
	}
	defer p.Kill()

	args := map[string]interface{}{"text": "benchmark"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.Call(MethodToolInvoke, ToolInvokeParams{
			Name: "echo_tool",
			Args: args,
		}); err != nil {
			b.Fatalf("第 %d 次调用失败: %v", i, err)
		}
	}
}

// BenchmarkStageLockArbitration 测量**内核侧锁仲裁本身**的成本。
//
// ⚠️ 不要拿这个数字对照实验 3 的 19.40µs——两者测的不是同一个东西：
//   - 实验 3：插件经 RPC 请求锁的**完整跨进程往返**
//   - 本基准：仅 lockRegistry.acquire/release，不跨进程
//
// 真实成本仍在 20µs 量级（那部分是 RPC 往返，见 BenchmarkToolInvoke）。
// 本基准的用途是确认仲裁逻辑自身不是瓶颈：若它也到了微秒级，
// 说明 sync.Mutex 之外又引入了什么开销。
func BenchmarkStageLockArbitration(b *testing.B) {
	lock := newStageLock()
	r := &lockRegistry{}
	r.bind(lock)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := r.acquire("bench"); err != nil {
			b.Fatalf("acquire: %v", err)
		}
		if err := r.release("bench"); err != nil {
			b.Fatalf("release: %v", err)
		}
	}
}

// BenchmarkEvtRingWritePush 测量事件环写入（Bus.Publish 路径）。
//
// 这是 §4.3 标记「风险高」的那一项：流式输出逐 token 发布，
// Publish 路径上任何阻塞都会直接卡顿。
func BenchmarkEvtRingWritePush(b *testing.B) {
	host, err := NewHost()
	if err != nil {
		b.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	ring := host.EvtRing()
	payload := []byte(`{"type":"content_delta","payload":{"text":"token"}}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ring.WritePush(pubsdk.EventContentDelta, payload)
	}
}

// BenchmarkEvtRingWritePushConcurrent 并发写入。
//
// 内核有多条路径并发发布事件（主循环、工具调用、流式增量），
// writeSeq 是 atomic 而 arena 分配有锁——确认锁不是瓶颈。
func BenchmarkEvtRingWritePushConcurrent(b *testing.B) {
	host, err := NewHost()
	if err != nil {
		b.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	ring := host.EvtRing()
	payload := []byte(`{"type":"content_delta","payload":{"text":"tok"}}`)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ring.WritePush(pubsdk.EventContentDelta, payload)
		}
	})
}

// BenchmarkStageInvokeSharedMemory 测量完整 stage 往返：
// 写共享段 → RPC → 插件读改写 → 回读 → 压实。
//
// 这是迁移引入的最重路径，每次 stage 都要走一遍。
//
// 实测 ~132µs，比单次 RPC（~24µs）高 5 倍，因为**一次 stage 要走 3 次
// 进程间往返**：stage.invoke + 插件侧反向的 stage.lock / stage.unlock。
// 共享段编解码只占 3.7µs（2.8%）——成本在往返次数而非数据搬运。
//
// 相对 LLM 往返 2-8 秒可忽略。若日后要优化，方向是把 lock/unlock
// 合入 stage.invoke 的请求/应答，省掉两次往返。
func BenchmarkStageInvokeSharedMemory(b *testing.B) {
	bin := buildBenchPlugin(b, "stageplugin.go")

	host, err := NewHost()
	if err != nil {
		b.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	core := newFakeCore()
	p := New("sanitizer", bin, b.TempDir(), nil, host, nil)
	if err := p.Start(core); err != nil {
		b.Fatalf("Start: %v", err)
	}
	defer p.Close()

	handlers := core.stageHandlers(pubsdk.StageAfterToolcall)
	if len(handlers) != 1 {
		b.Fatalf("应注册 1 个 handler，实际 %d", len(handlers))
	}
	handler := handlers[0]

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sc := &pubsdk.StageContext{
			Phase: pubsdk.StageAfterToolcall,
			ToolResults: []pubsdk.ToolResult{
				{CallID: "c1", Name: "t", Result: "结果：\x1b[31m脏\x1b[0m"},
			},
		}
		if err := handler(sc); err != nil {
			b.Fatalf("第 %d 次 stage 失败: %v", i, err)
		}
	}
}

// BenchmarkSegmentWriteAllReadInto 只测共享段编解码（不含 RPC）。
//
// 用于拆分 stage 往返的成本构成：编解码 vs 进程间通信。
func BenchmarkSegmentWriteAllReadInto(b *testing.B) {
	host, err := NewHost()
	if err != nil {
		b.Fatalf("NewHost: %v", err)
	}
	defer host.Close()
	seg := host.Segment()

	sc := &pubsdk.StageContext{
		Phase:      pubsdk.StageAfterToolcall,
		RawMessage: "用户输入的一段话",
		UserID:     "u1",
		LLMText:    "模型输出的文本",
		FinalText:  "最终文本",
		ToolResults: []pubsdk.ToolResult{
			{CallID: "c1", Name: "tool_a", Result: "结果 A"},
			{CallID: "c2", Name: "tool_b", Result: "结果 B"},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := seg.WriteAll(sc); err != nil {
			b.Fatalf("WriteAll: %v", err)
		}
		if err := seg.ReadInto(sc); err != nil {
			b.Fatalf("ReadInto: %v", err)
		}
		seg.Compact()
	}
}
