package memory

import (
	"testing"
)

func TestCleanTextTrim(t *testing.T) {
	prev := globalTextCleaner
	globalTextCleaner = nil
	defer func() { globalTextCleaner = prev }()

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

func TestCleanTextWithRegisteredCleaner(t *testing.T) {
	prev := globalTextCleaner
	globalTextCleaner = func(text string) string {
		return "prefix_" + text
	}
	defer func() { globalTextCleaner = prev }()

	got := CleanText("  hello  ")
	if got != "prefix_  hello" {
		t.Errorf("CleanText with cleaner = %q, want %q", got, "prefix_  hello")
	}
}

func TestCleanTextCleanerChain(t *testing.T) {
	prev := globalTextCleaner
	globalTextCleaner = func(text string) string {
		text = text + "_step1"
		text = text + "_step2"
		return text
	}
	defer func() { globalTextCleaner = prev }()

	got := CleanText("test")
	if got != "test_step1_step2" {
		t.Errorf("CleanText chain = %q, want %q", got, "test_step1_step2")
	}
}

func TestSetTextCleanerReplace(t *testing.T) {
	prev := globalTextCleaner
	globalTextCleaner = func(text string) string { return "old_" + text }

	SetTextCleaner(func(text string) string { return "new_" + text })
	defer func() { globalTextCleaner = prev }()

	got := CleanText("x")
	if got != "new_x" {
		t.Errorf("after SetTextCleaner = %q, want %q", got, "new_x")
	}
}

func TestCleanTextEmptyAfterCleaner(t *testing.T) {
	prev := globalTextCleaner
	globalTextCleaner = func(text string) string { return "" }
	defer func() { globalTextCleaner = prev }()

	got := CleanText("something")
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}
