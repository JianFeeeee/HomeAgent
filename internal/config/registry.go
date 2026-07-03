package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type ConfigRegistry struct {
	mu       sync.RWMutex
	values   map[string]interface{}
	persistPath string
	dirty    bool
}

func NewConfigRegistry(persistPath string) *ConfigRegistry {
	r := &ConfigRegistry{
		values:      make(map[string]interface{}),
		persistPath: persistPath,
	}
	r.load()
	return r
}

func (r *ConfigRegistry) Register(key string, value interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.values[key]; !exists {
		r.values[key] = value
	}
}

func (r *ConfigRegistry) RegisterDefault(key string, value interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.values[key]; !exists {
		r.values[key] = value
	}
}

func (r *ConfigRegistry) Get(key string) (interface{}, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.values[key]
	if !ok {
		return nil, fmt.Errorf("config key %q not found", key)
	}
	return v, nil
}

func (r *ConfigRegistry) Set(key string, value interface{}) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[key] = value
	r.dirty = true
	return nil
}

func (r *ConfigRegistry) List(prefix string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var keys []string
	for k := range r.values {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func (r *ConfigRegistry) Delete(key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.values, key)
	r.dirty = true
	return nil
}

func (r *ConfigRegistry) Dump() map[string]interface{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cp := make(map[string]interface{})
	for k, v := range r.values {
		cp[k] = v
	}
	return cp
}

func (r *ConfigRegistry) Flush() error {
	r.mu.RLock()
	if !r.dirty {
		r.mu.RUnlock()
		return nil
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.persistPath == "" {
		return nil
	}
	os.MkdirAll(filepath.Dir(r.persistPath), 0755)
	data, err := json.MarshalIndent(r.values, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(r.persistPath, data, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	r.dirty = false
	return nil
}

func (r *ConfigRegistry) load() {
	if r.persistPath == "" {
		return
	}
	data, err := os.ReadFile(r.persistPath)
	if err != nil {
		return
	}
	var vals map[string]interface{}
	if err := json.Unmarshal(data, &vals); err != nil {
		return
	}
	r.values = vals
}
