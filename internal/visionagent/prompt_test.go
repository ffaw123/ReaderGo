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

func TestOrchestratorPromptUsesServerOwnedObservationTracking(t *testing.T) {
	if OrchestratorPromptVersion != "vision-orchestrator-v9" {
		t.Fatalf("OrchestratorPromptVersion = %q, want vision-orchestrator-v9", OrchestratorPromptVersion)
	}
	if strings.Contains(orchestratorSystemPrompt, `"used_observation_ids"`) || !strings.Contains(orchestratorSystemPrompt, "由服务端记录") {
		t.Fatalf("orchestrator prompt must leave observation tracking to the server: %s", orchestratorSystemPrompt)
	}
	for _, required := range []string{"remaining_vision_calls", "remaining_frame_budget", "结合剩余预算决策"} {
		if !strings.Contains(orchestratorSystemPrompt, required) {
			t.Fatalf("orchestrator prompt misses budget rule %q: %s", required, orchestratorSystemPrompt)
		}
	}
	for _, required := range []string{"最近完整对话", "历史的助手回答", "不是本轮的视觉证据"} {
		if !strings.Contains(orchestratorSystemPrompt, required) {
			t.Fatalf("orchestrator prompt misses dialogue rule %q: %s", required, orchestratorSystemPrompt)
		}
	}
	for _, required := range []string{"缩小可疑时间范围", "减小 interval_ms", "尚未覆盖的其他时间段", "不要求一定比上一次更小", "不要重复相同的区间和采样间隔"} {
		if !strings.Contains(orchestratorSystemPrompt, required) {
			t.Fatalf("orchestrator prompt misses refinement rule %q: %s", required, orchestratorSystemPrompt)
		}
	}
	for _, heading := range []string{"## 角色", "## 证据与信任边界", "## 工具调用规则", "## 时间区间变更", "## 预算与终止", "## 最终输出"} {
		if !strings.Contains(orchestratorSystemPrompt, heading) {
			t.Fatalf("orchestrator prompt misses heading %q: %s", heading, orchestratorSystemPrompt)
		}
	}
	if strings.Contains(orchestratorSystemPrompt, "首次响应必须调用") {
		t.Fatalf("orchestrator prompt must not force an initial tool call: %s", orchestratorSystemPrompt)
	}
	for _, required := range []string{"allowed_range 之外", "抽帧前中断", "请求用户确认", "继续执行同一个工具调用"} {
		if !strings.Contains(orchestratorSystemPrompt, required) {
			t.Fatalf("orchestrator prompt misses range-change rule %q: %s", required, orchestratorSystemPrompt)
		}
	}
}
