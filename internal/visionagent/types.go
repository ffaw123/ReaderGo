package visionagent

import (
	"context"
	"time"
)

const (
	StatusGrounded   = "grounded"
	StatusNoEvidence = "no_evidence"

	ActionFinal = "FINAL"
	ToolName    = "inspect_video_range"

	TraceAgentDeciding = "AGENT_DECIDING"
	TraceInspect       = "INSPECT_VIDEO_RANGE"
	TraceObserving     = "VISION_OBSERVING"
	TraceRangeRequest  = "REQUEST_RANGE_CONFIRMATION"
	TraceVisionResult  = "VISION_RESULT"
	TraceFinal         = "FINAL"

	MinIntervalMS        int64 = 500
	MaxIntervalMS        int64 = 10_000
	MaxRangeMS           int64 = 30_000
	MaxToolCalls               = 3
	MaxUniqueFrames            = 18
	MinObservationFrames       = 4
	MaxDialogueTurns           = 3
	MaxObservationRunes        = 4_000
	MaxToolReasonRunes         = 200
	MaxFinalAnswerRunes        = 4_000

	OrchestratorPromptVersion = "vision-orchestrator-v9"
	ObserverPromptVersion     = "vision-observer-v3"
)

type Range struct {
	StartMS int64 `json:"start_ms"`
	EndMS   int64 `json:"end_ms"`
}

type HistoryTurn struct {
	User      string `json:"user"`
	Assistant string `json:"assistant"`
}

type Request struct {
	TenantID            string
	VideoID             string
	ConversationID      string
	Question            string
	RewrittenQuery      string
	Range               Range
	History             []HistoryTurn
	OnProgress          ProgressFunc
	OnTrace             TraceFunc
	OnRangeConfirmation PrepareRangeConfirmationFunc
}

type Progress struct {
	Stage   string `json:"stage"`
	Message string `json:"message"`
}

type ProgressFunc func(Progress) error

// AgentTrace 是允许展示给用户的结构化操作记录，不包含模型隐藏推理或本地文件路径。
type AgentTrace struct {
	ID                    string                  `json:"id"`
	Step                  int                     `json:"step"`
	Status                string                  `json:"status"`
	Action                string                  `json:"action"`
	Summary               string                  `json:"summary"`
	Reason                string                  `json:"reason,omitempty"`
	Arguments             *InspectVideoRangeInput `json:"arguments,omitempty"`
	Observation           string                  `json:"observation,omitempty"`
	NewFrameTimestampsMS  []int64                 `json:"new_frame_timestamps_ms,omitempty"`
	UsedFrameTimestampsMS []int64                 `json:"used_frame_timestamps_ms,omitempty"`
	RemainingToolCalls    int                     `json:"remaining_tool_calls"`
	RemainingFrameBudget  int                     `json:"remaining_frame_budget"`
	EvidenceStatus        string                  `json:"evidence_status,omitempty"`
	ConfirmationID        string                  `json:"confirmation_id,omitempty"`
}

type TraceFunc func(AgentTrace) error

type RangeConfirmationRequest struct {
	CurrentRange   Range
	SuggestedRange Range
}

// RangeConfirmationHandle 先完成服务端登记，再让 Agent 推送带 ID 的等待轨迹，避免前端确认时发生竞态。
type RangeConfirmationHandle struct {
	ID     string
	Wait   func(context.Context) (Range, error)
	Cancel func()
}

type PrepareRangeConfirmationFunc func(RangeConfirmationRequest) (RangeConfirmationHandle, error)

type Result struct {
	Answer                    string
	Status                    string
	Observation               string
	SampledFrameTimestampsMS  []int64
	UsedFrameTimestampsMS     []int64
	UsedObservationIDs        []string
	ToolCalls                 int
	OrchestratorModel         string
	ObserverModel             string
	OrchestratorPromptVersion string
	ObserverPromptVersion     string
	SegmentIDsByTimestamp     map[int64]string
	Latency                   map[string]time.Duration
	Trace                     []AgentTrace
	Range                     Range
}

type Runner interface {
	Run(context.Context, Request) (Result, error)
	Available() bool
	MaxRunDuration() time.Duration
}

type InspectVideoRangeInput struct {
	StartMS    int64 `json:"start_ms"`
	EndMS      int64 `json:"end_ms"`
	IntervalMS int64 `json:"interval_ms"`
}

type ToolCall struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	Arguments InspectVideoRangeInput `json:"arguments"`
}

type OrchestratorInput struct {
	Question            string            `json:"question"`
	RewrittenQuery      string            `json:"rewritten_query,omitempty"`
	AllowedRange        Range             `json:"allowed_range"`
	ProtocolInstruction string            `json:"protocol_instruction,omitempty"`
	ForceFinal          bool              `json:"-"`
	RetrievalWindows    []RetrievalWindow `json:"retrieval_windows"`
}

// LoopMessage 仅表示同一 Agent Loop 内需要回传给主模型的标准 assistant/tool 消息。
type LoopMessage struct {
	Role       string
	Content    string
	ToolCallID string
	Tool       *ToolCall
}

type OrchestratorTurn struct {
	Content string
	Tool    *ToolCall
	Final   *FinalDecision
}

type FinalDecision struct {
	Action         string `json:"action"`
	EvidenceStatus string `json:"evidence_status"`
	Answer         string `json:"answer"`
}

type ObserverInput struct {
	Question   string `json:"question"`
	IntervalMS int64  `json:"interval_ms"`
}

type Observation struct {
	Observation string
}

type ToolResult struct {
	OK bool `json:"ok"`
	// Range 使用指针，失败结果不会向主 Agent 暴露无意义的零值区间。
	Range                    *Range  `json:"range,omitempty"`
	IntervalMS               int64   `json:"interval_ms,omitempty"`
	SampledFrameTimestampsMS []int64 `json:"sampled_frame_timestamps_ms,omitempty"`
	Observation              string  `json:"observation,omitempty"`
	RemainingVisionCalls     int     `json:"remaining_vision_calls"`
	RemainingFrameBudget     int     `json:"remaining_frame_budget"`
	ErrorCode                string  `json:"error_code,omitempty"`
	Message                  string  `json:"message,omitempty"`
}

type Frame struct {
	TimestampMS int64
	// Path 仅供后端读取帧文件，禁止进入任何模型输入或 JSON 记录。
	Path string `json:"-"`
}

type RetrievalWindow struct {
	SegmentID     string `json:"segment_id"`
	StartMS       int64  `json:"start_ms"`
	EndMS         int64  `json:"end_ms"`
	RetrievalText string `json:"retrieval_text"`
}
