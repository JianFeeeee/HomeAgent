package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

// fakeGraphMemory 是一个最小 MemoryAPI 桩，只为把 GraphData 喂给 handler。
type fakeGraphMemory struct {
	data map[string]interface{}
}

func (f *fakeGraphMemory) Recall([]string, int) ([]pubsdk.Entity, []pubsdk.Relation, error) {
	return nil, nil, nil
}
func (f *fakeGraphMemory) Commit([]pubsdk.Triple) error { return nil }
func (f *fakeGraphMemory) Introspect() (map[string]interface{}, error) {
	return nil, nil
}
func (f *fakeGraphMemory) MergeEntities(string, string) (int, error) { return 0, nil }
func (f *fakeGraphMemory) Purge(map[string]string, string) (int, error) {
	return 0, nil
}
func (f *fakeGraphMemory) GraphData() (map[string]interface{}, error) { return f.data, nil }

// graphFixture 造一份与生产实例同构的图谱快照：
// 节点带 updated_at（pulse 端点按它过滤），memory_blocks 带稠密 vector。
func graphFixture() map[string]interface{} {
	now := time.Now()
	old := now.Add(-72 * time.Hour)
	return map[string]interface{}{
		"nodes": []map[string]interface{}{
			{"id": 1, "name": "小宅", "type": "Concept", "mention_count": 228,
				"created_at": old, "updated_at": now.Add(-8 * time.Hour)},
			{"id": 2, "name": "刚刚学到的东西", "type": "Concept", "mention_count": 1,
				"created_at": now, "updated_at": now},
		},
		"edges": []map[string]interface{}{},
		"memory_blocks": []memory.MemoryBlock{
			{ID: "b1", Modality: memory.BlockModality("text"), Text: "hi",
				PayloadDigest: "d1", Vector: []float64{0.1, 0.2, 0.3}, CreatedAt: now, UpdatedAt: now},
		},
	}
}

func newGraphHandler(m *fakeGraphMemory) *Handler {
	return &Handler{memory: m}
}

// TestHandleMemoryGraph_StripsVector 钉住「不下发稠密向量」。
//
// 生产实测：8 块记忆的 vector 占 79,314 B / 408,146 B = 19%，而星图
// （本接口唯一消费者）从不读 vector。这部分纯属白付带宽 + 堆内存。
func TestHandleMemoryGraph_StripsVector(t *testing.T) {
	h := newGraphHandler(&fakeGraphMemory{data: graphFixture()})
	rr := httptest.NewRecorder()
	h.handleMemoryGraph(rr, httptest.NewRequest(http.MethodGet, "/api/v1/memory/graph", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if bytes.Contains(rr.Body.Bytes(), []byte(`"vector"`)) {
		t.Fatalf("response still contains vector field:\n%s", rr.Body.String())
	}
	// 其余字段必须还在（不能顺手把整个 memory_blocks 删掉）。
	for _, want := range []string{`"payload_digest":"d1"`, `"id":"b1"`, `"nodes"`, `"edges"`} {
		if !bytes.Contains(rr.Body.Bytes(), []byte(want)) {
			t.Fatalf("response missing %s:\n%s", want, rr.Body.String())
		}
	}
}

// TestHandleMemoryGraphPulse_OnlyRecent 钉住 pulse 端点只回窗口内变动的节点。
//
// 这是星图「新记忆生长」的数据源；它必须比全量图谱小两个数量级。
func TestHandleMemoryGraphPulse_OnlyRecent(t *testing.T) {
	h := newGraphHandler(&fakeGraphMemory{data: graphFixture()})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/memory/graph/pulse", nil)
	h.handleMemoryGraphPulse(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Nodes []struct {
				ID           int64  `json:"id"`
				Name         string `json:"name"`
				MentionCount int    `json:"mention_count"`
			} `json:"nodes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Success {
		t.Fatalf("success = false")
	}
	// 默认窗口 900s：8h 前的「小宅」不该在里面，刚更新的应该在。
	if len(resp.Data.Nodes) != 1 {
		t.Fatalf("expected 1 recent node, got %d: %+v", len(resp.Data.Nodes), resp.Data.Nodes)
	}
	if resp.Data.Nodes[0].ID != 2 {
		t.Fatalf("expected node id=2, got %d", resp.Data.Nodes[0].ID)
	}
}

// TestHandleMemoryGraphPulse_SinceWidens 钉住 since 参数能放大窗口。
func TestHandleMemoryGraphPulse_SinceWidens(t *testing.T) {
	h := newGraphHandler(&fakeGraphMemory{data: graphFixture()})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/memory/graph/pulse?since=86400", nil)
	h.handleMemoryGraphPulse(rr, req)
	var resp struct {
		Data struct {
			Nodes []struct {
				ID int64 `json:"id"`
			} `json:"nodes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data.Nodes) != 2 {
		t.Fatalf("since=86400 should include both nodes, got %d", len(resp.Data.Nodes))
	}
}

// TestHandleMemoryGraphPulse_NoVector 钉住 pulse 响应同样不带 vector。
func TestHandleMemoryGraphPulse_NoVector(t *testing.T) {
	h := newGraphHandler(&fakeGraphMemory{data: graphFixture()})
	rr := httptest.NewRecorder()
	h.handleMemoryGraphPulse(rr, httptest.NewRequest(http.MethodGet, "/api/v1/memory/graph/pulse", nil))
	if bytes.Contains(rr.Body.Bytes(), []byte(`"vector"`)) {
		t.Fatalf("pulse response contains vector:\n%s", rr.Body.String())
	}
}

// TestGraphDataForVisual_PassthroughWhenTypeMismatch 钉住断言失败时不吞数据。
//
// graphDataForVisual 用类型断言识别 memory_blocks。若 GraphData 换了
// 返回类型（比如改成 []*MemoryBlock），断言不中时必须原样透传而不是
// 把字段弄丢 —— 宁可多发 vector，也不能让整个图谱接口变空。
func TestGraphDataForVisual_PassthroughWhenTypeMismatch(t *testing.T) {
	in := map[string]interface{}{
		"nodes":         []map[string]interface{}{{"id": 1}},
		"memory_blocks": []*memory.MemoryBlock{{ID: "x"}},
	}
	out := graphDataForVisual(in)
	if out["memory_blocks"] == nil {
		t.Fatalf("memory_blocks dropped on type mismatch")
	}
	if _, ok := out["nodes"]; !ok {
		t.Fatalf("nodes dropped")
	}
}
