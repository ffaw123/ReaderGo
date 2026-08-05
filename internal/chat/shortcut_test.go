package chat

import "testing"

func TestShortcutMatcherRequiresWholeUtterance(t *testing.T) {
	matcher := newShortcutMatcher()
	for _, input := range []string{"你好", "  HELLO！  ", "谢谢。"} {
		if _, ok := matcher.Match(input); !ok {
			t.Fatalf("expected shortcut match for %q", input)
		}
	}
	for _, input := range []string{"你好，请总结视频", "谢谢，能解释第三章吗？", "视频里为什么说你好很重要？"} {
		if _, ok := matcher.Match(input); ok {
			t.Fatalf("unexpected shortcut match for compound input %q", input)
		}
	}
}
