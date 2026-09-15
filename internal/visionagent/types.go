package visionagent

import (
	"context"
	"time"
)

const (
	StatusGrounded   = "grounded"
	StatusNoEvidence = "no_evidence"

	ActionCallTool = "CALL_TOOL"
	ActionFinal    = "FINAL"
	ToolName       = "inspect_video_range"

	InitialIntervalMS       int64 = 5_000
	MinIntervalMS           int64 = 500
	MaxRangeMS              int64 = 30_000
	MaxToolCalls                  = 2
	MaxUniqueFrames               = 18
	MaxMemoryTurns                = 4
	MaxMemoryRunes                = 6_000
	MaxObservationRunes           = 4_000
	MaxCombinedContextRunes       = 24_000

	AgentPromptVersion  = "vision-agent-v1"
	AnswerPromptVersion = "vision-answer-v1"
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
	TenantID       string
	VideoID        string
	ConversationID string
	UserMessageID  string
	Question       string
	RewrittenQuery string
	Range          Range
	History        []HistoryTurn
	OnProgress     ProgressFunc
}

type Progress struct {
	Stage   string `json:"stage"`
	Message string `json:"message"`
}

type ProgressFunc func(Progress) error

type Result struct {
	Answer                   string
	Status                   string
	Observation              string
	SampledFrameTimestampsMS []int64
	UsedFrameTimestampsMS    []int64
	ToolCalls                int
	Model                    string
	PromptVersion            string
	AnswerPromptVersion      string
	SegmentIDsByTimestamp    map[int64]string
	Latency                  map[string]time.Duration
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
	Name      string                 `json:"name"`
	Arguments InspectVideoRangeInput `json:"arguments"`
}

// ToolTrace 是本轮工作记忆中的结构化工具结果，不包含本地路径或图片数据。
type ToolTrace struct {
	Reason               string                 `json:"reason,omitempty"`
	Arguments            InspectVideoRangeInput `json:"arguments"`
	OK                   bool                   `json:"ok"`
	ErrorCode            string                 `json:"error_code,omitempty"`
	Message              string                 `json:"message,omitempty"`
	NewFrameTimestampsMS []int64                `json:"new_frame_timestamps_ms,omitempty"`
	RemainingToolCalls   int                    `json:"remaining_tool_calls"`
	RemainingFrameBudget int                    `json:"remaining_frame_budget"`
}

type Decision struct {
	Action                string    `json:"action"`
	Reason                string    `json:"reason,omitempty"`
	Tool                  *ToolCall `json:"tool,omitempty"`
	EvidenceStatus        string    `json:"evidence_status,omitempty"`
	Observation           string    `json:"observation,omitempty"`
	UsedFrameTimestampsMS []int64   `json:"used_frame_timestamps_ms,omitempty"`
}

type FinalAnswer struct {
	Answer                string  `json:"answer"`
	UsedFrameTimestampsMS []int64 `json:"used_frame_timestamps_ms"`
}

type Frame struct {
	TimestampMS int64
	DisplayTime string
	Path        string
}

type Memory struct {
	StartMS             int64   `json:"start_ms"`
	EndMS               int64   `json:"end_ms"`
	Status              string  `json:"status"`
	Observation         string  `json:"observation"`
	UsedFrameTimestamps []int64 `json:"used_frame_timestamps_ms"`
}

type RetrievalWindow struct {
	SegmentID     string `json:"segment_id"`
	StartMS       int64  `json:"start_ms"`
	EndMS         int64  `json:"end_ms"`
	RetrievalText string `json:"retrieval_text"`
}
