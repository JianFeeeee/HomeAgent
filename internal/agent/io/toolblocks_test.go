package io

import (
	"sync"
	"testing"
)

// 阶段 2b：`ConsumeToolBlocks` 从 **IOManager 级单队列** 改为 **per-call**。
//
// 现状（channel.go:1010-1021）：SetToolBlocks 写入一个共享的
// toolPendingBlocks，ConsumeToolBlocks 取走并清空。它是**全局单槽**，
// 没有 call_id 维度。
//
// 并行化的直接后果：同批多个工具各自注入媒体时，后执行的
// ConsumeToolBlocks 会**抢走**前一个的块 ⇒ 媒体挂到错误的 tool 消息上。
// 而多模态插件在 3 处调用 SetToolBlocks（multimodal/plugin.go:136,246,320），
// 这是真实使用面。
//
// 这条判据钉死「谁注入的归谁」——当前实现必然失败。

// 并发：两个工具各自注入媒体，各自取回，要求**互不串味**。
//
// 不用共享 map 收集结果：goroutine 内写 map 需要额外同步，而按
// 「各 goroutine 只写自己的下标」写切片即可，无需任何锁。
func TestConsumeToolBlocks_PerCallIsolation(t *testing.T) {
	m := NewIOManager()

	// 模拟两个工具按并行序注入：alpha 先注入，beta 后注入，
	// 但取回顺序不定（调度决定，这正是并行下的真实情况）。
	m.SetToolBlocksFor("call_alpha", []interface{}{"alpha-block"})
	m.SetToolBlocksFor("call_beta", []interface{}{"beta-block"})

	var alpha, beta []interface{}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		alpha = m.ConsumeToolBlocksFor("call_beta")
	}()
	go func() {
		defer wg.Done()
		beta = m.ConsumeToolBlocksFor("call_alpha")
	}()
	wg.Wait()

	// alpha 的槽里装的应是 beta 抢到的结果？不——上面故意交叉，
	// 目的是让两个 goroutine 真正并发取回；此处只断言**各自取到的内容对得上**。
	if len(alpha) != 1 || alpha[0] != "beta-block" {
		t.Errorf("取 call_beta 的 goroutine 拿到 %#v，期望 [beta-block]", alpha[0])
	}
	if len(beta) != 1 || beta[0] != "alpha-block" {
		t.Errorf("取 call_alpha 的 goroutine 拿到 %#v，期望 [alpha-block]", beta[0])
	}
}

// 取走是**消费**语义：同一 call_id 取第二次必须为空。
func TestConsumeToolBlocksFor_IsConsuming(t *testing.T) {
	m := NewIOManager()
	m.SetToolBlocksFor("c1", []interface{}{"x"})
	if b := m.ConsumeToolBlocksFor("c1"); len(b) != 1 {
		t.Fatalf("首次取回应得 1 块，实际 %#v", b)
	}
	if b := m.ConsumeToolBlocksFor("c1"); len(b) != 0 {
		t.Errorf("二次取回应为空（消费语义），实际 %#v", b)
	}
}

// 未知 call_id 取回必须为空，且**不得**影响他人的块。
func TestConsumeToolBlocksFor_UnknownCallIsEmpty(t *testing.T) {
	m := NewIOManager()
	m.SetToolBlocksFor("c1", []interface{}{"mine"})
	if b := m.ConsumeToolBlocksFor("no_such_call"); len(b) != 0 {
		t.Errorf("未知 call 应返回空，实际 %#v", b)
	}
	if b := m.ConsumeToolBlocksFor("c1"); len(b) != 1 {
		t.Errorf("取未知 call 误伤了 c1 的块: %#v", b)
	}
}

// 并发压力：N 个 call 各自注入并取回，要求**零串味**。
// 单槽实现在此必然大面积失败——这正是判据的价值。
func TestConsumeToolBlocksFor_ConcurrentNoCrossTalk(t *testing.T) {
	m := NewIOManager()
	const n = 32

	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		id := string(rune('A' + i%26))
		want := "block-" + string(rune('0'+i%10)) + "-" + id
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.SetToolBlocksFor(id, []interface{}{want})
			b := m.ConsumeToolBlocksFor(id)
			if len(b) != 1 || b[0] != want {
				errs <- "call " + id + " 期望 [" + want + "]，实际 " + toStr(b)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func toStr(b []interface{}) string {
	s := "["
	for i, v := range b {
		if i > 0 {
			s += " "
		}
		s += v.(string)
	}
	return s + "]"
}
