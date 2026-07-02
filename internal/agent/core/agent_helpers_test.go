package core

import (
	"testing"
)

func TestTruncateStr(t *testing.T) {
	tests := []struct {
		input string
		max   int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hello..."},
		{"你好世界", 2, "你好..."},
		{"", 5, ""},
		{"abc", 3, "abc"},
	}
	for _, tt := range tests {
		got := truncateStr(tt.input, tt.max)
		if got != tt.want {
			t.Errorf("truncateStr(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.want)
		}
	}
}

func TestGetString(t *testing.T) {
	m := map[string]interface{}{
		"name": "张三",
		"age":  30,
	}

	if got := getString(m, "name"); got != "张三" {
		t.Errorf("expected '张三', got %q", got)
	}
	if got := getString(m, "age"); got != "" {
		t.Errorf("expected empty for int, got %q", got)
	}
	if got := getString(m, "nonexistent"); got != "" {
		t.Errorf("expected empty for missing key, got %q", got)
	}
	if got := getString(nil, "key"); got != "" {
		t.Errorf("expected empty for nil map, got %q", got)
	}
}

func TestGetFloat(t *testing.T) {
	m := map[string]interface{}{
		"count": 42.5,
		"score": 100,
		"name":  "test",
	}

	if got := getFloat(m, "count"); got != 42.5 {
		t.Errorf("expected 42.5, got %f", got)
	}
	if got := getFloat(m, "score"); got != 100.0 {
		t.Errorf("expected 100.0, got %f", got)
	}
	if got := getFloat(m, "name"); got != 0 {
		t.Errorf("expected 0 for string, got %f", got)
	}
	if got := getFloat(m, "nonexistent"); got != 0 {
		t.Errorf("expected 0 for missing key, got %f", got)
	}
	if got := getFloat(nil, "key"); got != 0 {
		t.Errorf("expected 0 for nil map, got %f", got)
	}
}

func TestGetFloatInt(t *testing.T) {
	m := map[string]interface{}{
		"top_k": float64(5),
	}
	if got := getFloat(m, "top_k"); got != 5.0 {
		t.Errorf("expected 5.0, got %f", got)
	}
}
