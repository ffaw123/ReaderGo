package visionagent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"readergo/internal/media"

	"gorm.io/gorm"
)

type Options struct {
	BaseURL        string
	APIKey         string
	Model          string
	RequestTimeout time.Duration
	TotalTimeout   time.Duration
	Limiter        Limiter
}

type Agent struct {
	repository   dataRepository
	source       VideoSource
	ffmpeg       frameExtractor
	client       modelClient
	totalTimeout time.Duration
	limiter      Limiter
}

func New(db *gorm.DB, source VideoSource, ffmpeg *media.FFmpeg, options Options) (*Agent, error) {
	if db == nil || source == nil || ffmpeg == nil {
		return nil, errors.New("vision agent requires database, video source, and ffmpeg")
	}
	client, err := newVLLMClient(options.BaseURL, options.APIKey, options.Model, options.RequestTimeout)
	if err != nil {
		return nil, err
	}
	if options.TotalTimeout <= 0 {
		return nil, errors.New("vision total timeout must be positive")
	}
	limiter := options.Limiter
	if limiter == nil {
		// 默认每租户同时只运行一个请求，每分钟最多启动六次。
		limiter = NewLocalLimiter(1, 6, time.Minute)
	}
	return &Agent{repository: &repository{db: db}, source: source, ffmpeg: ffmpeg, client: client, totalTimeout: options.TotalTimeout, limiter: limiter}, nil
}

func (a *Agent) Available() bool               { return a != nil }
func (a *Agent) MaxRunDuration() time.Duration { return a.totalTimeout }

func (a *Agent) Run(parent context.Context, request Request) (Result, error) {
	started := time.Now()
	latency := make(map[string]time.Duration)
	toolHistory := make([]ToolTrace, 0, MaxToolCalls)
	traces := make([]AgentTrace, 0, MaxToolCalls+2)
	ctx, cancel := context.WithTimeout(parent, a.totalTimeout)
	defer cancel()
	releaseLimit, err := a.limiter.Acquire(ctx, request.TenantID)
	if err != nil {
		return Result{}, err
	}
	defer releaseLimit()

	contextStarted := time.Now()
	video, err := a.repository.loadVideo(ctx, request.TenantID, request.VideoID)
	if err != nil {
		return Result{}, err
	}
	if err := ValidateRange(request.Range, video.DurationMS); err != nil {
		return Result{}, err
	}
	windows, err := a.repository.loadWindows(ctx, request.TenantID, request.VideoID, request.Range)
	if err != nil {
		return Result{}, err
	}
	memories, err := a.repository.loadMemories(ctx, request)
	if err != nil {
		return Result{}, err
	}
	latency["context_load"] = time.Since(contextStarted)
	if err := reportProgress(request.OnProgress, "loading_source", "正在读取视频片段"); err != nil {
		return Result{}, err
	}
	sourceStarted := time.Now()
	videoPath, release, err := a.source.LocalPath(ctx, video)
	if err != nil {
		return Result{}, err
	}
	latency["source_load"] = time.Since(sourceStarted)
	session, err := newSession(ctx, request.Range, videoPath, release, a.ffmpeg)
	if err != nil {
		return Result{}, err
	}
	defer session.Close()

	if err := reportProgress(request.OnProgress, "initial_sampling", "正在查看所选时间段的画面"); err != nil {
		return Result{}, err
	}
	initialArguments := &InspectVideoRangeInput{StartMS: request.Range.StartMS, EndMS: request.Range.EndMS, IntervalMS: InitialIntervalMS}
	initialTrace := AgentTrace{
		ID: "initial", Step: 1, Status: "running", Action: TraceInitial,
		Summary: "正在按默认间隔查看所选时间区间", Arguments: initialArguments,
		RemainingToolCalls: MaxToolCalls, RemainingFrameBudget: MaxUniqueFrames,
	}
	if err := reportTrace(request.OnTrace, initialTrace); err != nil {
		return Result{}, err
	}
	initialSamplingStarted := time.Now()
	if err := session.InitialSample(); err != nil {
		return Result{}, fmt.Errorf("initial vision sampling: %w", err)
	}
	latency["initial_sampling"] = time.Since(initialSamplingStarted)
	initialTrace.Status = "completed"
	initialTrace.Summary = "已完成所选时间区间的初始画面检查"
	initialTrace.NewFrameTimestampsMS = timestampsOf(session.Frames())
	initialTrace.RemainingToolCalls, initialTrace.RemainingFrameBudget = session.RemainingToolCalls(), session.RemainingFrames()
	if err := reportTrace(request.OnTrace, initialTrace); err != nil {
		return Result{}, err
	}
	traces = append(traces, initialTrace)

	for {
		frames := session.Frames()
		input := decisionInput{
			Question: request.Question, RewrittenQuery: request.RewrittenQuery, AllowedRange: request.Range,
			History: request.History, PriorVisualMemory: memories, RetrievalWindows: windows,
			SampledTimestampsMS: timestampsOf(frames), RemainingToolCalls: session.RemainingToolCalls(),
			RemainingFrameBudget: session.RemainingFrames(), RemainingTimeMS: max(0, (a.totalTimeout - time.Since(started)).Milliseconds()),
			ToolHistory: append([]ToolTrace(nil), toolHistory...), ToolErrors: append([]string(nil), session.toolErrors...),
		}
		decisionStarted := time.Now()
		decision, err := a.decideWithValidationRetry(ctx, input, frames, session.frames)
		latency["vision_decision"] += time.Since(decisionStarted)
		if err != nil {
			return Result{}, err
		}
		if decision.Action == ActionCallTool {
			if session.RemainingToolCalls() == 0 {
				return a.finishNoEvidence(request, session, windows, started, latency, traces)
			}
			if err := reportProgress(request.OnProgress, "refining", "正在细化检查关键画面"); err != nil {
				return Result{}, err
			}
			trace := AgentTrace{
				ID: fmt.Sprintf("tool-%d", session.toolCalls+1), Step: len(traces) + 1,
				Status: "running", Action: TraceInspect, Summary: "正在细化检查关键画面",
				Reason: safeTraceReason(decision.Reason), Arguments: &decision.Tool.Arguments,
				RemainingToolCalls: session.RemainingToolCalls(), RemainingFrameBudget: session.RemainingFrames(),
			}
			if err := reportTrace(request.OnTrace, trace); err != nil {
				return Result{}, err
			}
			toolStarted := time.Now()
			before := session.frames
			beforeTimestamps := make(map[int64]struct{}, len(before))
			for timestamp := range before {
				beforeTimestamps[timestamp] = struct{}{}
			}
			err := session.ExecuteTool(decision.Tool.Arguments)
			latency["tool_sampling"] += time.Since(toolStarted)
			toolTrace := ToolTrace{Reason: decision.Reason, Arguments: decision.Tool.Arguments, OK: err == nil}
			for timestamp := range session.frames {
				if _, exists := beforeTimestamps[timestamp]; !exists {
					toolTrace.NewFrameTimestampsMS = append(toolTrace.NewFrameTimestampsMS, timestamp)
				}
			}
			slices.Sort(toolTrace.NewFrameTimestampsMS)
			toolTrace.RemainingToolCalls, toolTrace.RemainingFrameBudget = session.RemainingToolCalls(), session.RemainingFrames()
			trace.NewFrameTimestampsMS = append([]int64(nil), toolTrace.NewFrameTimestampsMS...)
			trace.RemainingToolCalls, trace.RemainingFrameBudget = toolTrace.RemainingToolCalls, toolTrace.RemainingFrameBudget
			if err != nil {
				code, message, correctable := safeToolError(err)
				if !correctable {
					trace.Status, trace.Summary = "failed", "画面提取失败，本次检查已停止"
					if traceErr := reportTrace(request.OnTrace, trace); traceErr != nil {
						return Result{}, traceErr
					}
					return Result{}, fmt.Errorf("vision tool sampling failed: %w", err)
				}
				toolTrace.ErrorCode, toolTrace.Message = code, message
				toolHistory = append(toolHistory, toolTrace)
				trace.Status, trace.Summary = "failed", message
				if traceErr := reportTrace(request.OnTrace, trace); traceErr != nil {
					return Result{}, traceErr
				}
				traces = append(traces, trace)
				session.AppendToolError(errors.New(code + ": " + message))
				if session.RemainingToolCalls() == 0 || errors.Is(err, ErrToolBudget) {
					return a.finishNoEvidence(request, session, windows, started, latency, traces)
				}
			} else {
				toolHistory = append(toolHistory, toolTrace)
				trace.Status = "completed"
				trace.Summary = fmt.Sprintf("已完成细化检查，新增 %d 帧", len(trace.NewFrameTimestampsMS))
				if err := reportTrace(request.OnTrace, trace); err != nil {
					return Result{}, err
				}
				traces = append(traces, trace)
			}
			continue
		}
		if decision.EvidenceStatus == "NO_EVIDENCE" {
			return a.finishNoEvidence(request, session, windows, started, latency, traces)
		}

		if err := reportProgress(request.OnProgress, "synthesizing", "正在结合视频检索内容生成回答"); err != nil {
			return Result{}, err
		}
		combined := buildCombinedRetrievalText(windows, memories, decision)
		answerEvidence := make(map[int64]Frame, len(decision.UsedFrameTimestampsMS))
		for _, timestamp := range decision.UsedFrameTimestampsMS {
			answerEvidence[timestamp] = session.frames[timestamp]
		}
		answerStarted := time.Now()
		answer, err := a.answerWithValidationRetry(ctx, answerInput{
			Question: request.Question, RewrittenQuery: request.RewrittenQuery, AllowedRange: request.Range,
			CombinedRetrievalText: combined, CurrentObservation: decision.Observation,
			UsedFrameTimestampsMS: decision.UsedFrameTimestampsMS,
		}, answerEvidence)
		if err != nil {
			return Result{}, err
		}
		latency["answer_synthesis"] = time.Since(answerStarted)
		used := answer.UsedFrameTimestampsMS
		latency["vision_total"] = time.Since(started)
		finalTrace := AgentTrace{
			ID: "final", Step: len(traces) + 1, Status: "completed", Action: TraceFinal,
			Summary: "已找到能够回答问题的画面证据", EvidenceStatus: "GROUNDED",
			UsedFrameTimestampsMS: append([]int64(nil), used...),
		}
		if err := reportTrace(request.OnTrace, finalTrace); err != nil {
			return Result{}, err
		}
		traces = append(traces, finalTrace)
		return Result{
			Answer: answer.Answer, Status: StatusGrounded, Observation: decision.Observation,
			SampledFrameTimestampsMS: timestampsOf(session.Frames()), UsedFrameTimestampsMS: used,
			ToolCalls: session.toolCalls, Model: a.client.Model(), PromptVersion: AgentPromptVersion,
			AnswerPromptVersion: AnswerPromptVersion, SegmentIDsByTimestamp: segmentIDsForTimestamps(windows, used),
			Latency: latency, Trace: traces,
		}, nil
	}
}

func (a *Agent) finishNoEvidence(request Request, session *session, windows []RetrievalWindow, started time.Time, latency map[string]time.Duration, traces []AgentTrace) (Result, error) {
	finalTrace := AgentTrace{
		ID: "final", Step: len(traces) + 1, Status: "completed", Action: TraceFinal,
		Summary: "在允许的检查预算内未找到足以回答问题的画面证据", EvidenceStatus: "NO_EVIDENCE",
	}
	if err := reportTrace(request.OnTrace, finalTrace); err != nil {
		return Result{}, err
	}
	traces = append(traces, finalTrace)
	return a.noEvidenceResult(session, windows, started, latency, traces), nil
}

func safeTraceReason(reason string) string {
	value := strings.Join(strings.Fields(reason), " ")
	lower := strings.ToLower(value)
	if value == "" || strings.Contains(lower, "://") || strings.Contains(lower, `:\`) || strings.Contains(lower, ":/") || strings.Contains(lower, "/tmp/") || strings.Contains(lower, "/home/") || strings.Contains(lower, "api_key") {
		return "当前画面不足以确认细节，需要在授权区间内继续检查"
	}
	runes := []rune(value)
	if len(runes) > 200 {
		value = string(runes[:200])
	}
	return value
}

func safeToolError(err error) (code, message string, correctable bool) {
	switch {
	case errors.Is(err, ErrRangeOutOfBounds):
		return "RANGE_OUT_OF_BOUNDS", "请求区间超出用户授权范围", true
	case errors.Is(err, ErrFrameBudget):
		return "FRAME_BUDGET_EXCEEDED", "请求会超过剩余唯一帧预算", true
	case errors.Is(err, ErrRangeInvalid):
		return "INVALID_ARGUMENTS", "起止时间或采样间隔无效", true
	case errors.Is(err, ErrToolBudget):
		return "TOOL_BUDGET_EXCEEDED", "追加工具调用次数已用尽", true
	default:
		return "", "", false
	}
}

func (a *Agent) decideWithValidationRetry(ctx context.Context, input decisionInput, frames []Frame, sampled map[int64]Frame) (Decision, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		decision, err := a.client.Decide(ctx, input, frames, sampled)
		if err == nil {
			return decision, nil
		}
		lastErr = err
	}
	return Decision{}, fmt.Errorf("vision decision remains invalid: %w", lastErr)
}

func (a *Agent) answerWithValidationRetry(ctx context.Context, input answerInput, sampled map[int64]Frame) (FinalAnswer, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		answer, err := a.client.Answer(ctx, input, sampled)
		if err == nil {
			return answer, nil
		}
		lastErr = err
	}
	return FinalAnswer{}, fmt.Errorf("vision answer remains invalid: %w", lastErr)
}

func (a *Agent) noEvidenceResult(session *session, windows []RetrievalWindow, started time.Time, latency map[string]time.Duration, traces []AgentTrace) Result {
	timestamps := timestampsOf(session.Frames())
	labels := make([]string, len(timestamps))
	for index, timestamp := range timestamps {
		labels[index] = formatTimestamp(timestamp)
	}
	latency["vision_total"] = time.Since(started)
	return Result{
		Answer: fmt.Sprintf("我查看了所选区间内的画面（%s），并在允许的细化次数内继续检查，但仍未观察到足以回答该问题的可靠证据。请提供更准确的时间区间后重试。", strings.Join(labels, "、")),
		Status: StatusNoEvidence, SampledFrameTimestampsMS: timestamps, ToolCalls: session.toolCalls,
		Model: a.client.Model(), PromptVersion: AgentPromptVersion, AnswerPromptVersion: AnswerPromptVersion,
		SegmentIDsByTimestamp: segmentIDsForTimestamps(windows, timestamps),
		Latency:               latency,
		Trace:                 traces,
	}
}

func reportProgress(callback ProgressFunc, stage, message string) error {
	if callback == nil {
		return nil
	}
	return callback(Progress{Stage: stage, Message: message})
}

func reportTrace(callback TraceFunc, trace AgentTrace) error {
	if callback == nil {
		return nil
	}
	return callback(trace)
}

func timestampsOf(frames []Frame) []int64 {
	result := make([]int64, len(frames))
	for index, frame := range frames {
		result[index] = frame.TimestampMS
	}
	slices.Sort(result)
	return result
}

func segmentIDsForTimestamps(windows []RetrievalWindow, timestamps []int64) map[int64]string {
	result := make(map[int64]string, len(timestamps))
	for _, timestamp := range timestamps {
		for _, window := range windows {
			if timestamp >= window.StartMS && timestamp < window.EndMS {
				result[timestamp] = window.SegmentID
				break
			}
		}
	}
	return result
}
