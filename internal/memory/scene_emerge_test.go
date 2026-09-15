package memory

import (
	"os"
	"testing"
	"time"
)

func mkSig(chanName, peer, tool string, topics ...string) Situation {
	feats := []SituationFeature{{Kind: "chan", Value: chanName}}
	if peer != "" {
		feats = append(feats, SituationFeature{Kind: "peer", Value: peer})
	}
	if tool != "" {
		feats = append(feats, SituationFeature{Kind: "tool", Value: tool})
	}
	feats = append(feats, SituationFeature{Kind: "part", Value: "morning"})
	for _, t := range topics {
		feats = append(feats, SituationFeature{Kind: "topic", Value: t})
	}
	return NewSituation(feats...)
}

// TestSceneEmergesFromRepetition 是这套机制的核心证据：
// 场景**没有人声明过**——同类场面重复出现时自己长出来。
func TestSceneEmergesFromRepetition(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	// 第 1 次：只登记足迹，不建场景（一次性的交互不是「场面」）
	key, created, err := g.EnterScene(mkSig("qq", "group_1027", "qq_get_message", "排班"))
	if err != nil {
		t.Fatalf("EnterScene: %v", err)
	}
	if key != "" || created {
		t.Fatalf("首次出现不该长出场景，得到 key=%q created=%v", key, created)
	}

	// 第 2 次同类场面：场景长出来了
	key, created, err = g.EnterScene(mkSig("qq", "group_1027", "qq_get_message", "排班表"))
	if err != nil {
		t.Fatalf("EnterScene: %v", err)
	}
	if key == "" || !created {
		t.Fatalf("第 2 次同类场面应长出场景，得到 key=%q created=%v", key, created)
	}
	emergentKey := key

	// 第 3 次：同样的场面、**不同的话题**——仍属于同一个场景（是场景，不是每轮一个键）
	key3, created3, err := g.EnterScene(mkSig("qq", "group_1027", "qq_get_message", "发版"))
	if err != nil {
		t.Fatalf("EnterScene: %v", err)
	}
	if created3 || key3 != emergentKey {
		t.Fatalf("同场面不同话题应并入既有场景: key=%q created=%v want=%q", key3, created3, emergentKey)
	}

	// 另一个场面（换通道）不会被并进去，重复两次后自己长出一个
	if k, c, _ := g.EnterScene(mkSig("webui", "", "", "排班")); k != "" && !c {
		t.Fatalf("不同通道不应并进 QQ 场景: %q", k)
	}
	if k2, c2, _ := g.EnterScene(mkSig("webui", "", "", "排班")); k2 == "" || !c2 || k2 == emergentKey {
		t.Fatalf("webui 场面应自己长出独立场景: key=%q created=%v", k2, c2)
	}

	scenes, err := g.EmergentScenes()
	if err != nil {
		t.Fatalf("EmergentScenes: %v", err)
	}
	if len(scenes) != 2 {
		t.Fatalf("应长出 2 个场景，得到 %d: %+v", len(scenes), scenes)
	}
	// 强化：QQ 场景被遇到 3 次（2 次缔造 + 1 次并入）→ strength > 1
	var qq SceneStat
	for _, sc := range scenes {
		if sc.Key == emergentKey {
			qq = sc
		}
	}
	if qq.Strength < 2 {
		t.Errorf("场景强度应随重现增加，得到 %d", qq.Strength)
	}
	if qq.Features < 3 {
		t.Errorf("场景应记住多个特征，得到 %d", qq.Features)
	}
}

// TestSceneRecallsBySituationNotWording 钉住「类似场面自动唤起记忆」：
// 唤起靠场面相似，而不是措辞命中。
func TestSceneRecallsBySituationNotWording(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	// 让 QQ 场面长出场景
	g.EnterScene(mkSig("qq", "group_1027", "qq_get_message", "排班"))
	sceneKey, _, err := g.EnterScene(mkSig("qq", "group_1027", "qq_get_message", "排班表"))
	if err != nil || sceneKey == "" {
		t.Fatalf("场景未长出: %q %v", sceneKey, err)
	}

	// 在这个场面里写下的规则（与后面提问的措辞零重合）
	if _, _, err := g.Commit([]Triple{
		{Subject: "群规", Relation: "禁止", Object: "Markdown排版", Confidence: 1.0,
			Scene: sceneKey, SentenceText: "本群只发纯文本"},
	}, "main", 0); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 换一种措辞、但同一个场面：应当自动唤起上面那条
	r, err := g.RecallBySituation(mkSig("qq", "group_1027", "qq_get_message", "统计"), 8)
	if err != nil {
		t.Fatalf("RecallBySituation: %v", err)
	}
	if len(r.Relations) != 1 || r.Relations[0].TargetName != "Markdown排版" {
		t.Fatalf("同场面应唤起记忆: %+v", r.Relations)
	}
	if r.Relations[0].SentenceText != "本群只发纯文本" {
		t.Errorf("唤起时要带原句: %+v", r.Relations[0])
	}

	// 别的场面不该被唤起
	other, err := g.RecallBySituation(mkSig("webui", "", "", "统计"), 8)
	if err != nil {
		t.Fatalf("RecallBySituation: %v", err)
	}
	if len(other.Relations) != 0 {
		t.Errorf("无关场面不该唤起: %+v", other.Relations)
	}
}

// TestSceneRefDecay 钉住「用进废退」：久未重现的关联会淡出并被清掉。
func TestSceneRefDecay(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	if _, _, err := g.Commit([]Triple{
		{Subject: "甲组", Relation: "是", Object: "乙组", Confidence: 1.0, Scene: "chan:qq"},
	}, "main", 0); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var before float64
	if err := g.db.QueryRow(`SELECT weight FROM scene_refs LIMIT 1`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	// 把时间推旧：模拟长期未重现
	if _, err := g.db.Exec(`UPDATE scene_refs SET created_at = ?`,
		time.Now().Add(-48*time.Hour).Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.db.Exec(`UPDATE scenes SET updated_at = ?`,
		time.Now().Add(-48*time.Hour).Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}

	// 一个半衰期：权重减半（1.0 → 0.5），仍高于 floor，保留
	if _, err := g.DecaySceneRefs(time.Hour, 0.4); err != nil {
		t.Fatalf("DecaySceneRefs: %v", err)
	}
	var after float64
	if err := g.db.QueryRow(`SELECT weight FROM scene_refs LIMIT 1`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after >= before {
		t.Errorf("一个半衰期后权重应减半: %v → %v", before, after)
	}

	// 再推旧一次：第二个半衰期后低于 floor，关联已无信息量，清掉
	if _, err := g.db.Exec(`UPDATE scene_refs SET created_at = ?`,
		time.Now().Add(-48*time.Hour).Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}
	n, err := g.DecaySceneRefs(time.Hour, 0.4)
	if err != nil {
		t.Fatalf("DecaySceneRefs: %v", err)
	}
	if n != 3 {
		t.Errorf("三个低权重引用应被清掉，得到 %d", n)
	}
	var left int
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM scene_refs`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("衰减后不该剩下引用，得到 %d", left)
	}

	// 仍在重现的场景不受影响（updated_at 新）
	if _, _, err := g.Commit([]Triple{
		{Subject: "丙组", Relation: "是", Object: "丁组", Confidence: 1.0, Scene: "chan:webui"},
	}, "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := g.DecaySceneRefs(time.Hour, 0.4); err != nil {
		t.Fatal(err)
	}
	if left, _ := g.RecallByScene([]string{"chan:webui"}, 8); len(left.Relations) != 1 {
		t.Errorf("刚用过的场景不该被衰减掉: %+v", left.Relations)
	}
}
