package sdk

import "github.com/JianFeeeee/HomeAgent/pkg/types"

// ConfigAPI provides read/write access to the runtime config object
// (unrelated to the config database).
type ConfigAPI interface {
	Get() *types.Config
	Put(cfg *types.Config)
}
