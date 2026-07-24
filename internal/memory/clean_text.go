package memory

import (
	"strings"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
)

var globalTextCleaner func(string) string

func SetTextCleaner(fn func(string) string) {
	globalTextCleaner = fn
}

func CleanText(text string) string {
	if globalTextCleaner != nil {
		text = globalTextCleaner(text)
	}

	text = strings.TrimSpace(text)

	if text == "" {
		return ""
	}

	text = strings.TrimPrefix(text, "，")
	text = strings.TrimPrefix(text, "，")
	text = strings.TrimSpace(text)
	return text
}

func (e *StaticEmbedder) VectorizeClean(text string) vector.Vector {
	return e.Vectorize(CleanText(text))
}
