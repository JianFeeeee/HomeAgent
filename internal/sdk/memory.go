package sdk

import (
	sdkext "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	doc "gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/text"
)

type MemoryAPI = sdkext.MemoryAPI
type Entity = sdkext.Entity
type Relation = sdkext.Relation
type Triple = sdkext.Triple

type TextMemoryAPI = sdkext.TextMemoryAPI
type TextEvent = sdkext.TextEvent

type DocMemoryAPI = sdkext.DocMemoryAPI
type Doc = sdkext.Doc

type graphMemory struct{ db *memory.GraphDB }

func NewGraphMemory(db *memory.GraphDB) MemoryAPI { return &graphMemory{db: db} }

func (m *graphMemory) Recall(query []string, depth int) ([]Entity, []Relation, error) {
	if m.db == nil {
		return nil, nil, nil
	}
	result, err := m.db.Recall(query, nil, depth, "")
	if err != nil {
		return nil, nil, err
	}
	entities := make([]Entity, len(result.Entities))
	for i, e := range result.Entities {
		entities[i] = Entity{Name: e.Name, Type: e.Type, MentionCount: e.MentionCount}
	}
	relations := make([]Relation, len(result.Relations))
	for i, r := range result.Relations {
		relations[i] = Relation{SourceName: r.SourceName, TargetName: r.TargetName, RelationType: r.RelationType}
	}
	return entities, relations, nil
}

func (m *graphMemory) Commit(triples []Triple) error {
	if m.db == nil { return nil }
	ts := make([]memory.Triple, len(triples))
	for i, t := range triples {
		ts[i] = memory.Triple{Subject: t.Subject, Relation: t.Relation, Object: t.Object}
	}
	_, _, err := m.db.Commit(ts, "plugin", 0)
	return err
}

func (m *graphMemory) Introspect() (map[string]interface{}, error) {
	if m.db == nil { return map[string]interface{}{}, nil }
	return m.db.Introspect()
}

func (m *graphMemory) MergeEntities(source, target string) (int, error) {
	if m.db == nil { return 0, nil }
	return m.db.MergeEntities(source, target)
}

func (m *graphMemory) Purge(criteria map[string]string, mode string) (int, error) {
	if m.db == nil { return 0, nil }
	return m.db.Purge(criteria, mode)
}

type textMemoryImpl struct{ tm *text.Memory }

func NewTextMemory(tm *text.Memory) TextMemoryAPI { return &textMemoryImpl{tm: tm} }

func (m *textMemoryImpl) Append(evt TextEvent) error {
	if m.tm == nil { return nil }
	return m.tm.Append(text.Event{Timestamp: evt.Timestamp, Source: evt.Role, Input: evt.Content, AgentID: evt.Channel})
}

type docMemoryImpl struct{ ds *doc.Store }

func NewDocMemory(ds *doc.Store) DocMemoryAPI { return &docMemoryImpl{ds: ds} }

func (m *docMemoryImpl) Query(text string, topK int) []*Doc {
	if m.ds == nil { return nil }
	got := m.ds.Query(text, topK)
	out := make([]*Doc, len(got))
	for i, d := range got {
		out[i] = &Doc{ID: d.ID, Title: d.Summary, Content: d.Content}
	}
	return out
}

func (m *docMemoryImpl) Insert(d *Doc) error {
	if m.ds == nil { return nil }
	return m.ds.Insert(&doc.Doc{ID: d.ID, Summary: d.Title, Content: d.Content})
}

func (m *docMemoryImpl) Remove(id string) { if m.ds != nil { m.ds.Remove(id) } }
func (m *docMemoryImpl) Stats() map[string]interface{} {
	if m.ds == nil { return map[string]interface{}{} }
	return m.ds.Stats()
}
