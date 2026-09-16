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

type fakeDataRepository struct {
	memories []Memory
}

type invalidModelClient struct {
	calls int
}

func (f *invalidModelClient) Decide(context.Context, decisionInput, []Frame, map[int64]Frame) (Decision, error) {
	f.calls++
	return Decision{}, ErrInvalidOutput
}

func (*invalidModelClient) Answer(context.Context, answerInput, map[int64]Frame) (FinalAnswer, error) {
	return FinalAnswer{}, ErrInvalidOutput
}

func (*invalidModelClient) Model() string { return "invalid-vllm" }

func (f fakeDataRepository) loadVideo(context.Context, string, string) (model.Video, error) {
	return model.Video{ID: "video-1", Status: model.VideoStatusReady, ObjectKey: "video.mp4", DurationMS: 60_000}, nil
}

func (f fakeDataRepository) loadWindows(context.Context, string, string, Range) ([]RetrievalWindow, error) {
	return []RetrievalWindow{{SegmentID: "segment-1", StartMS: 0, EndMS: 10_000, RetrievalText: "原始检索文本"}}, nil
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

type fakeModelClient struct {
	decisions []Decision
	answer    FinalAnswer
	decideAt  int
	answered  bool
	input     answerInput
}

func (f *fakeModelClient) Decide(context.Context, decisionInput, []Frame, map[int64]Frame) (Decision, error) {
	decision := f.decisions[f.decideAt]
	f.decideAt++
	return decision, nil
}

func (f *fakeModelClient) Answer(_ context.Context, input answerInput, _ map[int64]Frame) (FinalAnswer, error) {
	f.answered = true
	f.input = input
	return f.answer, nil
}

func (*fakeModelClient) Model() string { return "fake-vllm" }

func newTestAgent(repository dataRepository, client modelClient) *Agent {
	return &Agent{
		repository: repository,
		source:     agentFakeVideoSource{}, ffmpeg: agentFakeFrameExtractor{}, client: client,
		totalTimeout: time.Second, limiter: NewLocalLimiter(1, 100, time.Minute),
	}
}

func TestAgentGroundedAnswerCombinesRetrievalAndVisualMemory(t *testing.T) {
	client := &fakeModelClient{
		decisions: []Decision{{
			Action: ActionFinal, EvidenceStatus: "GROUNDED", Observation: "人物举起了杯子",
			UsedFrameTimestampsMS: []int64{5_000},
		}},
		answer: FinalAnswer{Answer: "00:05 时人物举起了杯子。", UsedFrameTimestampsMS: []int64{5_000}},
	}
	agent := newTestAgent(fakeDataRepository{memories: []Memory{{
		StartMS: 0, EndMS: 10_000, Status: StatusGrounded, Observation: "人物此前坐在桌旁",
	}}}, client)

	var streamedTraces []AgentTrace
	result, err := agent.Run(context.Background(), Request{
		TenantID: "tenant-1", VideoID: "video-1", ConversationID: "conversation-1",
		Question: "这个人在做什么？", RewrittenQuery: "人物动作", Range: Range{StartMS: 0, EndMS: 10_000},
		OnTrace: func(trace AgentTrace) error {
			streamedTraces = append(streamedTraces, trace)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != StatusGrounded || result.Answer != client.answer.Answer || !client.answered {
		t.Fatalf("unexpected grounded result: %#v", result)
	}
	for _, want := range []string{"原始检索文本", "人物此前坐在桌旁", "人物举起了杯子"} {
		if !strings.Contains(client.input.CombinedRetrievalText, want) {
			t.Fatalf("combined retrieval text missing %q: %s", want, client.input.CombinedRetrievalText)
		}
	}
	if len(streamedTraces) != 3 || len(result.Trace) != 2 {
		t.Fatalf("streamed/persisted traces = %d/%d, want 3/2", len(streamedTraces), len(result.Trace))
	}
	if result.Trace[0].Action != TraceInitial || result.Trace[1].EvidenceStatus != "GROUNDED" {
		t.Fatalf("unexpected persisted trace: %#v", result.Trace)
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

func TestAgentStopsAfterToolBudgetWithNoEvidence(t *testing.T) {
	call := func(start, end int64) Decision {
		return Decision{Action: ActionCallTool, Tool: &ToolCall{Name: ToolName, Arguments: InspectVideoRangeInput{
			StartMS: start, EndMS: end, IntervalMS: MinIntervalMS,
		}}}
	}
	client := &fakeModelClient{decisions: []Decision{call(1_000, 2_000), call(2_000, 3_000), call(3_000, 4_000)}}
	agent := newTestAgent(fakeDataRepository{}, client)

	result, err := agent.Run(context.Background(), Request{
		TenantID: "tenant-1", VideoID: "video-1", ConversationID: "conversation-1",
		Question: "人物手里有什么？", Range: Range{StartMS: 0, EndMS: 10_000},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != StatusNoEvidence || result.ToolCalls != MaxToolCalls || client.answered {
		t.Fatalf("agent did not stop deterministically at budget: result=%#v answered=%v", result, client.answered)
	}
	if client.decideAt != MaxToolCalls+1 {
		t.Fatalf("Decide() calls = %d, want %d", client.decideAt, MaxToolCalls+1)
	}
}

func TestAgentStopsAfterTwoInvalidModelOutputs(t *testing.T) {
	client := &invalidModelClient{}
	agent := newTestAgent(fakeDataRepository{}, client)
	_, err := agent.Run(context.Background(), Request{
		TenantID: "tenant-1", VideoID: "video-1", ConversationID: "conversation-1",
		Question: "人物手里有什么？", Range: Range{StartMS: 0, EndMS: 10_000},
	})
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("Run() error = %v, want ErrInvalidOutput", err)
	}
	if client.calls != 2 {
		t.Fatalf("invalid model calls = %d, want 2", client.calls)
	}
}
