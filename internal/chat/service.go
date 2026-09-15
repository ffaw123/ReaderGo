package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"readergo/internal/model"
	"readergo/internal/visionagent"

	"gorm.io/gorm"
)

type ServiceConfig struct {
	ChatAPIKey, ChatBaseURL, ChatModel                string
	ChatRequestTimeout                                time.Duration
	IntentAPIKey, IntentBaseURL, IntentModel          string
	IntentRequestTimeout                              time.Duration
	IntentConfidenceThreshold                         float64
	EmbeddingAPIKey, EmbeddingBaseURL, EmbeddingModel string
	EmbeddingRequestTimeout                           time.Duration
	RAGMinSimilarity                                  float64
	MaxInputRunes, HistoryTurns                       int
	VisionRunner                                      visionagent.Runner
}

type Service struct {
	repository    *repository
	shortcuts     shortcutMatcher
	intent        *intentClassifier
	answerClient  *streamingTextClient
	retriever     *retriever
	maxInputRunes int
	historyTurns  int
	visionRunner  visionagent.Runner
}

func NewService(db *gorm.DB, config ServiceConfig) (*Service, error) {
	if db == nil {
		return nil, errors.New("chat service requires database")
	}
	answerClient, err := newStreamingTextClient("CHAT", config.ChatBaseURL, config.ChatAPIKey, config.ChatModel, config.ChatRequestTimeout)
	if err != nil {
		return nil, err
	}
	intentClient, err := newTextClient("INTENT", config.IntentBaseURL, config.IntentAPIKey, config.IntentModel, config.IntentRequestTimeout)
	if err != nil {
		return nil, err
	}
	retriever, err := newRetriever(db, config.EmbeddingBaseURL, config.EmbeddingAPIKey, config.EmbeddingModel, config.EmbeddingRequestTimeout, config.RAGMinSimilarity)
	if err != nil {
		return nil, err
	}
	if config.IntentConfidenceThreshold <= 0 || config.IntentConfidenceThreshold > 1 {
		return nil, errors.New("intent confidence threshold must be greater than 0 and at most 1")
	}
	if config.MaxInputRunes < 1 || config.HistoryTurns < 1 {
		return nil, errors.New("chat input and history limits must be positive")
	}
	generationLease := config.IntentRequestTimeout + config.EmbeddingRequestTimeout + config.ChatRequestTimeout + 30*time.Second
	if config.VisionRunner != nil {
		generationLease = max(generationLease, config.VisionRunner.MaxRunDuration()+30*time.Second)
	}
	return &Service{
		repository: &repository{
			db:              db,
			generationLease: generationLease,
		},
		shortcuts:     newShortcutMatcher(),
		intent:        &intentClassifier{client: intentClient, threshold: config.IntentConfidenceThreshold},
		answerClient:  answerClient,
		retriever:     retriever,
		maxInputRunes: config.MaxInputRunes,
		historyTurns:  config.HistoryTurns,
		visionRunner:  config.VisionRunner,
	}, nil
}

// ValidateVisionRequest 在开启 Agent 模式时校验功能可用性、视频时长和用户授权区间。
func (s *Service) ValidateVisionRequest(ctx context.Context, tenantID, videoID string, request VisionRequest) error {
	if !request.Enabled {
		return nil
	}
	if request.Range == nil {
		return ErrVisionRangeRequired
	}
	durationMS, err := s.repository.LoadVideoDuration(ctx, tenantID, videoID)
	if err != nil {
		return err
	}
	if err := visionagent.ValidateRange(visionagent.Range{StartMS: request.Range.StartMS, EndMS: request.Range.EndMS}, durationMS); err != nil {
		return err
	}
	if s.visionRunner == nil || !s.visionRunner.Available() {
		return ErrVisionUnavailable
	}
	return nil
}

func (s *Service) ValidateContent(content string) (string, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return "", errors.New("content is required")
	}
	if utf8.RuneCountInString(trimmed) > s.maxInputRunes {
		return "", fmt.Errorf("content exceeds %d characters", s.maxInputRunes)
	}
	return trimmed, nil
}

func (s *Service) VisionAvailable() bool {
	return s.visionRunner != nil && s.visionRunner.Available()
}

func (s *Service) Reply(ctx context.Context, in ReplyInput) (result ReplyResult, returnErr error) {
	return s.reply(ctx, in, nil)
}

// ReplyStream 在生成回答时逐段调用 onDelta，并在模型输出校验通过后保存完整消息。
func (s *Service) ReplyStream(ctx context.Context, in ReplyInput, onDelta func(string) error) (result ReplyResult, returnErr error) {
	return s.reply(ctx, in, onDelta)
}

func (s *Service) reply(ctx context.Context, in ReplyInput, onDelta func(string) error) (result ReplyResult, returnErr error) {
	content, err := s.ValidateContent(in.Content)
	if err != nil {
		return result, err
	}
	in.Content = content
	begin, err := s.repository.BeginUserMessage(ctx, in)
	if err != nil {
		return result, err
	}
	if begin.AssistantMessage != nil {
		result, err := replyResultFromExisting(begin.UserMessage, *begin.AssistantMessage)
		if err != nil {
			return result, err
		}
		if onDelta != nil && result.AssistantMessage.Content != "" {
			if err := onDelta(result.AssistantMessage.Content); err != nil {
				return ReplyResult{}, err
			}
		}
		return result, nil
	}
	// 生成失败时保留用户问题并标记为可重试，客户端用同一 client_message_id 重试。
	defer func() {
		if returnErr != nil {
			s.repository.MarkUserMessageFailed(context.WithoutCancel(ctx), begin.UserMessage.ID)
		}
	}()

	reply, err := s.generateReply(ctx, in, onDelta)
	if err != nil {
		return result, err
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	assistant, err := s.repository.CompleteReply(persistCtx, begin.UserMessage, reply)
	if err != nil {
		return result, fmt.Errorf("persist assistant reply: %w", err)
	}
	return ReplyResult{
		UserMessage: begin.UserMessage, AssistantMessage: assistant,
		Route: reply.Route, Capabilities: reply.Capabilities,
	}, nil
}

func (s *Service) generateReply(ctx context.Context, in ReplyInput, onDelta func(string) error) (generatedReply, error) {
	if !in.Vision.Enabled {
		// Chat 模式完整复用当前问答链路，避免改变现有行为。
		return s.generateChatReply(ctx, in, onDelta)
	}
	// Agent 模式复用现有分类器，仅将 VIDEO_DETAIL 交给画面理解 Agent。
	return s.generateAgentReply(ctx, in, onDelta)
}

func (s *Service) generateChatReply(ctx context.Context, in ReplyInput, onDelta func(string) error) (generatedReply, error) {
	if preset, ok := s.shortcuts.Match(in.Content); ok {
		decision := IntentDecision{Intent: IntentGeneralChat, Confidence: 1, AmbiguityReason: AmbiguityNone, RequiredCapability: CapabilityNone}
		reply := generatedReply{
			Content: preset.Content, Route: RouteInfo{Mode: ModePreset, Intent: IntentGeneralChat}, Decision: decision,
			Capabilities:        CapabilityView{Summary: CapabilityStateUnknown, RAG: CapabilityStateUnknown},
			AnswerPromptVersion: "preset-" + preset.Rule + "-v1",
		}
		return reply, emitReplyContent(reply, onDelta)
	}

	history, err := s.repository.LoadHistory(ctx, in.Conversation.ID, s.historyTurns)
	if err != nil {
		return generatedReply{}, fmt.Errorf("load chat history: %w", err)
	}
	capabilities, err := s.repository.LoadCapabilities(ctx, in.TenantID, in.Conversation.VideoID)
	if err != nil {
		return generatedReply{}, fmt.Errorf("load chat capabilities: %w", err)
	}

	if !capabilities.SummaryReady() && !capabilities.RAGReady() {
		started := time.Now()
		answer, err := s.answerGeneral(ctx, history, in.Content, true, onDelta)
		if err != nil {
			return generatedReply{}, err
		}
		decision := IntentDecision{Intent: IntentGeneralChat, Confidence: 1, AmbiguityReason: AmbiguityNone, RequiredCapability: CapabilityNone}
		return generatedReply{
			Content: answer, Route: RouteInfo{Mode: ModeGeneralUnaware, Intent: IntentGeneralChat, Notice: "当前视频的摘要和详细检索均未准备完成；以下回答未参考当前视频内容。"},
			Decision: decision, Capabilities: capabilities.View(), AnswerPromptVersion: generalUnawarePromptVersion,
			Latency: map[string]time.Duration{"generation": time.Since(started)},
		}, nil
	}

	available := []string{CapabilityNone}
	if capabilities.SummaryReady() {
		available = append(available, CapabilitySummary)
	}
	if capabilities.RAGReady() {
		available = append(available, CapabilityRAG)
	}
	intentStarted := time.Now()
	decision, err := s.intent.Classify(ctx, intentInput{
		History: history, CurrentInput: in.Content, VideoTitle: capabilities.VideoTitle,
		VideoOverview: capabilities.Summary.Overview, AvailableCapabilities: available,
	})
	if err != nil {
		return generatedReply{}, err
	}
	intentLatency := time.Since(intentStarted)

	if decision.Intent == IntentAmbiguous {
		reply := ambiguousReply(decision)
		reply.Capabilities = capabilities.View()
		reply.IntentPromptVersion = intentPromptVersion
		reply.Latency = map[string]time.Duration{"intent": intentLatency}
		return reply, emitReplyContent(reply, onDelta)
	}

	switch decision.Intent {
	case IntentGeneralChat:
		started := time.Now()
		answer, err := s.answerGeneral(ctx, history, in.Content, false, onDelta)
		if err != nil {
			return generatedReply{}, err
		}
		return generatedReply{
			Content:  answer,
			Route:    RouteInfo{Mode: ModeGeneral, Intent: decision.Intent, Notice: "此回答未参考当前视频内容。"},
			Decision: decision, Capabilities: capabilities.View(), IntentPromptVersion: intentPromptVersion,
			AnswerPromptVersion: generalPromptVersion,
			Latency:             map[string]time.Duration{"intent": intentLatency, "generation": time.Since(started)},
		}, nil
	case IntentVideoOverview:
		if !capabilities.SummaryReady() {
			reply := capabilityReply(decision, capabilities, intentLatency)
			return reply, emitReplyContent(reply, onDelta)
		}
		started := time.Now()
		answer, err := s.answerSummary(ctx, history, in.Content, capabilities.VideoTitle, capabilities.Summary, onDelta)
		if err != nil {
			return generatedReply{}, err
		}
		return generatedReply{
			Content:  answer,
			Route:    RouteInfo{Mode: ModeSummary, Intent: decision.Intent, Notice: "此回答基于视频摘要。", Action: &RouteAction{Type: "OPEN_SUMMARY", Label: "查看摘要"}},
			Decision: decision, Capabilities: capabilities.View(), IntentPromptVersion: intentPromptVersion,
			AnswerPromptVersion: summaryAnswerPromptVersion,
			Latency:             map[string]time.Duration{"intent": intentLatency, "generation": time.Since(started)},
		}, nil
	case IntentVideoDetail:
		if !capabilities.RAGReady() {
			reply := capabilityReply(decision, capabilities, intentLatency)
			return reply, emitReplyContent(reply, onDelta)
		}
		reply, err := s.answerRAG(ctx, in.TenantID, in.Conversation.VideoID, history, in.Content, decision, onDelta)
		if err != nil {
			return generatedReply{}, err
		}
		reply.Capabilities = capabilities.View()
		reply.IntentPromptVersion = intentPromptVersion
		reply.Latency["intent"] = intentLatency
		return reply, nil
	default:
		reply := ambiguousReply(IntentDecision{Intent: IntentAmbiguous, AmbiguityReason: AmbiguityClassifierError})
		return reply, emitReplyContent(reply, onDelta)
	}
}

func (s *Service) generateAgentReply(ctx context.Context, in ReplyInput, onDelta func(string) error) (generatedReply, error) {
	if in.Vision.Range == nil {
		return generatedReply{}, ErrVisionRangeRequired
	}
	visionRange := in.Vision.Range
	if preset, ok := s.shortcuts.Match(in.Content); ok {
		decision := IntentDecision{Intent: IntentGeneralChat, Confidence: 1, AmbiguityReason: AmbiguityNone, RequiredCapability: CapabilityNone}
		reply := generatedReply{
			Content: preset.Content, Route: RouteInfo{Mode: ModePreset, Intent: IntentGeneralChat}, Decision: decision,
			Capabilities:        CapabilityView{Summary: CapabilityStateUnknown, RAG: CapabilityStateUnknown},
			AnswerPromptVersion: "preset-" + preset.Rule + "-v1", VisionRequested: true, VisionRange: visionRange,
		}
		return reply, emitReplyContent(reply, onDelta)
	}

	history, err := s.repository.LoadHistory(ctx, in.Conversation.ID, s.historyTurns)
	if err != nil {
		return generatedReply{}, fmt.Errorf("load chat history: %w", err)
	}
	capabilities, err := s.repository.LoadCapabilities(ctx, in.TenantID, in.Conversation.VideoID)
	if err != nil {
		return generatedReply{}, fmt.Errorf("load chat capabilities: %w", err)
	}
	available := []string{CapabilityNone}
	if capabilities.SummaryReady() {
		available = append(available, CapabilitySummary)
	}
	if capabilities.RAGReady() {
		available = append(available, CapabilityRAG)
	}
	intentStarted := time.Now()
	decision, err := s.intent.Classify(ctx, intentInput{
		History: history, CurrentInput: in.Content, VideoTitle: capabilities.VideoTitle,
		VideoOverview: capabilities.Summary.Overview, AvailableCapabilities: available,
		SelectedVisionRange: visionRange,
	})
	if err != nil {
		return generatedReply{}, err
	}
	intentLatency := time.Since(intentStarted)

	decorate := func(reply generatedReply) generatedReply {
		reply.VisionRequested = true
		reply.VisionRange = visionRange
		reply.IntentPromptVersion = agentIntentPromptVersion
		return reply
	}
	if decision.Intent == IntentAmbiguous {
		reply := ambiguousReply(decision)
		reply.Capabilities, reply.IntentPromptVersion = capabilities.View(), agentIntentPromptVersion
		reply.Latency = map[string]time.Duration{"intent": intentLatency}
		reply = decorate(reply)
		return reply, emitReplyContent(reply, onDelta)
	}

	switch decision.Intent {
	case IntentGeneralChat:
		started := time.Now()
		unaware := !capabilities.SummaryReady() && !capabilities.RAGReady()
		answer, err := s.answerGeneral(ctx, history, in.Content, unaware, onDelta)
		if err != nil {
			return generatedReply{}, err
		}
		mode, notice, promptVersion := ModeGeneral, "此回答未参考当前视频内容。", generalPromptVersion
		if unaware {
			mode, notice, promptVersion = ModeGeneralUnaware, "当前视频的摘要和详细检索均未准备完成；以下回答未参考当前视频内容。", generalUnawarePromptVersion
		}
		return decorate(generatedReply{
			Content: answer, Route: RouteInfo{Mode: mode, Intent: decision.Intent, Notice: notice},
			Decision: decision, Capabilities: capabilities.View(), IntentPromptVersion: agentIntentPromptVersion,
			AnswerPromptVersion: promptVersion, Latency: map[string]time.Duration{"intent": intentLatency, "generation": time.Since(started)},
		}), nil
	case IntentVideoOverview:
		if !capabilities.SummaryReady() {
			reply := decorate(capabilityReply(decision, capabilities, intentLatency))
			return reply, emitReplyContent(reply, onDelta)
		}
		started := time.Now()
		answer, err := s.answerSummary(ctx, history, in.Content, capabilities.VideoTitle, capabilities.Summary, onDelta)
		if err != nil {
			return generatedReply{}, err
		}
		return decorate(generatedReply{
			Content:  answer,
			Route:    RouteInfo{Mode: ModeSummary, Intent: decision.Intent, Notice: "此回答基于视频摘要，未调用画面理解。概览类问题无需指定时间区间，可以关闭“画面理解”开关。", Action: &RouteAction{Type: "OPEN_SUMMARY", Label: "查看摘要"}},
			Decision: decision, Capabilities: capabilities.View(), IntentPromptVersion: agentIntentPromptVersion,
			AnswerPromptVersion: summaryAnswerPromptVersion, Latency: map[string]time.Duration{"intent": intentLatency, "generation": time.Since(started)},
		}), nil
	case IntentVideoDetail:
		if !capabilities.RAGReady() {
			reply := decorate(capabilityReply(decision, capabilities, intentLatency))
			return reply, emitReplyContent(reply, onDelta)
		}
		if s.visionRunner == nil || !s.visionRunner.Available() {
			return generatedReply{}, ErrVisionUnavailable
		}
		visionHistory := make([]visionagent.HistoryTurn, len(history))
		for index, turn := range history {
			visionHistory[index] = visionagent.HistoryTurn{User: turn.User, Assistant: turn.Assistant}
		}
		visionResult, err := s.visionRunner.Run(ctx, visionagent.Request{
			TenantID: in.TenantID, VideoID: in.Conversation.VideoID, ConversationID: in.Conversation.ID,
			Question: in.Content, RewrittenQuery: decision.RewrittenQuery,
			Range:   visionagent.Range{StartMS: visionRange.StartMS, EndMS: visionRange.EndMS},
			History: visionHistory, OnProgress: in.OnProgress,
		})
		if err != nil {
			return generatedReply{}, err
		}
		citations := make([]Citation, 0, len(visionResult.UsedFrameTimestampsMS))
		for _, timestamp := range visionResult.UsedFrameTimestampsMS {
			citations = append(citations, Citation{
				SegmentID: visionResult.SegmentIDsByTimestamp[timestamp], StartMS: timestamp, EndMS: timestamp + 1,
				Label: formatCitationTimestamp(timestamp), SourceType: "vision_frame",
			})
		}
		latency := visionResult.Latency
		if latency == nil {
			latency = make(map[string]time.Duration)
		}
		latency["intent"] = intentLatency
		observation := &model.VisionObservation{
			TenantID: in.TenantID, VideoID: in.Conversation.VideoID, ConversationID: in.Conversation.ID,
			StartMS: visionRange.StartMS, EndMS: visionRange.EndMS, Status: visionResult.Status,
			ObservationText:        visionResult.Observation,
			SampledFrameTimestamps: model.NewJSONDocument(visionResult.SampledFrameTimestampsMS),
			UsedFrameTimestamps:    model.NewJSONDocument(visionResult.UsedFrameTimestampsMS),
			ToolCalls:              visionResult.ToolCalls, Model: visionResult.Model, PromptVersion: visionResult.PromptVersion,
		}
		reply := decorate(generatedReply{
			Content: visionResult.Answer, Citations: citations,
			Route:    RouteInfo{Mode: ModeVisionAgent, Intent: decision.Intent, Notice: "此回答结合了所选时间区间的实际画面与视频检索文本。"},
			Decision: decision, Capabilities: capabilities.View(), IntentPromptVersion: agentIntentPromptVersion,
			AnswerPromptVersion: visionResult.AnswerPromptVersion, Latency: latency,
			VisionUsed: true, VisionStatus: visionResult.Status, VisionModel: visionResult.Model,
			VisionPromptVersion: visionResult.PromptVersion, VisionToolCalls: visionResult.ToolCalls,
			VisionFrameTimestampsMS: visionResult.SampledFrameTimestampsMS, VisionObservation: observation,
		})
		return reply, emitReplyContent(reply, onDelta)
	default:
		reply := decorate(ambiguousReply(IntentDecision{Intent: IntentAmbiguous, AmbiguityReason: AmbiguityClassifierError}))
		return reply, emitReplyContent(reply, onDelta)
	}
}

func formatCitationTimestamp(milliseconds int64) string {
	minutes := milliseconds / 60_000
	seconds := (milliseconds % 60_000) / 1_000
	return fmt.Sprintf("%02d:%02d", minutes, seconds)
}

func emitReplyContent(reply generatedReply, onDelta func(string) error) error {
	if onDelta == nil || reply.Content == "" {
		return nil
	}
	return onDelta(reply.Content)
}
