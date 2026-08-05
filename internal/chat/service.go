package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

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
}

type Service struct {
	repository    *repository
	shortcuts     shortcutMatcher
	intent        *intentClassifier
	answerClient  *textClient
	retriever     *retriever
	maxInputRunes int
	historyTurns  int
}

func NewService(db *gorm.DB, config ServiceConfig) (*Service, error) {
	if db == nil {
		return nil, errors.New("chat service requires database")
	}
	answerClient, err := newTextClient("CHAT", config.ChatBaseURL, config.ChatAPIKey, config.ChatModel, config.ChatRequestTimeout)
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
	return &Service{
		repository: &repository{
			db: db,
			generationLease: config.IntentRequestTimeout + config.EmbeddingRequestTimeout +
				config.ChatRequestTimeout + 30*time.Second,
		},
		shortcuts:     newShortcutMatcher(),
		intent:        &intentClassifier{client: intentClient, threshold: config.IntentConfidenceThreshold},
		answerClient:  answerClient,
		retriever:     retriever,
		maxInputRunes: config.MaxInputRunes,
		historyTurns:  config.HistoryTurns,
	}, nil
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

func (s *Service) Reply(ctx context.Context, in ReplyInput) (result ReplyResult, returnErr error) {
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
		return replyResultFromExisting(begin.UserMessage, *begin.AssistantMessage)
	}
	// 生成失败时保留用户问题并标记为可重试，客户端用同一 client_message_id 重试。
	defer func() {
		if returnErr != nil {
			s.repository.MarkUserMessageFailed(context.WithoutCancel(ctx), begin.UserMessage.ID)
		}
	}()

	reply, err := s.generateReply(ctx, in)
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

func (s *Service) generateReply(ctx context.Context, in ReplyInput) (generatedReply, error) {
	if preset, ok := s.shortcuts.Match(in.Content); ok {
		decision := IntentDecision{Intent: IntentGeneralChat, Confidence: 1, AmbiguityReason: AmbiguityNone, RequiredCapability: CapabilityNone}
		return generatedReply{
			Content: preset.Content, Route: RouteInfo{Mode: ModePreset, Intent: IntentGeneralChat}, Decision: decision,
			Capabilities:        CapabilityView{Summary: CapabilityStateUnknown, RAG: CapabilityStateUnknown},
			AnswerPromptVersion: "preset-" + preset.Rule + "-v1",
		}, nil
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
		answer, err := s.answerGeneral(ctx, history, in.Content, true)
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
		return reply, nil
	}

	switch decision.Intent {
	case IntentGeneralChat:
		started := time.Now()
		answer, err := s.answerGeneral(ctx, history, in.Content, false)
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
			return capabilityReply(decision, capabilities, intentLatency), nil
		}
		started := time.Now()
		answer, err := s.answerSummary(ctx, history, in.Content, capabilities.VideoTitle, capabilities.Summary)
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
			return capabilityReply(decision, capabilities, intentLatency), nil
		}
		reply, err := s.answerRAG(ctx, in.TenantID, in.Conversation.VideoID, history, in.Content, decision)
		if err != nil {
			return generatedReply{}, err
		}
		reply.Capabilities = capabilities.View()
		reply.IntentPromptVersion = intentPromptVersion
		reply.Latency["intent"] = intentLatency
		return reply, nil
	default:
		return ambiguousReply(IntentDecision{Intent: IntentAmbiguous, AmbiguityReason: AmbiguityClassifierError}), nil
	}
}
