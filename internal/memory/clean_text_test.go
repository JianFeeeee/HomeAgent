package memory

import (
	"testing"
)

func TestCleanTextTrim(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"  hello  ", "hello"},
		{"，hello", "hello"},
		{"，，hello", "hello"},
		{"  ，，hello  ", "hello"},
		{"", ""},
		{"   ", ""},
		{"，", ""},
		{"，x", "x"},
	}
	for _, tt := range tests {
		got := CleanText(tt.input)
		if got != tt.expected {
			t.Errorf("CleanText(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
