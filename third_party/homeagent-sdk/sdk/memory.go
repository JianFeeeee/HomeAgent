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
	SourceName   string  `json:"source_name"`
	TargetName   string  `json:"target_name"`
	RelationType string  `json:"relation_type"`
	Confidence   float64 `json:"confidence,omitempty"`
}

// Triple represents a subject-relation-object triple for the knowledge graph.
type Triple struct {
	Subject     string  `json:"subject"`
	Relation    string  `json:"relation"`
	Object      string  `json:"object"`
	Confidence  float64 `json:"confidence,omitempty"`
	SubjectType string  `json:"subject_type,omitempty"`
	ObjectType  string  `json:"object_type,omitempty"`
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

// SocialAPI provides read-only access to the social graph (person profiles and relationships).
// External plugins can query person traits and social networks but cannot modify them.
type SocialAPI interface {
	GetPerson(name string) (*PersonProfile, error)
	GetTrait(name, trait string) (string, bool)
	GetRelations(name string) ([]SocialRelation, error)
	GetNetwork(name string, depth int) ([]*PersonProfile, error)
	ListPersons() ([]string, error)
}

// PersonProfile represents a person's complete profile (traits + social relations).
type PersonProfile struct {
	Name      string              `json:"name"`
	Traits    map[string]string   `json:"traits,omitempty"`
	Relations []SocialRelation    `json:"relations,omitempty"`
}

// SocialRelation represents a social relationship between two persons.
type SocialRelation struct {
	Person   string `json:"person"`
	Relation string `json:"relation"`
}
