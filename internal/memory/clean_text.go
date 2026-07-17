package memory

import (
	"regexp"
	"strings"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
)

var (
	reQQGroupSuffix = regexp.MustCompile(
		`，通过id\d+使用qq_get_message工具获取消息正文。获取内容后使用 output_send\(channel="qq"\) 回复该群聊，content 设为 JSON 字符串：\{[^}]*\}`,
	)
	reQQPrivateSuffix = regexp.MustCompile(
		`，通过id\d+使用qq_get_message工具获取消息正文。获取内容后使用 output_send\(channel="qq"\) 回复对方，content 设为 JSON 字符串：\{[^}]*\}`,
	)
	reQQOldReply = regexp.MustCompile(
		`通过id\d+使用qq_get_message工具获取消息正文。获取后必须使用[^。]+。`,
	)
	reQQOldForbid = regexp.MustCompile(
		`你只能通过qq_get_message先看消息，然后直接用%!s\(MISSING\)send_private_msg回复，中间的思考过程禁止调用任何其他工具\s*→\s*`,
	)
	reQQGeneral = regexp.MustCompile(
		`通过id\d+使用qq_get_message工具获取消息正文[。，][^。]*?(?:回复|发送消息)`,
	)
	reTimestamp = regexp.MustCompile(
		`\[\d{2}:\d{2}\]\s*`,
	)
	reAgentPrefix = regexp.MustCompile(
		`冷知识|注意|提示|核心要求|规则`,
	)
	reMultiSpace = regexp.MustCompile(`\s+`)
)

func CleanTemplateText(text string) string {
	text = reQQGroupSuffix.ReplaceAllString(text, "")
	text = reQQPrivateSuffix.ReplaceAllString(text, "")
	text = reQQOldReply.ReplaceAllString(text, "")
	text = reQQOldForbid.ReplaceAllString(text, "")
	text = reQQGeneral.ReplaceAllString(text, "")
	text = reTimestamp.ReplaceAllString(text, "")
	text = reMultiSpace.ReplaceAllString(text, " ")
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
	return e.Vectorize(CleanTemplateText(text))
}
