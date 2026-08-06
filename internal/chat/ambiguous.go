package chat

import "time"

func ambiguousReply(decision IntentDecision) generatedReply {
	content := "我暂时没能理解这个问题，请换一种更具体的说法。"
	switch decision.AmbiguityReason {
	case AmbiguityMultiIntent:
		content = "你的消息中包含多个问题。请一次只问一个问题，以便我准确选择视频摘要或详细检索。"
	case AmbiguityLowConfidence:
		content = "我还不能确定你是在询问当前视频还是一般知识。请说明问题是否与视频有关；如果有关，可以指出主题、章节或时间点。"
	case AmbiguityMissingReference:
		content = "你提到的对象不够明确，请说明“这个”“它”或“第二点”具体指什么。"
	}
	return generatedReply{
		Content:  content,
		Route:    RouteInfo{Mode: ModeClarify, Intent: IntentAmbiguous},
		Decision: decision, AnswerPromptVersion: "clarification-template-v1",
	}
}

// 当能力未构建完成时调用
func capabilityReply(decision IntentDecision, capabilities Capabilities, intentLatency time.Duration) generatedReply {
	mode := ModeCapabilityPending
	content := "相关视频内容仍在处理中，请稍后再试。"
	switch decision.RequiredCapability {
	case CapabilitySummary:
		if capabilities.SummaryState == CapabilityStateFailed {
			mode = ModeCapabilityFailed
			content = "视频摘要生成失败，暂时无法回答概览问题。你可以查看视频处理状态后重试。"
		} else {
			content = "视频摘要仍在生成中，暂时无法回答概览问题，请稍后再试。"
		}
	case CapabilityRAG:
		if capabilities.RAGState == CapabilityStateFailed {
			mode = ModeCapabilityFailed
			content = "视频详细检索构建失败，暂时无法回答具体内容问题。你可以查看视频处理状态后重试。"
		} else {
			content = "视频详细检索仍在构建中，暂时无法回答具体内容问题，请稍后再试。"
		}
	}
	return generatedReply{
		Content: content, Route: RouteInfo{Mode: mode, Intent: decision.Intent}, Decision: decision,
		Capabilities: capabilities.View(), IntentPromptVersion: intentPromptVersion,
		AnswerPromptVersion: "capability-template-v1", Latency: map[string]time.Duration{"intent": intentLatency},
	}
}
