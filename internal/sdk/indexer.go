package sdk

// IndexerAPI exposes the memory context indexer.
type IndexerAPI interface {
	BuildContext(q string) (*IndexContext, error)
	FormatContext(ctx *IndexContext) string
	GetToolDefinitions() []map[string]interface{}
	BuildToolPrompt() string
}

// IndexContext is a neutral snapshot of the injected memory context.
type IndexContext struct {
	Entities      []Entity `json:"entities"`
	Relations     []Relation `json:"relations"`
	Summary       string     `json:"summary"`
	TokenEstimate int        `json:"token_estimate"`
}
