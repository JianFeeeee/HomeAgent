package core

// M2 验收测试：调度器骨架（就绪队列、选择函数、快照、panic 隔离）。
//
// 设计依据 docs/zh/input-scheduler-design.md §11.4（Q1/Q4）与 §11.5（O1/K1）。

import (
	"testing"
	"time"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
)

func mkTask(id uint64, level Level, at time.Time) *Task {
	return &Task{ID: id, Level: level, EnqueuedAt: at}
}

// Q1：选择函数的排序键是 (-Level, EnqueuedAt, ID)。
func TestPickTaskIndex_Ordering(t *testing.T) {
	base := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		queue []*Task
		want  []uint64
	}{
		{
			name: "高优先级先执行，与入队先后无关",
			queue: []*Task{
				mkTask(1, LevelBackground, base),
				mkTask(2, LevelCritical, base.Add(time.Second)),
				mkTask(3, LevelMessage, base.Add(2*time.Second)),
			},
			want: []uint64{2, 3, 1},
		},
		{
			name: "同优先级先到先服务",
			queue: []*Task{
				mkTask(1, LevelInteractive, base.Add(3*time.Second)),
				mkTask(2, LevelInteractive, base.Add(time.Second)),
				mkTask(3, LevelInteractive, base.Add(2*time.Second)),
			},
			want: []uint64{2, 3, 1},
		},
		{
			name: "同优先级同入队时刻用 ID 兜底（保证确定性）",
			queue: []*Task{
				mkTask(7, LevelMessage, base),
				mkTask(3, LevelMessage, base),
				mkTask(5, LevelMessage, base),
			},
			want: []uint64{3, 5, 7},
		},
		{
			name: "四级全覆盖",
			queue: []*Task{
				mkTask(1, LevelBackground, base),
				mkTask(2, LevelMessage, base),
				mkTask(3, LevelInteractive, base),
				mkTask(4, LevelCritical, base),
			},
			want: []uint64{4, 3, 2, 1},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := append([]*Task(nil), c.queue...)
			var got []uint64
			for len(q) > 0 {
				i := pickTaskIndex(q)
				got = append(got, q[i].ID)
				q = append(q[:i], q[i+1:]...)
			}
			if len(got) != len(c.want) {
				t.Fatalf("取出的任务数=%d，期望 %d", len(got), len(c.want))
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("执行顺序=%v，期望 %v", got, c.want)
				}
			}
		})
	}
}

// Q4：队列有界；满了必须拒绝并计数，而不是静默丢弃或无界增长。
func TestScheduler_EnqueueBackpressure(t *testing.T) {
	s := newScheduler(2)
	if !s.enqueue(newSelfTask(selfInputMsg{text: "a"})) {
		t.Fatal("第 1 个任务应入队成功")
	}
	if !s.enqueue(newSelfTask(selfInputMsg{text: "b"})) {
		t.Fatal("第 2 个任务应入队成功")
	}
	if s.hasRoom() {
		t.Fatal("队列已满，hasRoom 应为 false")
	}
	if s.enqueue(newSelfTask(selfInputMsg{text: "c"})) {
		t.Fatal("队列满时第 3 个任务必须被拒绝")
	}
	if s.stats.Rejected != 1 {
		t.Fatalf("Rejected=%d，期望 1", s.stats.Rejected)
	}
	if s.stats.Enqueued != 2 {
		t.Fatalf("Enqueued=%d，期望 2", s.stats.Enqueued)
	}
}

// 生命周期：next 置 running 并移出队列；done 清 running 并累加计数。
func TestScheduler_Lifecycle(t *testing.T) {
	s := newScheduler(4)
	s.enqueue(newSelfTask(selfInputMsg{text: "a"}))
	s.enqueue(newSelfTask(selfInputMsg{text: "b"}))

	t1 := s.next()
	if t1 == nil || s.running != t1 {
		t.Fatal("next 应取出任务并置为 running")
	}
	if len(s.queue) != 1 {
		t.Fatalf("取出后队列长度=%d，期望 1", len(s.queue))
	}
	// 队列内不得同时出现 running（O1：三集合互不重叠）。
	for _, q := range s.queue {
		if q == t1 {
			t.Fatal("running 任务不得同时留在就绪队列")
		}
	}

	s.done(t1)
	if s.running != nil {
		t.Fatal("done 后 running 应为 nil")
	}
	if s.stats.Executed != 1 {
		t.Fatalf("Executed=%d，期望 1", s.stats.Executed)
	}
	if s.next() == nil {
		t.Fatal("队列里还有 b，next 不应为 nil")
	}
	if s.next() != nil {
		t.Fatal("队列已空，next 应返回 nil")
	}
}

// K1：任务 panic 必须被隔离——调度器统计仍然推进，且不向外抛出。
func TestScheduler_PanicIsolationOnExecuteTask(t *testing.T) {
	sp := &scriptProvider{}
	a := New(AgentConfig{
		ID:              "sched-panic",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
	})

	// Event 为 nil：handleInput 解引用即 panic，用来验证 recover 生效。
	task := &Task{Kind: TaskKindInput, Level: DefaultLevel, Event: nil}
	a.executeTask(task) // 若未隔离，这里会 panic 冒泡使测试失败

	if a.sched.stats.Executed != 1 {
		t.Fatalf("panic 后 Executed=%d，期望 1（任务失败但调度器存活）", a.sched.stats.Executed)
	}
	if a.sched.running != nil {
		t.Fatal("panic 后 running 必须被清空")
	}
}

// O1 轻量版：快照与内部状态一致，且 running 不出现在 queue 里。
func TestScheduler_SnapshotConsistency(t *testing.T) {
	sp := &scriptProvider{}
	a := New(AgentConfig{
		ID:              "sched-snap",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
	})

	a.sched.enqueue(newSelfTask(selfInputMsg{text: "a"}))
	a.sched.enqueue(newSelfTask(selfInputMsg{text: "b"}))
	snap := a.DumpScheduler()
	if snap.Running != nil {
		t.Fatal("尚未 next，快照的 running 应为 nil")
	}
	if len(snap.Queue) != 2 || snap.Stats.Enqueued != 2 {
		t.Fatalf("快照不一致：queue=%d enqueued=%d", len(snap.Queue), snap.Stats.Enqueued)
	}

	r := a.sched.next()
	a.executeTask(&Task{Kind: TaskKindSelf, Level: DefaultLevel, Self: selfInputMsg{text: "a"}})
	snap = a.DumpScheduler()
	if snap.Running != r {
		t.Fatal("执行完成后 running 应仍指向未 done 的任务")
	}
	for _, q := range snap.Queue {
		if q == r {
			t.Fatal("快照中 running 与 queue 不得重叠")
		}
	}

	// 队列快照必须是副本：改快照不得影响调度器。
	snap.Queue = append(snap.Queue, &Task{})
	if len(a.DumpScheduler().Queue) != 1 {
		t.Fatal("DumpScheduler 必须返回队列副本")
	}
}

// Level 的字面量是持久化/日志契约，改值必须是有意的。
func TestLevelContract(t *testing.T) {
	if LevelBackground != 1 || LevelMessage != 2 || LevelInteractive != 3 || LevelCritical != 4 {
		t.Fatalf("四级取值被改动：%d/%d/%d/%d",
			LevelBackground, LevelMessage, LevelInteractive, LevelCritical)
	}
	if DefaultLevel != LevelBackground {
		t.Fatalf("默认级必须是 L1（显式才是特权），实际 %v", DefaultLevel)
	}
}
