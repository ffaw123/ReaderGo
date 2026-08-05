package chat

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

type presetReply struct {
	Rule    string
	Content string
}

type shortcutMatcher struct {
	replies map[string]presetReply
}

func newShortcutMatcher() shortcutMatcher {
	return shortcutMatcher{replies: map[string]presetReply{
		"你好":     {Rule: "greeting", Content: "你好，我可以回答普通问题，也可以在视频处理完成后根据摘要或具体片段回答。"},
		"您好":     {Rule: "greeting", Content: "你好，我可以回答普通问题，也可以在视频处理完成后根据摘要或具体片段回答。"},
		"嗨":      {Rule: "greeting", Content: "你好，我可以回答普通问题，也可以在视频处理完成后根据摘要或具体片段回答。"},
		"hello":  {Rule: "greeting", Content: "你好，我可以回答普通问题，也可以在视频处理完成后根据摘要或具体片段回答。"},
		"hi":     {Rule: "greeting", Content: "你好，我可以回答普通问题，也可以在视频处理完成后根据摘要或具体片段回答。"},
		"在吗":     {Rule: "greeting", Content: "在的。你可以问普通问题，也可以询问当前视频。"},
		"谢谢":     {Rule: "thanks", Content: "不客气。"},
		"多谢":     {Rule: "thanks", Content: "不客气。"},
		"感谢":     {Rule: "thanks", Content: "不客气。"},
		"thanks": {Rule: "thanks", Content: "不客气。"},
		"再见":     {Rule: "farewell", Content: "再见，有需要可以继续问我。"},
		"拜拜":     {Rule: "farewell", Content: "再见，有需要可以继续问我。"},
		"bye":    {Rule: "farewell", Content: "再见，有需要可以继续问我。"},
		"好的":     {Rule: "acknowledge", Content: "好的。"},
		"明白了":    {Rule: "acknowledge", Content: "好的。"},
		"知道了":    {Rule: "acknowledge", Content: "好的。"},
		"收到":     {Rule: "acknowledge", Content: "好的。"},
		"你能做什么":  {Rule: "capabilities", Content: "我可以进行普通对话、概括当前视频，并在检索准备完成后回答视频中的具体问题。"},
		"怎么使用":   {Rule: "capabilities", Content: "直接输入一个问题即可。你可以询问视频概览，也可以询问视频中的具体内容。"},
	}}
}

func (m shortcutMatcher) Match(input string) (presetReply, bool) {
	normalized := normalizeShortcut(input)
	if normalized == "" || utf8.RuneCountInString(normalized) > 20 {
		return presetReply{}, false
	}
	// 必须完整匹配白名单，避免“你好，请总结视频”被问候规则截断。
	reply, ok := m.replies[normalized]
	return reply, ok
}

func normalizeShortcut(input string) string {
	value := strings.ToLower(strings.TrimSpace(input))
	value = strings.Join(strings.Fields(value), " ")
	return strings.TrimRightFunc(value, func(r rune) bool {
		return unicode.IsPunct(r) || unicode.IsSpace(r)
	})
}
