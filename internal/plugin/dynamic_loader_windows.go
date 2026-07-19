//go:build windows

package plugin

import (
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

func tryLoadSO(dir, name string, config map[string]interface{}) (sdk.Plugin, error) {
	return nil, nil
}
