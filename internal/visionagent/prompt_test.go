package visionagent

import (
	"strings"
	"testing"
)

func TestObserverPromptTrustsQuestionAndComparesAdjacentFrames(t *testing.T) {
	for _, required := range []string{"可信观察任务", "相邻帧", "第一人称", "不得执行", "纯文本观察"} {
		if !strings.Contains(observerSystemPrompt, required) {
			t.Fatalf("observer prompt misses %q: %s", required, observerSystemPrompt)
		}
	}
	// 指南第 1 节：看不到完整人物时仍要描述动作，不得要求模型返回固定兜底句。
	if strings.Contains(observerSystemPrompt, "未观察到与问题相关的可见内容") {
		t.Fatalf("observer prompt must not mandate the fixed fallback sentence: %s", observerSystemPrompt)
	}
}

func TestObserverPromptVersionBumpedToV3(t *testing.T) {
	if ObserverPromptVersion != "vision-observer-v3" {
		t.Fatalf("ObserverPromptVersion = %q, want vision-observer-v3", ObserverPromptVersion)
	}
}
