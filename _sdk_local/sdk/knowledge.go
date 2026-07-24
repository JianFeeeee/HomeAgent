package sdk

// KnowledgeAPI provides access to the knowledge store.
type KnowledgeAPI interface {
	Search(query string, topK int) ([]*Knowledge, error)
	Add(name, content string) error
	List() ([]string, error)
}

// Knowledge represents a knowledge entry.
type Knowledge struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}
