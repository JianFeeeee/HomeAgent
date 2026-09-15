package memory

import (
	"os"
	"strings"
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

	scenes, err := g.SceneStats()
	if err != nil {
		t.Fatalf("SceneStats: %v", err)
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
//
// 半衰期的语义是「每个引用至多每 halfLife 衰减一次」：衰减计时起点在
// scene_refs.decayed_at 上，同一半衰期内重复跑心跳不会再砍。
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

	// 把时间推旧：模拟长期未重现（上次衰减也在同一时刻）。用 SQLite 的
	// datetime('now') 与 CURRENT_TIMESTAMP 同一时间基准（UTC）。
	if _, err := g.db.Exec(`UPDATE scene_refs
		SET created_at = datetime('now','-48 hours'), decayed_at = datetime('now','-48 hours')`); err != nil {
		t.Fatal(err)
	}
	if _, err := g.db.Exec(`UPDATE scenes SET updated_at = datetime('now','-48 hours')`); err != nil {
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

	// 同一半衰期内再跑：decayed_at 已刷新，不该再砍一次
	// （否则心跳频率就成了实际半衰期，30 天的关联几小时就被清空）
	if _, err := g.DecaySceneRefs(time.Hour, 0.4); err != nil {
		t.Fatalf("DecaySceneRefs: %v", err)
	}
	var same float64
	if err := g.db.QueryRow(`SELECT weight FROM scene_refs LIMIT 1`).Scan(&same); err != nil {
		t.Fatal(err)
	}
	if same != after {
		t.Errorf("同一半衰期内不该重复衰减: %v → %v", after, same)
	}

	// 进入第二个半衰期：再推旧 decayed_at，权重低于 floor，清掉
	if _, err := g.db.Exec(`UPDATE scene_refs SET decayed_at = datetime('now','-48 hours')`); err != nil {
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

// TestDeclaredAndEmergentBothLearn 钉住「主动 + 被动两条路」的相互长进：
//   - 主动：声明即建场景（不等第二次涌现），特征只从键自身解析
//   - 被动：指纹聚类自己长出场景；声明场景**不**进相似度空间，靠声明/前缀键取回
//   - 第一次交互（涌现场景还没长出来）由声明场景兜底
func TestDeclaredAndEmergentBothLearn(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	// 第 1 轮：声明了 chan:qq。此刻还没有涌现场景 → Primary 必须兜底到声明场景
	turn, err := g.EnterSceneWithHint(mkSig("qq", "group_1027", "qq_get_message", "排班"), []string{"chan:qq"})
	if err != nil {
		t.Fatalf("EnterSceneWithHint: %v", err)
	}
	if turn.Primary != "chan:qq" {
		t.Fatalf("首次交互应兜底到声明场景，得到 %q", turn.Primary)
	}
	if turn.Emergent {
		t.Error("首次交互不该有涌现场景")
	}
	if len(turn.DeclaredCreated) != 1 || turn.DeclaredCreated[0] != "chan:qq" {
		t.Errorf("声明即建场景（不等第二次涌现）: %+v", turn.DeclaredCreated)
	}

	// 第 2 轮同类场面：涌现场景长出来，且它比声明场景**更优先**用于写入
	turn2, err := g.EnterSceneWithHint(mkSig("qq", "group_1027", "qq_get_message", "排班表"), []string{"chan:qq"})
	if err != nil {
		t.Fatalf("EnterSceneWithHint: %v", err)
	}
	if !turn2.Emergent || !strings.HasPrefix(turn2.Primary, "auto:") {
		t.Fatalf("第 2 轮应涌现出细粒度场景并优先: %+v", turn2)
	}
	if len(turn2.Keys) < 2 {
		t.Fatalf("两条路都要进召回集合: %+v", turn2.Keys)
	}

	// 在声明场景里写上一条（模拟首次交互时写的记忆）
	if _, _, err := g.Commit([]Triple{{
		Subject: "群规", Relation: "禁止", Object: "Markdown排版", Confidence: 1.0,
		Scenes: []string{"chan:qq"},
	}}, "main", 0); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// 在涌现场景里写上一条
	if _, _, err := g.Commit([]Triple{{
		Subject: "排班表", Relation: "格式", Object: "纯文本", Confidence: 1.0,
		Scenes: []string{turn2.Primary},
	}}, "main", 0); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// **没人声明**的同场面提问：被动路按相似度命中涌现场景；
	// 声明场景不在相似度空间里（否则它会吃掉被动路），靠声明/前缀路命中。
	sig := mkSig("qq", "group_1027", "qq_get_message", "统计")
	r, err := g.RecallBySituation(sig, 8)
	if err != nil {
		t.Fatalf("RecallBySituation: %v", err)
	}
	got := map[string]bool{}
	for _, rel := range r.Relations {
		got[rel.TargetName] = true
	}
	if !got["纯文本"] {
		t.Errorf("涌现场景应被被动命中: %+v", r.Relations)
	}
	if got["Markdown排版"] {
		t.Errorf("声明场景不该进相似度空间（会压死被动路）: %+v", r.Relations)
	}
	// 声明场景走声明键，照样取回
	byKey, err := g.RecallByScene([]string{"chan:qq"}, 8)
	if err != nil {
		t.Fatalf("RecallByScene: %v", err)
	}
	if len(byKey.Relations) != 1 || byKey.Relations[0].TargetName != "Markdown排版" {
		t.Errorf("声明路应取回声明场景的记忆: %+v", byKey.Relations)
	}
	// 而完整的一轮（声明+涌现）两条路都进召回集合
	full, err := g.EnterSceneWithHint(mkSig("qq", "group_1027", "qq_get_message", "统计"), []string{"chan:qq"})
	if err != nil {
		t.Fatalf("EnterSceneWithHint: %v", err)
	}
	if len(full.Keys) < 2 {
		t.Errorf("两条路都应进召回集合: %+v", full.Keys)
	}
	// 声明键从自身解析特征（不含整轮指纹）
	feats := FeaturesFromSceneKey("chan:qq/peer:group_1027")
	if len(feats) != 2 || feats[0].Key() != "chan:qq" || feats[1].Key() != "peer:group_1027" {
		t.Errorf("声明键特征解析失败: %+v", feats)
	}
	if len(FeaturesFromSceneKey("老大2026-09-04_12:27_qq私聊图片")) != 0 {
		t.Error("无 kind:value 结构的键不该硬猜特征")
	}

	// 声明场景不该被覆盖成涌现键：两者的身份各自保留
	if _, err := g.EnsureScene("chan:qq"); err != nil {
		t.Fatalf("EnsureScene: %v", err)
	}
	var n int
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM scenes WHERE key = 'chan:qq'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("声明场景应保持独立存在，得到 %d", n)
	}
}

// TestPeerGroupWeight 钉住群身份是**强**同一性信号：peer_group 与 peer 同为
// wFeatPeer。漏掉 peer_group 会让它在 Weight 里落到 default（话题级 0.4），
// 群聊场面被降级成软信号。
func TestPeerGroupWeight(t *testing.T) {
	if got := (SituationFeature{Kind: "peer_group", Value: "group_1"}).Weight(); got != wFeatPeer {
		t.Errorf("peer_group 权重应为 %v，得到 %v", wFeatPeer, got)
	}
	if got := (SituationFeature{Kind: "peer", Value: "user_1"}).Weight(); got != wFeatPeer {
		t.Errorf("peer 权重应为 %v，得到 %v", wFeatPeer, got)
	}
}

// TestEffectiveScenes 覆盖「单值声明 + 多值」合并去重。
func TestEffectiveScenes(t *testing.T) {
	got := effectiveScenes(Triple{Scene: "chan:qq", Scenes: []string{"auto:a", "chan:qq", ""}})
	if len(got) != 2 || got[0] != "auto:a" || got[1] != "chan:qq" {
		t.Errorf("合并去重保序失败: %v", got)
	}
	if effectiveScenes(Triple{}) != nil {
		t.Error("无场景应返回 nil")
	}
}
