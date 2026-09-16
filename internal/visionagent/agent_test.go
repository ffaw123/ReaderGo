package visionagent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"readergo/internal/model"
)

type fakeDataRepository struct{ memories []Memory }

func (f fakeDataRepository) loadVideo(context.Context, string, string) (model.Video, error) {
	return model.Video{ID: "video-1", Status: model.VideoStatusReady, ObjectKey: "video.mp4", DurationMS: 60_000}, nil
}
func (f fakeDataRepository) loadWindows(context.Context, string, string, Range) ([]RetrievalWindow, error) {
	return []RetrievalWindow{{SegmentID: "segment-1", StartMS: 0, EndMS: 60_000, RetrievalText: "原始检索文本"}}, nil
}
func (f fakeDataRepository) loadMemories(context.Context, Request) ([]Memory, error) {
	return f.memories, nil
}

type agentFakeVideoSource struct{}

func (agentFakeVideoSource) LocalPath(context.Context, model.Video) (string, func(), error) {
	return "fake-video.mp4", func() {}, nil
}

type agentFakeFrameExtractor struct{}

func (agentFakeFrameExtractor) ExtractFrameAt(_ context.Context, _ string, output string, _ time.Duration) error {
	return os.WriteFile(output, []byte("jpeg"), 0o600)
}

type fakeOrchestrator struct {
	turns     []OrchestratorTurn
	index     int
	inputs    []OrchestratorInput
	histories [][]LoopMessage
}

func (f *fakeOrchestrator) Next(_ context.Context, input OrchestratorInput, history []LoopMessage) (OrchestratorTurn, error) {
	f.inputs = append(f.inputs, input)
	f.histories = append(f.histories, append([]LoopMessage(nil), history...))
	turn := f.turns[f.index]
	f.index++
	return turn, nil
}
func (*fakeOrchestrator) Model() string { return "mimo-v2.5-pro" }

type fakeObserver struct {
	values []Observation
	index  int
	frames [][]Frame
	inputs []ObserverInput
}

func (f *fakeObserver) Observe(_ context.Context, input ObserverInput, frames []Frame) (Observation, error) {
	f.frames = append(f.frames, append([]Frame(nil), frames...))
	f.inputs = append(f.inputs, input)
	value := f.values[f.index]
	f.index++
	return value, nil
}
func (*fakeObserver) Model() string { return "qwen3-vl-flash" }

func newTestAgent(orchestrator orchestratorClient, observer visionObserverClient) *Agent {
	return &Agent{
		repository: fakeDataRepository{}, source: agentFakeVideoSource{}, ffmpeg: agentFakeFrameExtractor{},
		orchestrator: orchestrator, observer: observer,
		totalTimeout: time.Second, limiter: NewLocalLimiter(1, 100, time.Minute),
	}
}

func toolTurn(id string, start, end, interval int64) OrchestratorTurn {
	return OrchestratorTurn{Content: "需要检查关键画面", Tool: &ToolCall{
		ID: id, Name: ToolName, Arguments: InspectVideoRangeInput{StartMS: start, EndMS: end, IntervalMS: interval},
	}}
}

func finalTurn(status, answer string, ids ...string) OrchestratorTurn {
	decision := FinalDecision{Action: ActionFinal, EvidenceStatus: status, Answer: answer, UsedObservationIDs: ids}
	return OrchestratorTurn{Content: mustJSON(decision), Final: &decision}
}

func baseRequest() Request {
	return Request{
		TenantID: "tenant-1", VideoID: "video-1", ConversationID: "conversation-1",
		Question: "这个人在做什么？", RewrittenQuery: "人物动作", Range: Range{StartMS: 0, EndMS: 10_000},
	}
}

func TestAgentUsesMiMoToolCallThenQwenObservation(t *testing.T) {
	orchestrator := &fakeOrchestrator{turns: []OrchestratorTurn{
		toolTurn("call-1", 0, 10_000, 5_000),
		finalTurn("GROUNDED", "人物举起了杯子。", "obs-1"),
	}}
	observer := &fakeObserver{values: []Observation{{Observation: "人物举起杯子"}}}
	agent := newTestAgent(orchestrator, observer)

	result, err := agent.Run(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusGrounded || result.Answer != "人物举起了杯子。" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if len(result.UsedFrameTimestampsMS) != 2 || result.UsedFrameTimestampsMS[0] != 0 || result.UsedFrameTimestampsMS[1] != 5_000 {
		t.Fatalf("used source frames = %v, want all sampled frames from obs-1", result.UsedFrameTimestampsMS)
	}
	if result.OrchestratorModel != "mimo-v2.5-pro" || result.VisionModel != "qwen3-vl-flash" {
		t.Fatalf("model audit fields missing: %#v", result)
	}
	if len(orchestrator.histories) != 2 || len(orchestrator.histories[1]) != 2 {
		t.Fatalf("loop history = %#v", orchestrator.histories)
	}
	if orchestrator.histories[1][0].Role != "assistant" || orchestrator.histories[1][1].Role != "tool" ||
		orchestrator.histories[1][1].ToolCallID != "call-1" || !strings.Contains(orchestrator.histories[1][1].Content, "obs-1") {
		t.Fatalf("not a standard assistant/tool chain: %#v", orchestrator.histories[1])
	}
	if strings.Contains(orchestrator.histories[1][1].Content, "evidence_status") ||
		strings.Contains(orchestrator.histories[1][1].Content, "used_frame_timestamps_ms") {
		t.Fatalf("observer-owned evidence fields leaked into tool result: %s", orchestrator.histories[1][1].Content)
	}
	if len(observer.inputs) != 1 || observer.inputs[0].Question != "这个人在做什么？" || observer.inputs[0].IntervalMS != 5_000 {
		t.Fatalf("observer input = %#v, want question with the tool call interval", observer.inputs)
	}
}

func TestAgentCanRefineAfterObservation(t *testing.T) {
	orchestrator := &fakeOrchestrator{turns: []OrchestratorTurn{
		toolTurn("call-1", 0, 10_000, 5_000),
		toolTurn("call-2", 4_000, 6_000, 500),
		finalTurn("GROUNDED", "动作发生在五秒附近。", "obs-2"),
	}}
	observer := &fakeObserver{values: []Observation{
		{Observation: "当前画面未呈现与问题相关的明确动作。"},
		{Observation: "人物在五秒附近举杯"},
	}}
	result, err := newTestAgent(orchestrator, observer).Run(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.ToolCalls != 2 || len(result.UsedObservationIDs) != 1 || result.UsedObservationIDs[0] != "obs-2" {
		t.Fatalf("refinement result = %#v", result)
	}
}

func TestAgentContinuesSameToolCallAfterRangeConfirmation(t *testing.T) {
	orchestrator := &fakeOrchestrator{turns: []OrchestratorTurn{
		toolTurn("call-1", 18_000, 28_000, 5_000),
		finalTurn("GROUNDED", "人物继续向前行走。", "obs-1"),
	}}
	observer := &fakeObserver{values: []Observation{{Observation: "人物继续向前行走"}}}
	agent := newTestAgent(orchestrator, observer)
	request := baseRequest()
	request.Range = Range{StartMS: 13_000, EndMS: 18_000}
	request.OnRangeConfirmation = func(value RangeConfirmationRequest) (RangeConfirmationHandle, error) {
		return RangeConfirmationHandle{
			ID: "confirmation-1", Wait: func(context.Context) (Range, error) { return value.SuggestedRange, nil }, Cancel: func() {},
		}, nil
	}
	result, err := agent.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Range != (Range{StartMS: 18_000, EndMS: 28_000}) || result.ToolCalls != 1 {
		t.Fatalf("confirmation result = %#v", result)
	}
	if len(orchestrator.histories[1]) != 2 || orchestrator.histories[1][1].ToolCallID != "call-1" {
		t.Fatalf("tool call was not resumed: %#v", orchestrator.histories[1])
	}
}

func TestAgentRejectsGroundedWithoutToolResult(t *testing.T) {
	orchestrator := &fakeOrchestrator{turns: []OrchestratorTurn{
		finalTurn("GROUNDED", "伪造回答", "obs-1"),
		finalTurn("GROUNDED", "伪造回答", "obs-1"),
	}}
	_, err := newTestAgent(orchestrator, &fakeObserver{}).Run(context.Background(), baseRequest())
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("error = %v, want ErrInvalidOutput", err)
	}
	if orchestrator.index != 2 {
		t.Fatalf("orchestrator retries = %d, want 2", orchestrator.index)
	}
	if len(orchestrator.inputs) != 2 || orchestrator.inputs[1].ProtocolInstruction == "" {
		t.Fatalf("retry did not include protocol correction: %#v", orchestrator.inputs)
	}
}

func TestAgentRejectsNoEvidenceBeforeInitialInspection(t *testing.T) {
	orchestrator := &fakeOrchestrator{turns: []OrchestratorTurn{
		finalTurn("NO_EVIDENCE", "尚未检查画面。"), finalTurn("NO_EVIDENCE", "尚未检查画面。"),
	}}
	_, err := newTestAgent(orchestrator, &fakeObserver{}).Run(context.Background(), baseRequest())
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("error = %v, want ErrInvalidOutput", err)
	}
	if orchestrator.index != 2 {
		t.Fatalf("orchestrator retries = %d, want 2", orchestrator.index)
	}
	if len(orchestrator.inputs) != 2 || orchestrator.inputs[1].ProtocolInstruction == "" {
		t.Fatalf("retry did not include protocol correction: %#v", orchestrator.inputs)
	}
}

func TestAgentRejectsForgedObservationID(t *testing.T) {
	orchestrator := &fakeOrchestrator{turns: []OrchestratorTurn{
		toolTurn("call-1", 0, 10_000, 5_000),
		finalTurn("GROUNDED", "伪造引用", "obs-999"),
		finalTurn("GROUNDED", "伪造引用", "obs-999"),
	}}
	observer := &fakeObserver{values: []Observation{{Observation: "人物出现"}}}
	_, err := newTestAgent(orchestrator, observer).Run(context.Background(), baseRequest())
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("error = %v, want ErrInvalidOutput", err)
	}
}

func TestAgentStopsAtThreeCallBudget(t *testing.T) {
	orchestrator := &fakeOrchestrator{turns: []OrchestratorTurn{
		toolTurn("call-1", 0, 1_000, 500),
		toolTurn("call-2", 1_000, 2_000, 500),
		toolTurn("call-3", 2_000, 3_000, 500),
		toolTurn("call-4", 3_000, 4_000, 500),
	}}
	observer := &fakeObserver{values: []Observation{
		{Observation: "未观察到与问题相关的可见内容。"},
		{Observation: "未观察到与问题相关的可见内容。"},
		{Observation: "未观察到与问题相关的可见内容。"},
	}}
	result, err := newTestAgent(orchestrator, observer).Run(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusNoEvidence || result.ToolCalls != 3 || observer.index != 3 {
		t.Fatalf("budget result = %#v, observer calls = %d", result, observer.index)
	}
	if len(orchestrator.inputs) != 4 || !orchestrator.inputs[3].ForceFinal {
		t.Fatalf("budget exhaustion did not force final generation: %#v", orchestrator.inputs)
	}
}

func TestSafeTraceReasonRedactsSensitiveAndLongContent(t *testing.T) {
	fallback := safeTraceReason(`读取 C:\private\video.mp4 并访问 https://internal.example`)
	if strings.Contains(fallback, "private") || strings.Contains(fallback, "http") {
		t.Fatalf("sensitive reason was not replaced: %q", fallback)
	}
	if got := []rune(safeTraceReason(strings.Repeat("理由", 150))); len(got) != 200 {
		t.Fatalf("safe reason length = %d, want 200", len(got))
	}
}
