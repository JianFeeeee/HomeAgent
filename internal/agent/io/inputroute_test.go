package io

import "testing"

// 输入路由：路由器说"已被别的 agent 接管"时，事件**不得**进本内核队列。
//
// 语义（设计 §4.1）：inputch 是可分配资源，划给某个 agent 后输入只流向它 ——
// "父也顺便看到一份"是错的。
func TestInputRouter_TakesOverExclusively(t *testing.T) {
	m := NewIOManager()
	var got []*InputEvent
	var sawInterrupt bool
	m.SetInputRouter(func(evt *InputEvent, isInterrupt bool) bool {
		got = append(got, evt)
		sawInterrupt = sawInterrupt || isInterrupt
		return true // 全部接管
	})

	m.InjectInputTo("plugin-x", "sub/in", "text", map[string]interface{}{"content": "a"})
	m.InjectInterruptTextOpts("plugin-x", "sub/in", "b", InjectOptions{})

	if len(got) < 2 {
		t.Fatalf("路由器应被调用（含中断路径），实际 %d 次", len(got))
	}
	if n := len(m.InputChan()); n != 0 {
		t.Fatalf("被接管的排队输入不得进本内核队列，实际 %d 条", n)
	}
	if !sawInterrupt {
		t.Fatal("中断注入也必须经过路由（否则中断会绕过 inputch 归属直投父）")
	}
	if got[0].OutputChannel != "sub/in" {
		t.Fatalf("路由器应拿到事件的 inputch，得到 %q", got[0].OutputChannel)
	}
}

// 路由器放行（返回 false）或未设置时，行为与以前完全一致。
func TestInputRouter_PassthroughKeepsOldBehaviour(t *testing.T) {
	m := NewIOManager()
	calls := 0
	m.SetInputRouter(func(evt *InputEvent, isInterrupt bool) bool { calls++; return false })

	m.InjectInputTo("plugin-x", "sub/in", "text", map[string]interface{}{"content": "a"})
	if calls != 1 {
		t.Fatalf("路由器应被调用一次，实际 %d", calls)
	}
	if n := len(m.InputChan()); n != 1 {
		t.Fatalf("放行的输入应进本内核队列，实际 %d 条", n)
	}
	if _, ok := <-m.InputChan(); !ok {
		t.Fatal("队列应可读")
	}

	// 未设路由器：直接入队（历史行为）
	m2 := NewIOManager()
	m2.InjectInterruptText("plugin-x", "sub/in", "c")
	if n := len(m2.InputInterruptChan()); n != 1 {
		t.Fatalf("未设路由器时中断应直接入队，实际 %d 条", n)
	}
}

// DeliverRouted 是不再二次路由的投递口（路由器实现把事件交给持有者）。
func TestDeliverRouted_SkipsSecondRouting(t *testing.T) {
	m := NewIOManager()
	routerCalls := 0
	m.SetInputRouter(func(evt *InputEvent, isInterrupt bool) bool { routerCalls++; return true })

	m.DeliverRouted(&InputEvent{OutputChannel: "sub/in"}, false)
	if routerCalls != 0 {
		t.Fatalf("DeliverRouted 不应再触发路由（会成环），实际 %d 次", routerCalls)
	}
	if n := len(m.InputChan()); n != 1 {
		t.Fatalf("应已入队，实际 %d 条", n)
	}
}
