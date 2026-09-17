package visionagent

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"readergo/internal/model"
)

type fakeDataRepository struct{}

func (f fakeDataRepository) loadVideo(context.Context, string, string) (model.Video, error) {
	return model.Video{ID: "video-1", Status: model.VideoStatusReady, ObjectKey: "video.mp4", DurationMS: 60_000}, nil
}
func (f fakeDataRepository) loadWindows(context.Context, string, string, Range) ([]RetrievalWindow, error) {
	return []RetrievalWindow{{SegmentID: "segment-1", StartMS: 0, EndMS: 60_000, RetrievalText: "原始检索文本"}}, nil
}

type agentFakeVideoSource struct{}

func (agentFakeVideoSource) LocalPath(context.Context, model.Video) (string, func(), error) {
	return "fake-video.mp4", func() {}, nil
}

type agentFakeFrameExtractor struct{ calls *int }

func (f agentFakeFrameExtractor) ExtractFrameAt(_ context.Context, _ string, output string, _ time.Duration) error {
	if f.calls != nil {
		(*f.calls)++
	}
	return os.WriteFile(output, []byte("jpeg"), 0o600)
}

type fakeOrchestrator struct {
	turns     []OrchestratorTurn
	index     int
	inputs    []OrchestratorInput
	dialogues [][]HistoryTurn
	histories [][]LoopMessage
}

func (f *fakeOrchestrator) Next(_ context.Context, input OrchestratorInput, dialogue []HistoryTurn, history []LoopMessage) (OrchestratorTurn, error) {
	f.inputs = append(f.inputs, input)
	f.dialogues = append(f.dialogues, append([]HistoryTurn(nil), dialogue...))
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

func finalTurn(status, answer string) OrchestratorTurn {
	decision := FinalDecision{Action: ActionFinal, EvidenceStatus: status, Answer: answer}
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
		finalTurn("GROUNDED", "人物举起了杯子。"),
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
	if !slices.Equal(result.UsedFrameTimestampsMS, []int64{0, 3_333, 6_666, 9_999}) {
		t.Fatalf("used source frames = %v, want all sampled frames from obs-1", result.UsedFrameTimestampsMS)
	}
	if !slices.Equal(result.UsedObservationIDs, []string{"obs-1"}) {
		t.Fatalf("server-owned observation IDs = %v, want [obs-1]", result.UsedObservationIDs)
	}
	if result.OrchestratorModel != "mimo-v2.5-pro" || result.ObserverModel != "qwen3-vl-flash" {
		t.Fatalf("model audit fields missing: %#v", result)
	}
	if len(orchestrator.histories) != 2 || len(orchestrator.histories[1]) != 2 {
		t.Fatalf("loop history = %#v", orchestrator.histories)
	}
	if orchestrator.histories[1][0].Role != "assistant" || orchestrator.histories[1][1].Role != "tool" ||
		orchestrator.histories[1][1].ToolCallID != "call-1" {
		t.Fatalf("not a standard assistant/tool chain: %#v", orchestrator.histories[1])
	}
	if strings.Contains(orchestrator.histories[1][1].Content, "observation_id") ||
		strings.Contains(orchestrator.histories[1][1].Content, "evidence_status") ||
		strings.Contains(orchestrator.histories[1][1].Content, "used_frame_timestamps_ms") {
		t.Fatalf("server-owned evidence fields leaked into tool result: %s", orchestrator.histories[1][1].Content)
	}
	if !strings.Contains(orchestrator.histories[1][1].Content, `"observation"`) {
		t.Fatalf("tool result is missing the visual observation: %s", orchestrator.histories[1][1].Content)
	}
	if !strings.Contains(orchestrator.histories[1][1].Content, `"interval_ms":3333`) {
		t.Fatalf("tool result did not report the effective interval: %s", orchestrator.histories[1][1].Content)
	}
	if len(orchestrator.inputs) == 0 || orchestrator.inputs[0].Question != "这个人在做什么？" || orchestrator.inputs[0].RewrittenQuery != "人物动作" {
		t.Fatalf("orchestrator must receive both original and rewritten questions: %#v", orchestrator.inputs)
	}
	if len(observer.inputs) != 1 || observer.inputs[0].Question != "人物动作" || observer.inputs[0].IntervalMS != 3_333 {
		t.Fatalf("observer input = %#v, want rewritten query with the effective sampling interval", observer.inputs)
	}
}

func TestAgentUsesLastThreeCompleteDialogueTurns(t *testing.T) {
	orchestrator := &fakeOrchestrator{turns: []OrchestratorTurn{
		toolTurn("call-1", 0, 10_000, 5_000),
		finalTurn("GROUNDED", "已结合当前画面回答。"),
	}}
	observer := &fakeObserver{values: []Observation{{Observation: "当前画面观察"}}}
	request := baseRequest()
	request.History = []HistoryTurn{
		{User: "第一轮问题", Assistant: "第一轮普通回答"},
		{User: "第二轮问题", Assistant: "第二轮摘要回答"},
		{User: "第三轮问题", Assistant: "第三轮 RAG 回答"},
		{User: "第四轮问题", Assistant: "第四轮画面回答"},
	}

	if _, err := newTestAgent(orchestrator, observer).Run(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	want := request.History[1:]
	if len(orchestrator.dialogues) != 2 {
		t.Fatalf("orchestrator dialogue calls = %d, want 2", len(orchestrator.dialogues))
	}
	for index, got := range orchestrator.dialogues {
		if !slices.Equal(got, want) {
			t.Fatalf("dialogue[%d] = %#v, want %#v", index, got, want)
		}
	}
}

func TestFailedToolResultOmitsUnavailableEvidenceFields(t *testing.T) {
	message := toolResultMessage("call-1", ToolResult{
		OK: false, ErrorCode: "RANGE_INVALID", Message: "区间无效",
		RemainingVisionCalls: 2, RemainingFrameBudget: 18,
	})
	if strings.Contains(message.Content, `"range"`) || strings.Contains(message.Content, "observation_id") {
		t.Fatalf("failed tool result leaked unavailable evidence fields: %s", message.Content)
	}
}

func TestAgentCanRefineAfterObservation(t *testing.T) {
	orchestrator := &fakeOrchestrator{turns: []OrchestratorTurn{
		toolTurn("call-1", 0, 10_000, 5_000),
		toolTurn("call-2", 4_000, 6_000, 500),
		finalTurn("GROUNDED", "动作发生在五秒附近。"),
	}}
	observer := &fakeObserver{values: []Observation{
		{Observation: "当前画面未呈现与问题相关的明确动作。"},
		{Observation: "人物在五秒附近举杯"},
	}}
	result, err := newTestAgent(orchestrator, observer).Run(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.ToolCalls != 2 || !slices.Equal(result.UsedObservationIDs, []string{"obs-1", "obs-2"}) {
		t.Fatalf("refinement result = %#v", result)
	}
	wantFrames := []int64{0, 3_333, 4_000, 4_500, 5_000, 5_500, 6_666, 9_999}
	if !slices.Equal(result.UsedFrameTimestampsMS, wantFrames) {
		t.Fatalf("server-owned evidence frames = %v, want %v", result.UsedFrameTimestampsMS, wantFrames)
	}
}

func TestAgentContinuesSameToolCallAfterRangeConfirmation(t *testing.T) {
	orchestrator := &fakeOrchestrator{turns: []OrchestratorTurn{
		toolTurn("call-1", 18_000, 28_000, 5_000),
		finalTurn("GROUNDED", "人物继续向前行走。"),
	}}
	observer := &fakeObserver{values: []Observation{{Observation: "人物继续向前行走"}}}
	agent := newTestAgent(orchestrator, observer)
	extractCalls := 0
	agent.ffmpeg = agentFakeFrameExtractor{calls: &extractCalls}
	request := baseRequest()
	request.Range = Range{StartMS: 13_000, EndMS: 18_000}
	request.OnRangeConfirmation = func(value RangeConfirmationRequest) (RangeConfirmationHandle, error) {
		if extractCalls != 0 {
			t.Fatalf("frames were extracted before range confirmation: %d", extractCalls)
		}
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
	if extractCalls == 0 {
		t.Fatal("confirmed tool call did not extract frames")
	}
	if len(orchestrator.histories[1]) != 2 || orchestrator.histories[1][1].ToolCallID != "call-1" {
		t.Fatalf("tool call was not resumed: %#v", orchestrator.histories[1])
	}
}

func TestAgentRejectsGroundedWithoutToolResult(t *testing.T) {
	orchestrator := &fakeOrchestrator{turns: []OrchestratorTurn{
		finalTurn("GROUNDED", "伪造回答"),
		finalTurn("GROUNDED", "伪造回答"),
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

func TestAgentAllowsNoEvidenceWithoutInitialInspection(t *testing.T) {
	orchestrator := &fakeOrchestrator{turns: []OrchestratorTurn{
		finalTurn("NO_EVIDENCE", "现有信息不足以回答。"),
	}}
	observer := &fakeObserver{}
	result, err := newTestAgent(orchestrator, observer).Run(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusNoEvidence || result.ToolCalls != 0 || observer.index != 0 {
		t.Fatalf("zero-inspection final result = %#v, observer calls = %d", result, observer.index)
	}
	if orchestrator.index != 1 {
		t.Fatalf("orchestrator calls = %d, want 1", orchestrator.index)
	}
}

func TestAgentStopsAtThreeCallBudget(t *testing.T) {
	orchestrator := &fakeOrchestrator{turns: []OrchestratorTurn{
		toolTurn("call-1", 0, 2_000, 500),
		toolTurn("call-2", 2_000, 4_000, 500),
		toolTurn("call-3", 4_000, 6_000, 500),
		toolTurn("call-4", 6_000, 8_000, 500),
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
	if !slices.Equal(result.UsedObservationIDs, []string{"obs-1", "obs-2", "obs-3"}) || len(result.UsedFrameTimestampsMS) != 12 {
		t.Fatalf("budget fallback lost server-owned observations: %#v", result)
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
