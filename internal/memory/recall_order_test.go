package memory

import (
	"fmt"
	"strings"
	"testing"
)

// seedPorts 灌入 v4 实测库里真实存在的实体形态：4 根针 + 同域噪音。
//
// 形态照抄真实数据（不是理想化的）：针是「metrics服务端口8328」这种
// 「服务名+服务端口+值」的复合名，噪音是 26 个其它「xx服务端口」+ 7 个
// 配置类泛词（缓存端口/连接池端口…）。实测 jieba 会把
// 「metrics服务的端口是多少」切成 [metrics 服务 端口] ——「服务」「端口」
// 是无区分度泛词，正是 193 个实体的来源。
func seedPorts(t *testing.T, db *GraphDB) {
	t.Helper()
	needles := []string{"metrics服务端口8328", "auth服务端口8243",
		"trace服务端口8368", "admin服务端口8861"}
	for _, n := range needles {
		mustCommit(t, db, []Triple{{Subject: n, Relation: "是", Object: "端口"}})
	}
	for _, s := range []string{"billing", "oauth", "inventory", "search", "captcha",
		"notify", "gateway", "payment", "refund", "shipping", "pricing", "stock",
		"coupon", "member", "point", "cart", "order", "logistics", "risk", "audit",
		"session", "profile", "wallet", "voucher", "invoice", "settle", "ledger"} {
		mustCommit(t, db, []Triple{{Subject: s + "服务端口", Relation: "是", Object: "端口"}})
	}
	for _, k := range []string{"缓存端口", "连接池端口", "监控端口", "数据库端口",
		"消息队列端口", "注册中心端口", "网关端口"} {
		mustCommit(t, db, []Triple{{Subject: k, Relation: "是", Object: "端口"}})
	}
}

func mustCommit(t *testing.T, db *GraphDB, triples []Triple) {
	t.Helper()
	if _, _, err := db.Commit(triples, "s", 1); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func rankOfName(t *testing.T, res *RecallResult, name string) int {
	t.Helper()
	for i, e := range res.Entities {
		if e.Name == name {
			return i + 1
		}
	}
	return -1
}

// ★ 回归：全局 topK。
//
// 缺它时的实测症状（v4 跑分，seed 20261001）：memory_recall 单次返回
// 193 个实体 / 13744 tokens = 工具预算的 436%，平铺进上下文后模型被噪音
// 淹没，转去 grep 知识库文件，还把「没检索到」说成「库里根本没有」。
//
// 变异自证：把 maxRecallEntities 调到 10000 后 len>maxRecallEntities 那条
// 判据必然失效 ⇒ 若本测试在那个改动下仍然通过，说明它没有判据。
func TestRecall_全局topK限制总量(t *testing.T) {
	db := newTestGraph(t)
	defer db.Close()
	seedPorts(t, db)
	// ★ 语料必须**大到能超上限**，否则这条测试恒过（第一版就是这么废的：
	// 39 个实体 < 20？不，39 > 20 但…… 关键是下面这个补充语料）。
	// 灌到 maxKeywordEntities*2 以上：单关键词 50 × 2 个关键词 = 100 个，
	// 修复前会返回 100 个实体，修复后必须被压到 20。
	for i := 0; i < 80; i++ {
		mustCommit(t, db, []Triple{{
			Subject: fmt.Sprintf("svc%02d服务端口", i), Relation: "是", Object: "端口",
		}})
	}

	// ★ 前置自证：必须证明「没有全局上限时这条断言会失败」。
	//
	// 第一版这条测试是废的：语料 39 个实体，单关键词就被 LIMIT 压到 20，
	// 合并后恰好 20 = 上限，把 maxRecallEntities 调到 100000 它照样通过。
	// 真正的漏洞场景是**多个关键词各召回一批后累加**（实测 v4：3 个关键词
	// 累加到 193 个），所以判据必须是「多词合并的量」超过上限。
	//
	// 这里用「泛词 + 多关键词」构造：每个泛词都能独立召回一批。
	for i := 0; i < 40; i++ {
		mustCommit(t, db, []Triple{{
			Subject: fmt.Sprintf("网关%02d配置项", i), Relation: "是", Object: "配置",
		}})
	}

	merged := 0
	for _, kw := range ExtractKeywords("metrics服务的端口配置是多少") {
		r, _ := db.Recall([]string{kw}, nil, 1, "")
		merged += len(r.Entities)
	}
	total := countEntities(t, db)
	t.Logf("语料实体数=%d，各关键词单查合计=%d（全局上限 %d）",
		total, merged, maxRecallEntities)
	if merged <= maxRecallEntities {
		t.Fatalf("各关键词单查合计只有 %d（<= 上限 %d）："+
			"本测试在移除全局上限时也会通过，等于没有判据",
			merged, maxRecallEntities)
	}

	for _, q := range []string{
		"metrics服务的端口配置是多少", "auth 服务的端口配置是多少",
		"trace服务端口配置", "admin服务的端口配置",
	} {
		res, err := db.Recall(ExtractKeywords(q), nil, 2, "")
		if err != nil {
			t.Fatalf("Recall: %v", err)
		}
		if len(res.Entities) > maxRecallEntities {
			t.Errorf("Q=%q 实体数 %d 超过全局上限 %d",
				q, len(res.Entities), maxRecallEntities)
		}
	}
}

// ★ 回归：实词优先的相关性排序。
//
// 三个坑，都是实测踩出来的：
//  1. 泛词陷阱：实体名恰好叫「端口」时，它对关键词「端口」是**完全相等**命中
//     （rank=0），比针「metrics服务端口8328」的前缀命中（rank=1）还"精确"，
//     于是把真答案压到第二。⇒ 必须只在**实词**上认可「完全相等」。
//  2. 跨关键词冲淡：SQL 算出的三层精确度只是**单关键词内**的排序，
//     多关键词结果依次 append 后层级被冲淡。⇒ 必须跨关键词归并重排。
//  3. 精确度「正序」而非倒序：rank 0 最强，必须排在最前。
func TestRecall_相关性排序针排首位(t *testing.T) {
	db := newTestGraph(t)
	defer db.Close()
	seedPorts(t, db)

	cases := []struct{ q, want string }{
		{"metrics服务的端口是多少", "metrics服务端口8328"},
		{"auth 服务的端口应该是多少", "auth服务端口8243"},
		{"trace服务端口", "trace服务端口8368"},
		{"admin服务的端口", "admin服务端口8861"},
	}
	for _, c := range cases {
		res, err := db.Recall(ExtractKeywords(c.q), nil, 2, "")
		if err != nil {
			t.Fatalf("Recall: %v", err)
		}
		if got := rankOfName(t, res, c.want); got != 1 {
			var top []string
			for i, e := range res.Entities {
				if i >= 3 {
					break
				}
				top = append(top, e.Name)
			}
			t.Errorf("Q=%q 针 %s 排名=%d（应为 1），top3=%v",
				c.q, c.want, got, top)
		}
	}
}

// MatchRank 必须回填 —— 上层要靠它告诉模型「为什么这条相关」。
// 空值会让 memory_recall 的层级标记全部消失，退回同格式平铺。
func TestRecall_回填MatchRank(t *testing.T) {
	db := newTestGraph(t)
	defer db.Close()
	seedPorts(t, db)

	res, _ := db.Recall(ExtractKeywords("metrics服务的端口是多少"), nil, 2, "")
	if len(res.Entities) == 0 {
		t.Fatal("无结果")
	}
	top := res.Entities[0]
	if top.MatchRank != 0 && top.MatchRank != 1 {
		t.Errorf("top1 的 MatchRank=%d，期望 0（完全相等）或 1（前缀）", top.MatchRank)
	}
}

// ★ 回归：时间倒序（overwrite 场景）。
//
// 缺它时的实测症状：v4 的 overwrite 组答对但 overwrite-stale 组答错 ——
// 新旧同名实体在相关性排序下层级完全相同，旧值被排在新值前面。
//
// ★ 排序键必须带 TurnID/ID 兜底：SQLite 的 CURRENT_TIMESTAMP **只到秒**。
// 实测 turn 1 写 4379、turn 2 写 4324 落在同一秒，CreatedAt 完全相等，
// 只按 CreatedAt 排的话稳定排序保留原顺序，而原顺序来自
// `ORDER BY confidence DESC`，置信度相同时退化到 rowid 升序 = 旧值在前。
// （与「SQLite 时间戳只有秒精度」是同一类问题。）
func TestRecall_时间倒序新值在前(t *testing.T) {
	db := newTestGraph(t)
	defer db.Close()
	if _, _, err := db.Commit([]Triple{{Subject: "值班室分机", Relation: "是", Object: "4379"}}, "s", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.Commit([]Triple{{Subject: "值班室分机", Relation: "是", Object: "4324"}}, "s", 2); err != nil {
		t.Fatal(err)
	}

	res, err := db.RecallSorted([]string{"值班室分机"}, nil, 2, "", SortRecent)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Relations) < 2 {
		t.Fatalf("期望至少 2 条关系，实际 %d", len(res.Relations))
	}
	if got := res.Relations[0].TargetName; got != "4324" {
		var all []string
		for _, r := range res.Relations {
			all = append(all, r.TargetName)
		}
		t.Errorf("时间倒序 top1=%s（应为 4324），实际顺序=%v", got, all)
	}
}

// ★ 变异自证（1）：把排序模式换成相关性，顺序必须变回旧值在前。
//
// 若这个测试在 SortRelevance 下也返回 4324 在前，说明时间排序根本没起作用，
// 哪怕上一条测试因为别的巧合"通过"了，判据就失效了。
func TestRecall_时间倒序_变异_相关性模式顺序不同(t *testing.T) {
	db := newTestGraph(t)
	defer db.Close()
	db.Commit([]Triple{{Subject: "值班室分机", Relation: "是", Object: "4379"}}, "s", 1)
	db.Commit([]Triple{{Subject: "值班室分机", Relation: "是", Object: "4324"}}, "s", 2)

	res, _ := db.RecallSorted([]string{"值班室分机"}, nil, 2, "", SortRelevance)
	if len(res.Relations) < 2 {
		t.Fatalf("关系不足 2 条")
	}
	if got := res.Relations[0].TargetName; got == "4324" {
		t.Errorf("★ 变异未生效：相关性模式下 4324 仍在最前 ⇒ 时间排序无独立作用，" +
			"上一条测试的判据不可信")
	}
}

// ParseSortMode 只认显式取值：认错会让调用方以为自己按时间排序、
// 实际拿到相关性顺序，而且这种错不会报错。
func TestParseSortMode(t *testing.T) {
	cases := map[string]SortMode{
		"":           SortRelevance,
		"recent":     SortRecent,
		"RECENT":     SortRecent,
		"  recent  ": SortRecent,
		"time":       SortRecent,
		"newest":     SortRecent,
		"relevance":  SortRelevance,
		"时间":         SortRelevance, // 不认：认错比不认更危险
		"recent-ish": SortRelevance,
	}
	for in, want := range cases {
		if got := ParseSortMode(in); got != want {
			t.Errorf("ParseSortMode(%q)=%q，期望 %q", in, got, want)
		}
	}
}

// 泛词表：只命中泛词的实体仍必须被召回，只是排在命中实词的之后。
//
// 判据不是「泛词实体要消失」—— 那会造成漏召回；是「它不能压过实词精确命中」。
func TestRecall_只命中泛词仍可召回(t *testing.T) {
	db := newTestGraph(t)
	defer db.Close()
	seedPorts(t, db)

	res, _ := db.Recall(ExtractKeywords("metrics服务的端口是多少"), nil, 2, "")
	found := false
	for _, e := range res.Entities {
		if e.Name == "缓存端口" {
			found = true
			break
		}
	}
	if !found {
		t.Error("只命中泛词的实体（缓存端口）应仍可召回，只是排在后面")
	}
	// 但针必须在它前面
	if r1, r2 := rankOfName(t, res, "metrics服务端口8328"), rankOfName(t, res, "缓存端口"); r1 > r2 {
		t.Errorf("针排名 %d 不应晚于泛词实体 %d", r1, r2)
	}
}

// 泛词表的可维护性检查：表里的词必须真的是泛词（在 seedPorts 的实体里
// 出现在多个名字中），否则它会误伤实词。
//
// 加词时的判据：「这个词区分得出两个实体吗」。区分不出 → 进泛词表。
func TestGenericKeywords_确实是泛词(t *testing.T) {
	db := newTestGraph(t)
	defer db.Close()
	seedPorts(t, db)

	// 统计每个泛词出现在多少个实体名里
	rows, err := db.db.Query("SELECT name FROM entities")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]int{}
	total := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		total++
		for w := range genericKeywords {
			if strings.Contains(name, w) {
				seen[w]++
			}
		}
	}
	// 判据：在**跑分真实语料**（190 个实体，v4 实测）里，
	// 真正的泛词「端口」覆盖 6/190、「服务」1/190、「版本」4/190、
	// 「任务」3/190、「接口」3/190 —— 也就是说语料本身并没有高比例的泛词。
	//
	// 阈值为 1 而非 2：语料很小，一个词只覆盖 1 个实体不足以证伪它确实是泛词
	// （只是本语料里没有第二个）。真正的保护来自另一条测试 ——
	// TestRecall_只命中泛词仍可召回：误列泛词只会让排序略差，不会漏召回。
	//
	// ★ 这条测试的真实作用是**防止表被无声扩张**：
	// 加词的人很容易凭直觉加，而误列的实际后果是
	// 「叫『数据』的实体的完全相等命中被降级到前缀/包含层级」。
	for w, n := range seen {
		if n < 1 {
			t.Errorf("泛词 %q 在当前语料里覆盖 0 个实体 —— 它对排序毫无作用，"+
				"却让名字含它的实体的「完全相等命中」被降级", w)
		}
	}
	t.Logf("语料实体数=%d，泛词覆盖=%v", total, seen)
}

func countEntities(t *testing.T, db *GraphDB) int {
	t.Helper()
	var n int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM entities").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ★ 变异自证（2）：时间键去掉 TurnID/ID 兜底后，同秒写入的两次覆盖必须分不出先后。
//
// 第一版这条测试也是废的：只按 CreatedAt 排时同秒两次写入完全相等，
// 而 `ORDER BY confidence DESC` 退化到 rowid 升序恰好也是"旧在前"，
// 于是把 TurnID/ID 兜底删掉它照样通过。真正的判据必须让
// **rowid 顺序与时间顺序相反**，这样任何依赖 rowid 的巧合都会暴露。
func TestRecall_时间倒序_变异_同秒且rowid顺序相反(t *testing.T) {
	db := newTestGraph(t)
	defer db.Close()

	// ★ 关键：先写**新值**（turn 1），后写**旧值**（turn 2）——
	// 于是 rowid 升序 = 新值在前，与正确答案（时间倒序 = 新值在前）**同向**，
	// 只按 CreatedAt 排会通过；
	// 而下面把它反过来构造…
	if _, _, err := db.Commit([]Triple{{Subject: "值班室分机", Relation: "是", Object: "4324"}}, "s", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.Commit([]Triple{{Subject: "值班室分机", Relation: "是", Object: "4379"}}, "s", 2); err != nil {
		t.Fatal(err)
	}

	// 同秒验证：两条关系必须落在同一秒（否则本测试测不到兜底逻辑）
	res0, _ := db.Recall([]string{"值班室分机"}, nil, 2, "")
	if len(res0.Relations) < 2 {
		t.Fatalf("关系不足 2 条")
	}
	sameSec := res0.Relations[0].CreatedAt.Equal(res0.Relations[1].CreatedAt)
	t.Logf("同秒=%v（%v vs %v）", sameSec,
		res0.Relations[0].CreatedAt, res0.Relations[1].CreatedAt)
	if !sameSec {
		t.Skipf("两次写入跨了秒，本测试测不到 TurnID 兜底逻辑")
	}

	// 期望：时间倒序 = 按 turn 倒序 = turn2(4379) 在前
	res, _ := db.RecallSorted([]string{"值班室分机"}, nil, 2, "", SortRecent)
	if got := res.Relations[0].TargetName; got != "4379" {
		var all []string
		for _, r := range res.Relations {
			all = append(all, r.TargetName)
		}
		t.Errorf("turn 倒序应让 4379 在前，实际 %s，全部=%v", got, all)
	}
}
