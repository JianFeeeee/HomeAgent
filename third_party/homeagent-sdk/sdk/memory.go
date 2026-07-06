package sdk

// MemoryAPI provides access to the graph memory (entity-relation store).
type MemoryAPI interface {
	Recall(query []string, depth int) ([]Entity, []Relation, error)
	Commit(triples []Triple) error
	Introspect() (map[string]interface{}, error)
	MergeEntities(source, target string) (int, error)
	Purge(criteria map[string]string, mode string) (int, error)
}

// Entity represents a named entity in the knowledge graph.
type Entity struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	MentionCount int    `json:"mention_count"`
}

// Relation represents a relationship between two entities.
type Relation struct {
	SourceName   string `json:"source_name"`
	TargetName   string `json:"target_name"`
	RelationType string `json:"relation_type"`
}

// Triple represents a subject-relation-object triple for the knowledge graph.
type Triple struct {
	Subject  string `json:"subject"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
}

// TextMemoryAPI provides access to chronological text event storage.
type TextMemoryAPI interface {
	Append(evt TextEvent) error
}

// TextEvent represents a single text memory event.
type TextEvent struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Timestamp int64  `json:"timestamp"`
	Channel   string `json:"channel,omitempty"`
}

// DocMemoryAPI provides access to the document vector store.
type DocMemoryAPI interface {
	Query(text string, topK int) []*Doc
	Insert(doc *Doc) error
	Remove(id string)
	Stats() map[string]interface{}
}

// Doc represents a document in the document store.
type Doc struct {
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	Content string  `json:"content"`
	Score   float64 `json:"score,omitempty"`
}
