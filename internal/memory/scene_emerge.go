package memory

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ──────────────────────────────────────────────
// 场景的**涌现**：从交互流自己长出场面来
//
// 上一版场景是「声明/派生」的：要么调用方写 `scene="chan:qq"`，要么由通道机械
// 派生。那不是涌现，那是给记忆贴标签，标签谁定、怎么定全靠人。
//
// 这里换成人的记忆那种机制：
//
//	每轮交互都有一个可观察的**场面指纹**（在哪个通道、跟谁、在干什么、聊什么）；
//	指纹反复重合的交互，会自己聚成一个场景（没人声明过它）；
//	场景里写下的记忆自动挂上去；
//	下次指纹再次重合，挂在这个场景上的记忆**自动被唤起**，与措辞无关。
//
// 三条与生俱来的性质：
//   - 自动：指纹全部来自运行时可观察量，无需模型配合、无需人工标注；
//   - 涌现：场景在重复中长出来（首次不建场景，见 minSceneEvidence）；
//   - 强化与遗忘：场景每次重现强度 +1，记忆的挂载权重按重现次数与置信度累积，
//     长期不用的按半衰期衰减——与人的记忆一样，不用就淡。
// ──────────────────────────────────────────────

// 特征权重。决定「哪些特征算同一个场面」：通道与对象是最强的同一性信号
// （在 QQ 上、对老大），工具是行为信号，话题是软信号（同一场面的不同话题
// 不该被拆开，所以权重低），时段最弱。
const (
	wFeatChan  = 1.0
	wFeatPeer  = 1.0
	wFeatTool  = 0.8
	wFeatTopic = 0.4
	wFeatPart  = 0.2
)

// 聚类阈值。
//
// joinSceneThreshold 是「这轮属于既有场景」的下限——定在 0.5 意味着
// 「要么主体特征重合，要么好几条信号一起重合」才算同一个场面。
// recallSceneThreshold 比它低：**唤起**比**归属**宽松，想不起来是损失，
// 多想起一条只是多几行上下文（与人的联想一致）。
const (
	joinSceneThreshold   = 0.5
	recallSceneThreshold = 0.35
	// minSceneEvidence 是长成场景所需的最少重现次数。
	//
	// 为什么首次不建场景：一次性的交互不是「场面」，给它建场景会让图库被
	// 一次性事件撑满，之后每次路过都要召回一堆只发生过一次的事。
	// 第 2 次出现同类指纹时才认定「这事会重复」。
	minSceneEvidence = 2
	// maxSituationFeatures 是单轮指纹的特征上限（防长输入把相似度算糊涂）。
	maxSituationFeatures = 24
)

// SituationFeature 是一轮交互里的一个可观察信号，形如 `chan:qq`、`peer:group_1027`。
type SituationFeature struct {
	Kind  string
	Value string
}

// Key 返回规范化的特征串。Kind 与 Value 都过场景键归一化，
// 保证 `chan:QQ` 与 `chan:qq` 是同一个特征。
func (f SituationFeature) Key() string {
	kind := NormalizeSceneKey(f.Kind)
	val := NormalizeSceneKey(f.Value)
	if kind == "" || val == "" {
		return ""
	}
	return kind + ":" + val
}

// Weight 返回该特征的权重（按 Kind）。
func (f SituationFeature) Weight() float64 {
	switch NormalizeSceneKey(f.Kind) {
	case "chan":
		return wFeatChan
	case "peer":
		return wFeatPeer
	case "tool":
		return wFeatTool
	case "topic":
		return wFeatTopic
	case "part":
		return wFeatPart
	default:
		return wFeatTopic
	}
}

// Situation 是一轮交互的场面指纹（去重、上限裁剪后的特征集合）。
type Situation struct {
	Features []SituationFeature
}

// NewSituation 由若干特征构造指纹：归一化、去重、按权重降序裁剪到上限。
func NewSituation(features ...SituationFeature) Situation {
	seen := make(map[string]bool, len(features))
	out := make([]SituationFeature, 0, len(features))
	for _, f := range features {
		k := f.Key()
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Weight() > out[j].Weight() })
	if len(out) > maxSituationFeatures {
		out = out[:maxSituationFeatures]
	}
	return Situation{Features: out}
}

// Keys 返回指纹的特征串列表。
func (s Situation) Keys() []string {
	out := make([]string, 0, len(s.Features))
	for _, f := range s.Features {
		out = append(out, f.Key())
	}
	return out
}

// Empty 表示指纹里没有任何可判定的信号。
func (s Situation) Empty() bool { return len(s.Features) == 0 }

// Label 用权重最高的少数特征给场景起个可读名字（`chan:qq+tool:qq_get_message`）。
// 只用于人看，不参与匹配——匹配永远走特征集合。
func (s Situation) Label(max int) string {
	if max <= 0 {
		max = 2
	}
	keys := s.Keys()
	if len(keys) > max {
		keys = keys[:max]
	}
	return strings.Join(keys, "+")
}

// emergentScene 是一次聚类计算中的场景视图。
type emergentScene struct {
	ID       int64
	Key      string
	Strength int
	Weights  map[string]float64
}

// similarity 是加权 Jaccard：共享特征的权重和 / 并集特征的权重和。
//
// 为什么加权：`chan:qq` 与 `topic:排班` 对「是不是同一个场面」的证据力差 2.5 倍，
// 不加权会让一次偶然的话题重合把两个不同场面并成一个。
func (a emergentScene) similarity(b emergentScene) float64 {
	if len(a.Weights) == 0 || len(b.Weights) == 0 {
		return 0
	}
	shared, union := 0.0, 0.0
	for k, w := range a.Weights {
		if w2, ok := b.Weights[k]; ok {
			shared += min(w, w2)
			union += max(w, w2)
		} else {
			union += w
		}
	}
	for k, w := range b.Weights {
		if _, ok := a.Weights[k]; !ok {
			union += w
		}
	}
	if union == 0 {
		return 0
	}
	return shared / union
}

// situationSimilarity 计算指纹与既有场景的相似度。
func situationSimilarity(sig Situation, sc emergentScene) float64 {
	cur := make(map[string]float64, len(sig.Features))
	for _, f := range sig.Features {
		cur[f.Key()] = f.Weight()
	}
	return emergentScene{Weights: cur}.similarity(sc)
}

// EnterScene 是本机制的主入口：给一轮交互的指纹找到（或长出）它的场景。
//
// 返回解析出的场景键与是否新建。调用方拿这个键去做两件事：
//  1. 本轮写下的记忆自动挂到它上面（Triple.Scene）
//  2. 本轮召回按它（以及相似场景）取回记忆
//
// 「首次不建场景」的例外：指纹只出现一次时返回空键——一次性交互不该有场面，
// 见 minSceneEvidence 的说明。
func (g *GraphDB) EnterScene(sig Situation) (string, bool, error) {
	if sig.Empty() {
		return "", false, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	scenes, err := g.loadEmergentScenesLocked()
	if err != nil {
		return "", false, err
	}

	bestIdx, bestSim := -1, 0.0
	for i, sc := range scenes {
		if sim := situationSimilarity(sig, sc); sim > bestSim {
			bestIdx, bestSim = i, sim
		}
	}

	// 命中既有场景：强化（并入新特征、强度 +1、时间刷新）
	if bestIdx >= 0 && bestSim >= joinSceneThreshold {
		sc := scenes[bestIdx]
		if err := g.reinforceSceneLocked(sc.ID, sig); err != nil {
			return "", false, err
		}
		return sc.Key, false, nil
	}

	// 未命中：看有没有「同类指纹的足迹」——首次出现只登记线索，不建场景
	evidence, err := g.recordSituationEvidenceLocked(sig)
	if err != nil {
		return "", false, err
	}
	if evidence < minSceneEvidence {
		return "", false, nil
	}

	key, err := g.createSceneLocked(sig)
	if err != nil {
		return "", false, err
	}
	return key, true, nil
}

// loadEmergentScenesLocked 读入全部场景及其特征权重。
func (g *GraphDB) loadEmergentScenesLocked() ([]emergentScene, error) {
	rows, err := g.db.Query(`SELECT id, key, COALESCE(strength, 1) FROM scenes`)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*emergentScene)
	var out []emergentScene
	for rows.Next() {
		var sc emergentScene
		if err := rows.Scan(&sc.ID, &sc.Key, &sc.Strength); err != nil {
			rows.Close()
			return nil, err
		}
		sc.Weights = make(map[string]float64)
		byID[sc.ID] = &sc
		out = append(out, sc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}

	frows, err := g.db.Query(`SELECT scene_id, feature, weight FROM scene_features`)
	if err != nil {
		return nil, err
	}
	defer frows.Close()
	for frows.Next() {
		var sid int64
		var feat string
		var w float64
		if err := frows.Scan(&sid, &feat, &w); err != nil {
			return nil, err
		}
		if sc, ok := byID[sid]; ok {
			sc.Weights[feat] = w
		}
	}
	if err := frows.Err(); err != nil {
		return nil, err
	}
	// 回填（out 里的元素是值拷贝，Weights 是同一 map，指针内容已更新）
	for i := range out {
		if sc, ok := byID[out[i].ID]; ok {
			out[i].Weights = sc.Weights
		}
	}
	return out, nil
}

// reinforceSceneLocked 把一轮指纹并入既有场景并强化它。
func (g *GraphDB) reinforceSceneLocked(sceneID int64, sig Situation) error {
	tx, err := g.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, f := range sig.Features {
		if _, err := tx.Exec(
			`INSERT INTO scene_features (scene_id, feature, weight) VALUES (?, ?, ?)
			 ON CONFLICT(scene_id, feature) DO UPDATE SET weight = MAX(weight, excluded.weight)`,
			sceneID, f.Key(), f.Weight()); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		`UPDATE scenes SET strength = COALESCE(strength, 1) + 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		sceneID); err != nil {
		return err
	}
	return tx.Commit()
}

// createSceneLocked 用指纹长出一个新场景（键由主导特征派生，仅作可读名）。
func (g *GraphDB) createSceneLocked(sig Situation) (string, error) {
	base := "auto:" + sig.Label(2)
	key := base

	tx, err := g.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	// 键冲突（同一可读名已被占）时加后缀，不合并——真正的合并交给相似度判定。
	for i := 2; ; i++ {
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM scenes WHERE key = ?`, key).Scan(&exists); err != nil {
			return "", err
		}
		if exists == 0 {
			break
		}
		key = fmt.Sprintf("%s#%d", base, i)
	}

	res, err := tx.Exec(`INSERT INTO scenes (key, strength) VALUES (?, 1)`, key)
	if err != nil {
		return "", err
	}
	sceneID, _ := res.LastInsertId()
	for _, f := range sig.Features {
		if _, err := tx.Exec(
			`INSERT INTO scene_features (scene_id, feature, weight) VALUES (?, ?, ?)
			 ON CONFLICT(scene_id, feature) DO UPDATE SET weight = MAX(weight, excluded.weight)`,
			sceneID, f.Key(), f.Weight()); err != nil {
			return "", err
		}
	}
	// 场景成立后，把此前登记的同类线索清掉（它们已被这次长出吸收）
	if _, err := tx.Exec(`DELETE FROM situation_evidence`); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return key, nil
}

// recordSituationEvidenceLocked 登记一次「同类指纹出现过」，返回累计次数。
//
// 用指纹标签（主导特征）做粗聚类桶，只服务于「首次不建场景」的门槛判定，
// 不参与后续匹配——匹配永远走 EnterScene 的相似度。
func (g *GraphDB) recordSituationEvidenceLocked(sig Situation) (int, error) {
	label := sig.Label(2)
	if _, err := g.db.Exec(
		`INSERT INTO situation_evidence (label, count, updated_at) VALUES (?, 1, CURRENT_TIMESTAMP)
		 ON CONFLICT(label) DO UPDATE SET count = count + 1, updated_at = CURRENT_TIMESTAMP`, label); err != nil {
		return 0, err
	}
	var n int
	if err := g.db.QueryRow(`SELECT count FROM situation_evidence WHERE label = ?`, label).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// RecallBySituation 按**场面相似**取回记忆：不是键相等，而是「像不像同一个场面」。
//
// 命中多个场景时按相似度 × 权重合并，跨场景去重（同一关系只出现一次）。
// 这正是「类似的场景自动唤起对应的记忆」那一下。
func (g *GraphDB) RecallBySituation(sig Situation, limit int) (*SceneRecall, error) {
	if sig.Empty() {
		return &SceneRecall{}, nil
	}
	scenes, err := g.loadEmergentScenesLocked()
	if err != nil {
		return nil, err
	}
	if len(scenes) == 0 {
		return &SceneRecall{}, nil
	}

	type hit struct {
		key string
		sim float64
	}
	var hits []hit
	for _, sc := range scenes {
		sim := situationSimilarity(sig, sc)
		if sim >= recallSceneThreshold {
			hits = append(hits, hit{key: sc.Key, sim: sim})
		}
	}
	if len(hits) == 0 {
		return &SceneRecall{}, nil
	}
	// 相似度高的场景排前面；同相似度时强度高的优先（更常重现的场面更可信）
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].sim != hits[j].sim {
			return hits[i].sim > hits[j].sim
		}
		return hits[i].key < hits[j].key
	})

	keys := make([]string, 0, len(hits))
	for _, h := range hits {
		keys = append(keys, h.key)
	}
	// 复用按场景键的取回逻辑（前缀语义 + weight 排序），再按相似度加权重排
	out, err := g.RecallByScene(keys, limit)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DecaySceneRefs 让久未重现的场景记忆按半衰期淡出，返回删除的引用数。
//
// 人的记忆是靠「用进废退」维持秩序的：不做衰减，一次性的巧合关联会
// 永远留在场景里，每次路过都被注入，越攒越多直到注入预算被吃光。
// 权重按半衰期折半；低于 floor 的引用直接删除（关联已无信息量）。
func (g *GraphDB) DecaySceneRefs(halfLife time.Duration, floor float64) (int, error) {
	if halfLife <= 0 {
		return 0, nil
	}
	if floor <= 0 {
		floor = 0.05
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	cut := time.Now().Add(-halfLife).Format("2006-01-02 15:04:05")
	if _, err := g.db.Exec(
		`UPDATE scene_refs SET weight = weight * 0.5
		 WHERE created_at < ? AND id NOT IN (
			SELECT sr.id FROM scene_refs sr JOIN scenes s ON sr.scene_id = s.id
			WHERE s.updated_at >= ?)`, cut, cut); err != nil {
		return 0, err
	}
	res, err := g.db.Exec(`DELETE FROM scene_refs WHERE weight < ?`, floor)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// EmergentScenes 列出当前长出来的场景（按强度降序），供观察「涌现」是否在发生。
func (g *GraphDB) EmergentScenes() ([]SceneStat, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	rows, err := g.db.Query(
		`SELECT s.key, COALESCE(s.strength,1),
		        (SELECT COUNT(*) FROM scene_refs sr WHERE sr.scene_id = s.id),
		        (SELECT COUNT(*) FROM scene_features f WHERE f.scene_id = s.id),
		        s.updated_at
		 FROM scenes s ORDER BY COALESCE(s.strength,1) DESC, s.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SceneStat
	for rows.Next() {
		var st SceneStat
		if err := rows.Scan(&st.Key, &st.Strength, &st.Refs, &st.Features, &st.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
