package memory

// R5 的判据：场景成立时清证据，只清**本指纹那个桶**，不是全表。
//
// 现状（scene_emerge.go createSceneLocked 末尾）：
//
//	DELETE FROM situation_evidence   ← 全表清
//
// 后果：多场景并发轮次下，A 场景的建立会连带清掉 B 尚未攒够
// minSceneEvidence=2 的证据 ⇒ 门槛判定被「别的场景刚好长出来」随机打断。
// 这不是理论：QQ / mc / webui 三个通道在同一进程里各自计数。
//
// 判据参照物在生产代码之外：期望值是「建一个场景后，其它桶的证据必须原样还在」。

import (
	"os"
	"testing"
)

// evidenceCount 读某桶的累计次数（直查表，不走被测函数）。
func evidenceCount(t *testing.T, g *GraphDB, label string) int {
	t.Helper()
	var n int
	err := g.db.QueryRow(
		`SELECT COALESCE((SELECT count FROM situation_evidence WHERE label = ?), 0)`, label,
	).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A 场景成立时，B 桶的证据不得被动。
func TestSceneCreationKeepsOtherEvidence(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	qq := mkSig("qq", "", "")
	webui := mkSig("webui", "", "")

	// 两个通道各登记一次证据（都还没到门槛）
	if _, _, err := g.EnterScene(qq); err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.EnterScene(webui); err != nil {
		t.Fatal(err)
	}
	// 桶键 = sig.Label(2) 本身，**不带 "auto:" 前缀**（已用探针实测：
	// 登记出来的 label 是 "chan:qq" / "chan:webui"）。
	if evidenceCount(t, g, NormalizeSceneKey("chan:webui")) == 0 {
		t.Fatal("webui 桶的证据没登记上，前提不成立")
	}

	// qq 第 2 次 → 建出场景
	key, _, err := g.EnterScene(qq)
	if err != nil {
		t.Fatal(err)
	}
	if key == "" {
		t.Fatal("qq 第 2 次应建出场景")
	}

	// ★ 关键断言：qq 的证据被吸收了，webui 的必须还在
	if n := evidenceCount(t, g, NormalizeSceneKey("chan:webui")); n != 1 {
		t.Errorf("建 qq 场景时把 webui 桶的证据清了（剩 %d，应为 1）—— "+
			"DELETE FROM situation_evidence 是全表清，"+
			"别的场景的门槛计数被这次建键随机打断了", n)
	}
}

// 被吸收的应该是**本指纹**那个桶。
func TestSceneCreationClearsOwnEvidence(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	qq := mkSig("qq", "", "")
	webui := mkSig("webui", "", "")

	g.EnterScene(qq)
	g.EnterScene(webui)
	qqLabel := NormalizeSceneKey(qq.Label(2)) // 桶键不带 auto: 前缀
	if evidenceCount(t, g, qqLabel) == 0 {
		t.Fatalf("前提不成立：%q 桶应有证据", qqLabel)
	}

	if _, _, err := g.EnterScene(qq); err != nil {
		t.Fatal(err)
	}

	if n := evidenceCount(t, g, qqLabel); n != 0 {
		t.Errorf("场景已成立，%q 桶的证据应被吸收（该场面已长出场景），实际仍为 %d", qqLabel, n)
	}
}

// 反复建多个场景，早期桶的证据必须能活到自己的门槛。
// 这是 R5 的真实后果形态：三个通道轮流入，每个都只来过一次，
// 全表清会让它们**永远**攒不到 2 次。
func TestEvidenceSurvivesOtherSceneCreations(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	sigs := map[string]Situation{
		"qq":    mkSig("qq", "", ""),
		"mc":    mkSig("mc", "", ""),
		"cli":   mkSig("cli", "", ""),
		"acp":   mkSig("acp", "", ""),
		"http":  mkSig("http", "", ""),
		"timer": mkSig("timer", "", ""),
	}

	// 每个通道轮流来 3 次。若全表清，后到的通道永远攒不够。
	for round := 0; round < 3; round++ {
		for _, sig := range sigs {
			if _, _, err := g.EnterScene(sig); err != nil {
				t.Fatal(err)
			}
		}
	}

	// 6 个通道都该长出场景
	if n := len(sceneKeys(t, g)); n != len(sigs) {
		t.Errorf("6 个通道各来 3 次，应长出 %d 个场景，实际 %d: %v\n"+
			"全表清证据会让除第一个之外的通道永远攒不到 minSceneEvidence",
			len(sigs), n, sceneKeys(t, g))
	}
}
