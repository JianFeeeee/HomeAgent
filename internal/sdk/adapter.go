package sdk

// AdapterAPI provides access to the Lua LLM protocol adapters.
type AdapterAPI interface {
	List() []APIAdapter
	// Load writes the adapter source file and hot-reloads it.
	Load(name, code string) error
	Remove(name string) error
	AdapterDir() string
}

// APIAdapter describes a loaded Lua protocol adapter.
type APIAdapter struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}
