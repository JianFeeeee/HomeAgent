package core

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
)

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getFloat(m map[string]interface{}, key string) float64 {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		}
	}
	return 0
}

// getStringSlice 从工具参数里取字符串数组。
//
// 需要单独一个 helper 而不是直接断言 []string：LLM 的参数经 JSON 解码后是
// []interface{}，直接断言 []string 恒失败——静默拿到 nil，参数像没传一样。
func getStringSlice(m map[string]interface{}, key string) []string {
	raw, ok := m[key].([]interface{})
	if !ok {
		return nil
	}
	var out []string
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func truncateStr(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	var truncated int
	for i := range s {
		if truncated >= max {
			return s[:i] + "..."
		}
		truncated++
	}
	return s
}

func formatTree(node *knowledge.TreeIndex, depth int) string {
	var sb strings.Builder
	indent := strings.Repeat("  ", depth)
	for _, child := range node.Children {
		sb.WriteString(fmt.Sprintf("%s%s/\n", indent, child.Name))
		sb.WriteString(formatTree(child, depth+1))
	}
	for _, item := range node.Items {
		preview := item.Preview
		if len([]rune(preview)) > 60 {
			preview = string([]rune(preview)[:60]) + "..."
		}
		tags := ""
		if len(item.Tags) > 0 {
			tags = " [" + strings.Join(item.Tags, ", ") + "]"
		}
		sb.WriteString(fmt.Sprintf("%s· %s%s\n", indent, item.Name, tags))
		sb.WriteString(fmt.Sprintf("%s  %s\n", indent, preview))
	}
	if sb.Len() == 0 {
		sb.WriteString("(空)")
	}
	return sb.String()
}
