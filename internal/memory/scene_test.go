package memory

import (
	"os"
	"strings"
	"testing"
)

func TestNormalizeSceneKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"chan:qq", "chan:qq"},
		{"chan:QQ", "chan:qq"},
		{"  chan:qq  ", "chan:qq"},
		{"chan:qq/peer:group_1027993713", "chan:qq/peer:group_1027993713"},
		{"chan:qq /  peer:1", "chan:qq/peer:1"},
		{"chan:qq/", "chan:qq"},
		{"chan:qq///peer:1", "chan:qq/peer:1"},
		{"老大2026-09-04 12:27 QQ私聊图片", "老大2026-09-04_12:27_qq私聊图片"},
		{"", ""},
		{"   ", ""},
		{"///", ""},
	}
	for _, c := range cases {
		if got := NormalizeSceneKey(c.in); got != c.want {
			t.Errorf("NormalizeSceneKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// 超长键要截到上限且不留尾部分隔符
	long := strings.Repeat("a", maxSceneKeyLen+40)
	if got := NormalizeSceneKey(long); len(got) > maxSceneKeyLen {
		t.Errorf("超长键未截断: %d", len(got))
	}
	if strings.HasSuffix(NormalizeSceneKey(long+"/x"), "/") {
		t.Error("截断后不应留尾部分隔符")
	}
}

func TestChannelAndToolScene(t *testing.T) {
	if got := ChannelScene("qq"); got != "chan:qq" {
		t.Errorf("ChannelScene(qq) = %q", got)
	}
	if got := ChannelScene(""); got != "" {
		t.Errorf("空 source 应为空场景，得到 %q", got)
	}
	if got := ToolScene("qq_get_message"); got != "tool:qq_get_message" {
		t.Errorf("ToolScene = %q", got)
	}
}

// TestSceneRefsFromCommit 钉住「写入即挂场景」：三元组带 Scene 时，
// 关系与两端实体都进场景，且同一场景的其它记忆不受影响。
func TestSceneRefsFromCommit(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	if _, _, err := g.Commit([]Triple{
		{Subject: "老大", Relation: "偏好", Object: "QQ回复禁用Markdown格式", Confidence: 1.0, Scene: "chan:qq", SentenceText: "回QQ消息别用markdown"},
		{Subject: "小宅", Relation: "使用", Object: "CodeGraph", Confidence: 1.0},
	}, "main", 0); err != nil {
		t.Fatalf("commit: %v", err)
	}

	r, err := g.RecallByScene([]string{"chan:qq"}, 8)
	if err != nil {
		t.Fatalf("RecallByScene: %v", err)
	}
	if len(r.Relations) != 1 || r.Relations[0].RelationType != "偏好" {
		t.Fatalf("场景关系不对: %+v", r.Relations)
	}
	if r.Relations[0].SentenceText != "回QQ消息别用markdown" {
		t.Errorf("场景召回必须带原句，得到 %q", r.Relations[0].SentenceText)
	}
	names := map[string]bool{}
	for _, e := range r.Entities {
		names[e.Name] = true
	}
	if !names["老大"] || !names["QQ回复禁用Markdown格式"] {
		t.Errorf("两端实体都应进场景: %v", names)
	}
	if names["CodeGraph"] {
		t.Error("未标场景的三元组实体不该被卷进场景")
	}

	// 无关场景不命中
	if r2, _ := g.RecallByScene([]string{"chan:webui"}, 8); len(r2.Relations) != 0 {
		t.Errorf("chan:webui 不该命中: %+v", r2.Relations)
	}
}

// TestRecallByScenePrefix 钉住前缀语义：宽场景召回包含更窄的场景，
// 但不会把同前缀不同层的场景（chan:qq2）吞进来。
func TestRecallByScenePrefix(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	if _, _, err := g.Commit([]Triple{
		{Subject: "群规", Relation: "禁止", Object: "刷屏", Confidence: 1.0, Scene: "chan:qq/peer:group_1027993713"},
		{Subject: "别的", Relation: "是", Object: "无关", Confidence: 1.0, Scene: "chan:qq2"},
	}, "main", 0); err != nil {
		t.Fatalf("commit: %v", err)
	}

	r, err := g.RecallByScene([]string{"chan:qq"}, 8)
	if err != nil {
		t.Fatalf("RecallByScene: %v", err)
	}
	if len(r.Relations) != 1 || r.Relations[0].SourceName != "群规" {
		t.Fatalf("宽场景应取回窄场景的记忆: %+v", r.Relations)
	}
}

// TestRecallBySceneOrderAndLimit 钉住排序（weight 降序）与上限。
func TestRecallBySceneOrderAndLimit(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	if _, _, err := g.Commit([]Triple{
		{Subject: "低值", Relation: "置信", Object: "甲组", Confidence: 0.2, Scene: "chan:qq"},
		{Subject: "高值", Relation: "置信", Object: "乙组", Confidence: 0.9, Scene: "chan:qq"},
		{Subject: "中值", Relation: "置信", Object: "丙组", Confidence: 0.5, Scene: "chan:qq"},
	}, "main", 0); err != nil {
		t.Fatalf("commit: %v", err)
	}
	r, err := g.RecallByScene([]string{"chan:qq"}, 2)
	if err != nil {
		t.Fatalf("RecallByScene: %v", err)
	}
	if len(r.Relations) != 2 {
		t.Fatalf("limit 未生效: %d", len(r.Relations))
	}
	if r.Relations[0].SourceName != "高值" || r.Relations[1].SourceName != "中值" {
		t.Errorf("应按 weight 降序: %s, %s", r.Relations[0].SourceName, r.Relations[1].SourceName)
	}
}

// TestTagSceneByEntityGlob 覆盖存量引导（dry-run 不写库、apply 后才建引用）。
func TestTagSceneByEntityGlob(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	if _, _, err := g.Commit([]Triple{
		{Subject: "老大", Relation: "偏好", Object: "QQ回复禁用Markdown格式", Confidence: 1.0},
		{Subject: "小宅", Relation: "使用", Object: "CodeGraph", Confidence: 1.0},
	}, "main", 0); err != nil {
		t.Fatalf("commit: %v", err)
	}

	n, err := g.TagSceneByEntityGlob("chan:qq", "*QQ*", true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if n != 1 {
		t.Errorf("dry-run 命中 %d，want 1", n)
	}
	if r, _ := g.RecallByScene([]string{"chan:qq"}, 8); len(r.Relations) != 0 {
		t.Error("dry-run 不应写库")
	}

	n, err = g.TagSceneByEntityGlob("chan:qq", "*QQ*", false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if n != 1 {
		t.Errorf("apply 标注 %d，want 1", n)
	}
	r, _ := g.RecallByScene([]string{"chan:qq"}, 8)
	if len(r.Relations) != 1 || r.Relations[0].TargetName != "QQ回复禁用Markdown格式" {
		t.Errorf("标注后应能按场景取回: %+v", r.Relations)
	}
}

// TestPurgeNoiseClearsSceneRefs 钉住清理后不留悬空场景引用。
func TestPurgeNoiseClearsSceneRefs(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	if _, _, err := g.Commit([]Triple{
		{Subject: "结果", Relation: "是", Object: "问题", Confidence: 1.0, Scene: "chan:qq"},
		{Subject: "小宅", Relation: "使用", Object: "CodeGraph", Confidence: 1.0, Scene: "chan:qq"},
	}, "main", 0); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, _, err := g.PurgeNoise(false); err != nil {
		t.Fatalf("purge: %v", err)
	}

	var refs int
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM scene_refs sr
		WHERE (sr.kind='relation' AND sr.ref_id NOT IN (SELECT id FROM relations))
		   OR (sr.kind='entity' AND sr.ref_id NOT IN (SELECT id FROM entities))`).Scan(&refs); err != nil {
		t.Fatalf("count: %v", err)
	}
	if refs != 0 {
		t.Errorf("清理后仍有 %d 条悬空场景引用", refs)
	}
	// 干净的那条仍在场景里
	r, _ := g.RecallByScene([]string{"chan:qq"}, 8)
	if len(r.Relations) != 1 || r.Relations[0].TargetName != "CodeGraph" {
		t.Errorf("清理误伤场景记忆: %+v", r.Relations)
	}
}

func TestSceneStats(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	if _, _, err := g.Commit([]Triple{
		{Subject: "甲组", Relation: "是", Object: "乙组", Confidence: 1.0, Scene: "chan:qq"},
		{Subject: "丙组", Relation: "是", Object: "丁组", Confidence: 1.0, Scene: "chan:webui"},
	}, "main", 0); err != nil {
		t.Fatalf("commit: %v", err)
	}
	stats, err := g.SceneStats()
	if err != nil {
		t.Fatalf("SceneStats: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("场景数 %d，want 2: %+v", len(stats), stats)
	}
	for _, st := range stats {
		// 1 条关系 + 2 个实体
		if st.Refs != 3 || st.Relations != 1 || st.Entities != 2 {
			t.Errorf("场景 %s 统计不对: %+v", st.Key, st)
		}
	}
}

// TestBuildContextInScene 覆盖注入面：场景块要给到关系全文与原句，
// 且场景实体不在【记忆索引】里重复占位。
func TestBuildContextInScene(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	if _, _, err := g.Commit([]Triple{
		{Subject: "老大", Relation: "偏好", Object: "QQ回复禁用Markdown格式", Confidence: 1.0,
			Scene: "chan:qq", SentenceText: "以后回QQ消息不要用markdown"},
	}, "main", 0); err != nil {
		t.Fatalf("commit: %v", err)
	}
	idx := NewIndexer(g)
	if err := idx.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// 措辞与记忆零字面重合，词法/向量路召不回；场景路必须兜住。
	ctx := idx.BuildContextInScene("在吗", []string{"chan:qq"})
	text := idx.FormatContext(ctx)
	if !strings.Contains(text, "【场景记忆 chan:qq】") {
		t.Fatalf("没有场景块: %q", text)
	}
	if !strings.Contains(text, "老大 --偏好--> QQ回复禁用Markdown格式") {
		t.Errorf("场景块里没有关系全文: %q", text)
	}
	if !strings.Contains(text, "以后回QQ消息不要用markdown") {
		t.Errorf("场景块里没有原句: %q", text)
	}
	if strings.Contains(text, "【记忆索引】") && strings.Contains(text, "索引: 老大") {
		t.Errorf("场景实体不该在索引里重复占位: %q", text)
	}

	// 无场景时行为与原来一致：不出现场景块
	plain := idx.FormatContext(idx.BuildContextInScene("在吗", nil))
	if strings.Contains(plain, "【场景记忆") {
		t.Errorf("无场景却出现场景块: %q", plain)
	}
}
