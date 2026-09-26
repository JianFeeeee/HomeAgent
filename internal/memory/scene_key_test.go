package memory

// 场景键唯一性的判据（修复 R1/R2 的红测试）。
//
// 背景（生产实测）：scenes.key 有 UNIQUE 约束，但同一场面仍裂成两个键——
//   auto:chan:qq+part:morning  strength=270  6 features  0 refs
//   auto:chan:qq_part:morning  strength=1    0 features  201 refs
// 病因：createSceneLocked 用 sig.Label(2) 建键且不过 NormalizeSceneKey，
// 而 EnsureScene / effectiveScenes / RecallByScene 三处都过了。
//
// 本组测试的判据在**生产代码之外**：期望值是「同一场面 ⇒ 同一个键」这条
// 不变量，不引用任何被测实现细节。判据自身也做了双向检查：
// 先用「改实现 ⇒ 必须变红」验证过它真的在跑。

import (
	"os"
	"strings"
	"testing"
)

// sigWithPart 只有 chan + part 两个特征（part 权重 0.2，是最弱维度）。
func sigWithPart(chanName, part string) Situation {
	return NewSituation(
		SituationFeature{Kind: "chan", Value: chanName},
		SituationFeature{Kind: "part", Value: part},
	)
}

// R1：同一指纹反复出现，键必须唯一，不能每次都造新行。
func TestSceneKeyIsUniqueForSameSituation(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	sig := mkSig("qq", "", "")

	// 连喂 6 次。minSceneEvidence=2 ⇒ 第 2 次建场景、之后只强化。
	// 要断言的**不是**哪一次 created，而是：全程只允许存在一个键。
	keys := map[string]int{}
	for i := 0; i < 6; i++ {
		key, _, err := g.EnterScene(sig)
		if err != nil {
			t.Fatalf("第 %d 次 EnterScene 出错: %v", i+1, err)
		}
		if key == "" {
			continue
		}
		keys[key]++
	}

	if len(keys) == 0 {
		t.Fatal("6 次重复交互后仍未建出场景，与 minSceneEvidence=2 的设计矛盾")
	}
	if len(keys) > 1 {
		t.Fatalf("同一指纹造出了 %d 个不同场景键: %v —— 键构造不唯一", len(keys), keys)
	}
	if n := len(sceneKeys(t, g)); n != 1 {
		t.Fatalf("scenes 表里有 %d 行，应为 1（重复交互不得增殖场景行）", n)
	}
}

// 门槛本身：第 1 次只留足迹、第 2 次建场景。这是 minSceneEvidence 的语义，
// 单独钉住，免得修键时把门槛一起改掉。
func TestSceneEvidenceThresholdIsTwo(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	sig := mkSig("qq", "", "")

	if key, _, err := g.EnterScene(sig); err != nil {
		t.Fatal(err)
	} else if key != "" {
		t.Fatalf("第 1 次就建了场景 %q，minSceneEvidence=2 失效", key)
	}

	key, created, err := g.EnterScene(sig)
	if err != nil {
		t.Fatal(err)
	}
	if key == "" || !created {
		t.Fatalf("第 2 次应建出新场景（key=%q created=%v）", key, created)
	}

	// 第 3 次起只强化，不再新建
	if _, created, err := g.EnterScene(sig); err != nil {
		t.Fatal(err)
	} else if created {
		t.Fatal("第 3 次不该再新建场景")
	}
}

// R1（生产现场形态）：带 + 的键与带 _ 的键必须归一到同一个。
// 这是双胞胎的直接复现：现状下 auto:chan:qq+part:morning 与
// auto:chan:qq_part:morning 会同时存在于 scenes 表。
func TestSceneKeyNormalized_NoPlusVersusUnderscore(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	// 造两次：强特征相同、只有 part 不同（现实中「早上在 QQ」与「晚上在 QQ」）
	for _, part := range []string{"morning", "evening"} {
		sig := sigWithPart("qq", part)
		for i := 0; i < 3; i++ {
			if _, _, err := g.EnterScene(sig); err != nil {
				t.Fatalf("EnterScene(%s) 出错: %v", part, err)
			}
		}
	}

	keys := sceneKeys(t, g)
	if len(keys) == 0 {
		t.Fatal("未建出任何场景")
	}
	for _, k := range keys {
		if strings.Contains(k, "+") {
			t.Errorf("场景键 %q 含未归一化的 '+'；其余三条路径（EnsureScene/"+
				"effectiveScenes/RecallByScene）都用 NormalizeSceneKey，"+
				"只有建键路径漏了 ⇒ 写侧永远匹配不上", k)
		}
	}
}

// R2：part（权重 0.2，最弱维度）不该成为场景身份的一部分。
func TestSceneKeyExcludesWeakPartFeature(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	// 只有 chan 一个强特征，part 必然挤进 Label(2) 的第二位。
	for i := 0; i < 3; i++ {
		if _, _, err := g.EnterScene(sigWithPart("qq", "morning")); err != nil {
			t.Fatal(err)
		}
	}

	keys := sceneKeys(t, g)
	if len(keys) == 0 {
		t.Fatal("未建出场景")
	}
	for _, k := range keys {
		if strings.Contains(k, "part") {
			t.Errorf("场景键 %q 把时段(part, 权重 0.2)写进了身份。"+
				"时段是最弱维度：生产库实测出现「morning」场景吞掉 evening 指纹"+
				"（共享 chan:qq，相似度 1.0/1.4=0.714 > 阈值 0.5）", k)
		}
	}
}

// R2 的另一半：part 不该影响「是否建场景」的门槛桶。
// 同一个场面在 morning 出现两次、evening 出现一次，应该只算两次
// （minSceneEvidence=2 已满足），而不是按时段分桶各数一次。
func TestEvidenceBucketIgnoresPart(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	// 第一轮 morning：只登记足迹，不建场景
	if key, _, err := g.EnterScene(sigWithPart("qq", "morning")); err != nil {
		t.Fatal(err)
	} else if key != "" {
		t.Fatalf("首次不该建场景，却建了 %q", key)
	}

	// 第二轮换成 evening：若门槛按 part 分桶，这里就又要再等一次
	key, _, err := g.EnterScene(sigWithPart("qq", "evening"))
	if err != nil {
		t.Fatal(err)
	}
	if key == "" {
		t.Fatal("同一场面（只差时段）第 2 次仍未建场景 —— 时段把证据桶拆开了")
	}
}

// R1 完整复现：模拟生产库里「写入侧归一化、建键侧不归一化」的分裂。
// 断言：写侧挂的记忆，最终能通过读侧召回回到同一个场景。
func TestWrittenRefReachableFromItsScene(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	sig := mkSig("qq", "", "")
	var key string
	for i := 0; i < 3 && key == ""; i++ {
		k, _, err := g.EnterScene(sig)
		if err != nil {
			t.Fatal(err)
		}
		key = k
	}
	if key == "" {
		t.Fatal("未建出场景")
	}

	// 写侧走 effectiveScenes（它会归一化）——这正是生产代码的路径
	ec, rc, err := g.Commit([]Triple{{
		Subject: "老大", Relation: "偏好", Object: "咖啡",
		Scenes: []string{key}, // 模型/内核给的键，可能带 + 或 _
	}}, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ec == 0 || rc == 0 {
		t.Fatal("三元组未写入")
	}

	// 读侧也走归一化（RecallByScene 内部会做）
	res, err := g.RecallByScene([]string{key}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Relations) == 0 {
		t.Fatalf("刚写进场景 %q 的关系，经同键召回却取不回 —— "+
			"建键与写/读两侧对「同一个键」的认定不一致", key)
	}
}

// sceneKeys 直查 scenes 表（不引用被测统计函数，独立复算）。
func sceneKeys(t *testing.T, g *GraphDB) []string {
	t.Helper()
	rows, err := g.db.Query(`SELECT key FROM scenes ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
