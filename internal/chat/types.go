package chat

import (
	"errors"
	"time"

	"readergo/internal/model"
)

const (
	IntentGeneralChat   = "GENERAL_CHAT"
	IntentVideoOverview = "VIDEO_OVERVIEW"
	IntentVideoDetail   = "VIDEO_DETAIL"
	IntentAmbiguous     = "AMBIGUOUS"

	AmbiguityNone             = "NONE"
	AmbiguityLowConfidence    = "LOW_CONFIDENCE"
	AmbiguityMultiIntent      = "MULTI_INTENT"
	AmbiguityMissingReference = "MISSING_REFERENCE"
	AmbiguityClassifierError  = "CLASSIFIER_ERROR"

	CapabilityNone    = "NONE"
	CapabilitySummary = "SUMMARY"
	CapabilityRAG     = "RAG"

	ModePreset            = "preset"
	ModeGeneral           = "general"
	ModeGeneralUnaware    = "general_unaware"
	ModeSummary           = "summary"
	ModeRAG               = "rag"
	ModeClarify           = "clarify"
	ModeCapabilityPending = "capability_pending"
	ModeCapabilityFailed  = "capability_failed"

	CapabilityStateUnknown    = "unknown"
	CapabilityStateProcessing = "processing"
	CapabilityStateReady      = "ready"
	CapabilityStateFailed     = "failed"
)

var ErrConversationBusy = errors.New("conversation already has a generating message")

type HistoryTurn struct {
	User      string `json:"user"`
	Assistant string `json:"assistant"`
}

type SummaryContext struct {
	Overview      string
	ChapterTitles []string
}

type Capabilities struct {
	VideoTitle   string
	SummaryState string
	RAGState     string
	SummaryError string
	RAGError     string
	Summary      SummaryContext
}

func (c Capabilities) SummaryReady() bool { return c.SummaryState == CapabilityStateReady }
func (c Capabilities) RAGReady() bool     { return c.RAGState == CapabilityStateReady }

type CapabilityView struct {
	Summary string `json:"summary"`
	RAG     string `json:"rag"`
}

func (c Capabilities) View() CapabilityView {
	return CapabilityView{Summary: c.SummaryState, RAG: c.RAGState}
}

type IntentDecision struct {
	Intent             string            `json:"intent"`
	Confidence         float64           `json:"confidence"`
	IsMultiIntent      bool              `json:"is_multi_intent"`
	AmbiguityReason    string            `json:"ambiguity_reason"`
	RequiredCapability string            `json:"required_capability"`
	ResolvedReferences map[string]string `json:"resolved_references"`
	RewrittenQuery     string            `json:"rewritten_query"`
}

type Citation struct {
	SegmentID string `json:"segment_id"`
	StartMS   int64  `json:"start_ms"`
	EndMS     int64  `json:"end_ms"`
	Label     string `json:"label"`
}

type RouteAction struct {
	Type  string `json:"type"`
	Label string `json:"label"`
}

type RouteInfo struct {
	Mode   string       `json:"mode"`
	Intent string       `json:"intent"`
	Notice string       `json:"notice"`
	Action *RouteAction `json:"action"`
}

type ReplyInput struct {
	TenantID        string
	Conversation    model.Conversation
	ClientMessageID string
	Content         string
}

type ReplyResult struct {
	UserMessage      model.Message  `json:"user_message"`
	AssistantMessage model.Message  `json:"assistant_message"`
	Route            RouteInfo      `json:"route"`
	Capabilities     CapabilityView `json:"capabilities"`
}

type MessageMetadata struct {
	Mode                string         `json:"mode"`
	Intent              string         `json:"intent"`
	IntentConfidence    float64        `json:"intent_confidence"`
	AmbiguityReason     string         `json:"ambiguity_reason"`
	Notice              string         `json:"notice"`
	Action              *RouteAction   `json:"action,omitempty"`
	SummaryState        string         `json:"summary_state"`
	RAGState            string         `json:"rag_state"`
	IntentPromptVersion string         `json:"intent_prompt_version,omitempty"`
	AnswerPromptVersion string         `json:"answer_prompt_version,omitempty"`
	LatencyMS           map[string]int `json:"latency_ms,omitempty"`
}

type generatedReply struct {
	Content             string
	Citations           []Citation
	Route               RouteInfo
	Decision            IntentDecision
	Capabilities        CapabilityView
	IntentPromptVersion string
	AnswerPromptVersion string
	Latency             map[string]time.Duration
}
