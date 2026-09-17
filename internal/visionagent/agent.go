package visionagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"readergo/internal/media"

	"gorm.io/gorm"
)

type Options struct {
	OrchestratorBaseURL        string
	OrchestratorAPIKey         string
	OrchestratorModel          string
	OrchestratorRequestTimeout time.Duration
	VisionBaseURL              string
	VisionAPIKey               string
	VisionModel                string
	VisionRequestTimeout       time.Duration
	TotalTimeout               time.Duration
	Limiter                    Limiter
}

type Agent struct {
	repository   dataRepository
	source       VideoSource
	ffmpeg       frameExtractor
	orchestrator orchestratorClient
	observer     visionObserverClient
	totalTimeout time.Duration
	limiter      Limiter
}

func New(db *gorm.DB, source VideoSource, ffmpeg *media.FFmpeg, options Options) (*Agent, error) {
	if db == nil || source == nil || ffmpeg == nil {
		return nil, errors.New("vision agent requires database, video source, and ffmpeg")
	}
	orchestrator, err := newOrchestratorClient(
		options.OrchestratorBaseURL, options.OrchestratorAPIKey, options.OrchestratorModel, options.OrchestratorRequestTimeout,
	)
	if err != nil {
		return nil, err
	}
	observer, err := newVisionObserverClient(
		options.VisionBaseURL, options.VisionAPIKey, options.VisionModel, options.VisionRequestTimeout,
	)
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
	return &Agent{
		repository: &repository{db: db}, source: source, ffmpeg: ffmpeg,
		orchestrator: orchestrator, observer: observer,
		totalTimeout: options.TotalTimeout, limiter: limiter,
	}, nil
}

func (a *Agent) Available() bool               { return a != nil && a.orchestrator != nil && a.observer != nil }
func (a *Agent) MaxRunDuration() time.Duration { return a.totalTimeout }

func (a *Agent) Run(parent context.Context, request Request) (Result, error) {
	started := time.Now()
	latency := make(map[string]time.Duration)
	traces := make([]AgentTrace, 0, MaxToolCalls*2+1)
	messages := make([]LoopMessage, 0, MaxToolCalls*2)
	observations := make(map[string]ToolResult, MaxToolCalls)
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
	// 中文注释：画面 Agent 固定只接收最后三轮完整问答，不依赖上游历史配置或旧视觉记忆。
	recentDialogue := lastDialogueTurns(request.History)
	latency["context_load"] = time.Since(contextStarted)

	if err := reportProgress(request.OnProgress, "agent_deciding", "正在判断需要检查哪些画面"); err != nil {
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

	input := OrchestratorInput{
		Question: request.Question, RewrittenQuery: request.RewrittenQuery, AllowedRange: request.Range,
		RetrievalWindows: windows,
	}

	for {
		// 视觉预算耗尽后进入专用最终生成，不再向 MiMo 暴露工具。
		input.ForceFinal = len(messages) > 0 && session.RemainingToolCalls() == 0
		if input.ForceFinal {
			input.ProtocolInstruction = "视觉工具预算已用尽。现在必须返回严格 FINAL JSON，并根据已有视觉观察自行判断 GROUNDED 或 NO_EVIDENCE。不要输出 observation_id 或帧时间戳。"
		}
		deciding := AgentTrace{
			ID: fmt.Sprintf("decide-%d", len(messages)/2+1), Step: len(traces) + 1, Status: "running",
			Action: TraceAgentDeciding, Summary: "主 Agent 正在判断下一步",
			RemainingToolCalls: session.RemainingToolCalls(), RemainingFrameBudget: session.RemainingFrames(),
		}
		if err := reportTrace(request.OnTrace, deciding); err != nil {
			return Result{}, err
		}
		decisionStarted := time.Now()
		turn, err := a.nextWithValidationRetry(ctx, input, recentDialogue, messages, observations)
		latency["agent_decision"] += time.Since(decisionStarted)
		if err != nil {
			return Result{}, err
		}
		deciding.Status = "completed"
		if turn.Tool != nil {
			deciding.Summary = "主 Agent 决定检查画面"
		} else {
			deciding.Summary = "主 Agent 已形成最终回答"
		}
		if err := reportTrace(request.OnTrace, deciding); err != nil {
			return Result{}, err
		}
		traces = append(traces, deciding)

		if turn.Final != nil {
			return a.finish(request, session, windows, started, latency, traces, observations, *turn.Final)
		}
		if turn.Tool == nil {
			return Result{}, ErrInvalidOutput
		}
		// 中文注释：先保存主模型原始 tool_call，随后必须用同一个 ID 回传 role=tool 结果。
		messages = append(messages, LoopMessage{Role: "assistant", Content: turn.Content, Tool: turn.Tool})
		if session.RemainingToolCalls() == 0 {
			return a.finishNoEvidence(request, session, windows, started, latency, traces, observations)
		}

		arguments := turn.Tool.Arguments
		trace := AgentTrace{
			ID: fmt.Sprintf("tool-%d", session.toolCalls+1), Step: len(traces) + 1, Status: "running",
			Action: TraceInspect, Summary: "正在提取主 Agent 指定区间的画面",
			Reason: safeTraceReason(turn.Content), Arguments: &arguments,
			RemainingToolCalls: session.RemainingToolCalls(), RemainingFrameBudget: session.RemainingFrames(),
		}
		if err := reportProgress(request.OnProgress, "inspect_video_range", "正在提取指定时间区间的画面"); err != nil {
			return Result{}, err
		}
		if err := reportTrace(request.OnTrace, trace); err != nil {
			return Result{}, err
		}

		samplingStarted := time.Now()
		frames, toolErr := session.ExecuteTool(arguments)
		latency["tool_sampling"] += time.Since(samplingStarted)
		if errors.Is(toolErr, ErrRangeOutOfBounds) {
			var confirmed Range
			var confirmationTrace AgentTrace
			arguments, confirmed, confirmationTrace, toolErr = a.confirmRange(ctx, request, trace, arguments, video.DurationMS)
			if toolErr == nil {
				traces = append(traces, confirmationTrace)
				session.ConfirmRange(confirmed)
				request.Range, input.AllowedRange = confirmed, confirmed
				windows, toolErr = a.repository.loadWindows(ctx, request.TenantID, request.VideoID, confirmed)
				if toolErr == nil {
					input.RetrievalWindows = windows
				}
				if toolErr == nil {
					samplingStarted = time.Now()
					frames, toolErr = session.ExecuteConfirmedTool(arguments)
					latency["tool_sampling"] += time.Since(samplingStarted)
				}
			}
		}
		if toolErr != nil {
			code, message, correctable := safeToolError(toolErr)
			if !correctable {
				trace.Status, trace.Summary = "failed", "画面提取失败，本次检查已停止"
				_ = reportTrace(request.OnTrace, trace)
				return Result{}, fmt.Errorf("vision tool failed: %w", toolErr)
			}
			result := ToolResult{
				OK: false, ErrorCode: code, Message: message,
				RemainingVisionCalls: session.RemainingToolCalls(), RemainingFrameBudget: session.RemainingFrames(),
			}
			messages = append(messages, toolResultMessage(turn.Tool.ID, result))
			trace.Status, trace.Summary = "failed", message
			trace.RemainingToolCalls, trace.RemainingFrameBudget = result.RemainingVisionCalls, result.RemainingFrameBudget
			if err := reportTrace(request.OnTrace, trace); err != nil {
				return Result{}, err
			}
			traces = append(traces, trace)
			if session.RemainingToolCalls() == 0 {
				return a.finishNoEvidence(request, session, windows, started, latency, traces, observations)
			}
			continue
		}
		// 采样器可能按区间长度收紧模型给出的间隔；后续 fps、工具结果和审计记录必须使用实际采样间隔。
		arguments.IntervalMS = frames[1].TimestampMS - frames[0].TimestampMS

		trace.Status, trace.Summary = "completed", fmt.Sprintf("已提取 %d 帧画面", len(frames))
		trace.NewFrameTimestampsMS = timestampsOf(frames)
		trace.RemainingToolCalls, trace.RemainingFrameBudget = session.RemainingToolCalls(), session.RemainingFrames()
		if err := reportTrace(request.OnTrace, trace); err != nil {
			return Result{}, err
		}
		traces = append(traces, trace)

		observing := AgentTrace{
			ID: fmt.Sprintf("observe-%d", session.toolCalls), Step: len(traces) + 1, Status: "running",
			Action: TraceObserving, Summary: "视觉观察器正在识别已提取画面",
			NewFrameTimestampsMS: timestampsOf(frames),
			RemainingToolCalls:   session.RemainingToolCalls(), RemainingFrameBudget: session.RemainingFrames(),
		}
		if err := reportProgress(request.OnProgress, "vision_observing", "正在识别已提取的画面"); err != nil {
			return Result{}, err
		}
		if err := reportTrace(request.OnTrace, observing); err != nil {
			return Result{}, err
		}
		observerStarted := time.Now()
		observation, observeErr := a.observeWithValidationRetry(ctx, ObserverInput{Question: request.RewrittenQuery, IntervalMS: arguments.IntervalMS}, frames)
		latency["vision_observation"] += time.Since(observerStarted)
		if observeErr != nil {
			errorCode, errorMessage := safeObserverError(observeErr)
			result := ToolResult{
				OK: false, ErrorCode: errorCode, Message: errorMessage,
				RemainingVisionCalls: session.RemainingToolCalls(), RemainingFrameBudget: session.RemainingFrames(),
			}
			messages = append(messages, toolResultMessage(turn.Tool.ID, result))
			observing.Status, observing.Summary = "failed", result.Message
			if err := reportTrace(request.OnTrace, observing); err != nil {
				return Result{}, err
			}
			traces = append(traces, observing)
			if session.RemainingToolCalls() == 0 {
				return a.finishNoEvidence(request, session, windows, started, latency, traces, observations)
			}
			continue
		}

		// 观察 ID 由后端生成并保存，不放进主 Agent 可见的工具结果。
		observationID := fmt.Sprintf("obs-%d", len(observations)+1)
		result := ToolResult{
			OK:    true,
			Range: &Range{StartMS: arguments.StartMS, EndMS: arguments.EndMS}, IntervalMS: arguments.IntervalMS,
			SampledFrameTimestampsMS: timestampsOf(frames), Observation: observation.Observation,
			RemainingVisionCalls: session.RemainingToolCalls(), RemainingFrameBudget: session.RemainingFrames(),
		}
		observations[observationID] = result
		messages = append(messages, toolResultMessage(turn.Tool.ID, result))
		observing.Status, observing.Action, observing.Summary = "completed", TraceVisionResult, "视觉工具已返回画面观察"
		observing.Observation = result.Observation
		if err := reportTrace(request.OnTrace, observing); err != nil {
			return Result{}, err
		}
		traces = append(traces, observing)
	}
}

func (a *Agent) nextWithValidationRetry(ctx context.Context, input OrchestratorInput, recentDialogue []HistoryTurn, messages []LoopMessage, observations map[string]ToolResult) (OrchestratorTurn, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		turn, err := a.orchestrator.Next(ctx, input, recentDialogue, messages)
		if err == nil && turn.Final != nil {
			var decision FinalDecision
			decision, err = decodeFinalDecision(turn.Content)
			// 中文注释：证据状态仍由主 Agent 判断，但 GROUNDED 至少必须存在一条后端记录的成功视觉观察。
			if err == nil && decision.EvidenceStatus == "GROUNDED" && !hasSuccessfulObservation(observations) {
				err = fmt.Errorf("grounded final requires a successful visual observation: %w", ErrInvalidOutput)
			}
		}
		if err == nil {
			return turn, nil
		}
		lastErr = err
		if !errors.Is(err, ErrInvalidOutput) {
			return OrchestratorTurn{}, err
		}
		code, _ := safeOrchestratorError(err)
		log.Printf("vision orchestrator turn rejected: code=%s error=%v", code, err)
		if len(messages) > 0 {
			input.ForceFinal = true
			input.ProtocolInstruction = "上一响应不符合最终输出协议。不要再调用工具；只返回包含 action、evidence_status 和 answer 的严格 FINAL JSON。"
		} else {
			// 中文注释：首轮不强制调用工具，但无效响应必须显式纠正，避免零温度下重复同一错误输出。
			input.ProtocolInstruction = "上一响应不符合协议。需要画面证据时调用 inspect_video_range；否则只返回严格 FINAL JSON。没有成功视觉观察时不得返回 GROUNDED。"
		}
	}
	return OrchestratorTurn{}, fmt.Errorf("vision orchestrator output remains invalid: %w", lastErr)
}

func (a *Agent) observeWithValidationRetry(ctx context.Context, input ObserverInput, frames []Frame) (Observation, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		observation, err := a.observer.Observe(ctx, input, frames)
		if err == nil {
			return observation, nil
		}
		lastErr = err
		if !errors.Is(err, ErrInvalidOutput) {
			return Observation{}, err
		}
	}
	return Observation{}, lastErr
}

func (a *Agent) confirmRange(ctx context.Context, request Request, trace AgentTrace, input InspectVideoRangeInput, durationMS int64) (InspectVideoRangeInput, Range, AgentTrace, error) {
	suggested, ok := buildSuggestedRange(input, durationMS)
	if !ok || request.OnRangeConfirmation == nil {
		return input, Range{}, AgentTrace{}, ErrRangeConfirmationUnavailable
	}
	handle, err := request.OnRangeConfirmation(RangeConfirmationRequest{CurrentRange: request.Range, SuggestedRange: suggested})
	if err != nil {
		return input, Range{}, AgentTrace{}, err
	}
	if handle.ID == "" || handle.Wait == nil || handle.Cancel == nil {
		return input, Range{}, AgentTrace{}, ErrRangeConfirmationUnavailable
	}
	defer handle.Cancel()
	trace.ID = "confirmation-" + handle.ID
	trace.Action, trace.Summary, trace.ConfirmationID = TraceRangeRequest, "需要用户确认新的画面区间", handle.ID
	trace.Arguments = &InspectVideoRangeInput{StartMS: suggested.StartMS, EndMS: suggested.EndMS, IntervalMS: input.IntervalMS}
	if err := reportTrace(request.OnTrace, trace); err != nil {
		return input, Range{}, AgentTrace{}, err
	}
	confirmed, err := handle.Wait(ctx)
	if err != nil {
		return input, Range{}, AgentTrace{}, err
	}
	if confirmed != suggested || ValidateRange(confirmed, durationMS) != nil {
		return input, Range{}, AgentTrace{}, ErrRangeInvalid
	}
	trace.Status, trace.Summary = "completed", "用户已确认新的画面区间"
	if err := reportTrace(request.OnTrace, trace); err != nil {
		return input, Range{}, AgentTrace{}, err
	}
	input.StartMS, input.EndMS = confirmed.StartMS, confirmed.EndMS
	return input, confirmed, trace, nil
}

func (a *Agent) finish(request Request, session *session, windows []RetrievalWindow, started time.Time, latency map[string]time.Duration, traces []AgentTrace, observations map[string]ToolResult, decision FinalDecision) (Result, error) {
	decision, err := decodeFinalDecision(mustJSON(decision))
	if err != nil {
		return Result{}, err
	}
	usedObservationIDs, observationTexts, usedFrames := collectObservationEvidence(observations)
	status := StatusNoEvidence
	if decision.EvidenceStatus == "GROUNDED" {
		status = StatusGrounded
	}
	latency["vision_total"] = time.Since(started)
	finalTrace := AgentTrace{
		ID: "final", Step: len(traces) + 1, Status: "completed", Action: TraceFinal,
		Summary: "主 Agent 已完成回答", EvidenceStatus: decision.EvidenceStatus,
		UsedFrameTimestampsMS: append([]int64(nil), usedFrames...),
		RemainingToolCalls:    session.RemainingToolCalls(), RemainingFrameBudget: session.RemainingFrames(),
	}
	if err := reportTrace(request.OnTrace, finalTrace); err != nil {
		return Result{}, err
	}
	traces = append(traces, finalTrace)
	return Result{
		Answer: decision.Answer, Status: status, Observation: strings.Join(observationTexts, "\n"),
		SampledFrameTimestampsMS: timestampsOf(session.Frames()), UsedFrameTimestampsMS: usedFrames,
		UsedObservationIDs: usedObservationIDs, ToolCalls: session.toolCalls,
		OrchestratorModel: a.orchestrator.Model(), ObserverModel: a.observer.Model(),
		OrchestratorPromptVersion: OrchestratorPromptVersion, ObserverPromptVersion: ObserverPromptVersion,
		SegmentIDsByTimestamp: segmentIDsForTimestamps(windows, usedFrames),
		Latency:               latency, Trace: traces, Range: session.allowed,
	}, nil
}

func (a *Agent) finishNoEvidence(request Request, session *session, windows []RetrievalWindow, started time.Time, latency map[string]time.Duration, traces []AgentTrace, observations map[string]ToolResult) (Result, error) {
	latency["vision_total"] = time.Since(started)
	usedObservationIDs, observationTexts, usedFrames := collectObservationEvidence(observations)
	trace := AgentTrace{
		ID: "final", Step: len(traces) + 1, Status: "completed", Action: TraceFinal,
		Summary: "检查预算已用尽，未找到足够画面证据", EvidenceStatus: "NO_EVIDENCE",
		UsedFrameTimestampsMS: append([]int64(nil), usedFrames...),
		RemainingToolCalls:    session.RemainingToolCalls(), RemainingFrameBudget: session.RemainingFrames(),
	}
	if err := reportTrace(request.OnTrace, trace); err != nil {
		return Result{}, err
	}
	traces = append(traces, trace)
	return Result{
		Answer: "在允许的检查次数和帧预算内，没有观察到足以确认该问题的画面证据。",
		Status: StatusNoEvidence, Observation: strings.Join(observationTexts, "\n"),
		SampledFrameTimestampsMS: timestampsOf(session.Frames()), UsedFrameTimestampsMS: usedFrames,
		UsedObservationIDs: usedObservationIDs,
		ToolCalls:          session.toolCalls, OrchestratorModel: a.orchestrator.Model(), ObserverModel: a.observer.Model(),
		OrchestratorPromptVersion: OrchestratorPromptVersion, ObserverPromptVersion: ObserverPromptVersion,
		SegmentIDsByTimestamp: segmentIDsForTimestamps(windows, usedFrames), Latency: latency, Trace: traces, Range: session.allowed,
	}, nil
}

func collectObservationEvidence(observations map[string]ToolResult) ([]string, []string, []int64) {
	observationIDs := make([]string, 0, len(observations))
	for id, value := range observations {
		if value.OK && strings.TrimSpace(value.Observation) != "" {
			observationIDs = append(observationIDs, id)
		}
	}
	// 观察 ID 由后端按调用顺序生成，排序后再聚合可避免 map 迭代导致审计记录不稳定。
	slices.Sort(observationIDs)
	observationTexts := make([]string, 0, len(observationIDs))
	usedFrames := make([]int64, 0)
	for _, id := range observationIDs {
		value := observations[id]
		observationTexts = append(observationTexts, value.Observation)
		// 帧来源以后端真实传给视觉模型的采样帧为准，不再接受主 Agent 自报。
		usedFrames = append(usedFrames, value.SampledFrameTimestampsMS...)
	}
	slices.Sort(usedFrames)
	usedFrames = slices.Compact(usedFrames)
	return observationIDs, observationTexts, usedFrames
}

func hasSuccessfulObservation(observations map[string]ToolResult) bool {
	for _, value := range observations {
		if value.OK && strings.TrimSpace(value.Observation) != "" {
			return true
		}
	}
	return false
}

func toolResultMessage(callID string, result ToolResult) LoopMessage {
	encoded, _ := json.Marshal(result)
	return LoopMessage{Role: "tool", ToolCallID: callID, Content: string(encoded)}
}

func lastDialogueTurns(history []HistoryTurn) []HistoryTurn {
	start := max(0, len(history)-MaxDialogueTurns)
	return append([]HistoryTurn(nil), history[start:]...)
}

func buildSuggestedRange(input InspectVideoRangeInput, durationMS int64) (Range, bool) {
	startMS := max(int64(0), input.StartMS)
	endMS := min(durationMS, input.EndMS)
	if startMS >= durationMS || endMS <= startMS {
		return Range{}, false
	}
	if endMS-startMS > MaxRangeMS {
		endMS = startMS + MaxRangeMS
	}
	return Range{StartMS: startMS, EndMS: endMS}, true
}

func safeTraceReason(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return "需要进一步检查关键画面"
	}
	lower := strings.ToLower(value)
	if strings.Contains(lower, "http://") || strings.Contains(lower, "https://") || strings.Contains(value, "\\") || strings.Contains(value, "/") {
		return "需要进一步检查关键画面"
	}
	return truncateRunes(value, MaxToolReasonRunes)
}

func truncateRunes(value string, maximum int) string {
	if maximum <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	return string(runes[:maximum])
}

func safeToolError(err error) (string, string, bool) {
	switch {
	case errors.Is(err, ErrToolBudget):
		return "TOOL_BUDGET_EXHAUSTED", "画面检查次数已用尽", true
	case errors.Is(err, ErrFrameBudget):
		return "FRAME_BUDGET_EXCEEDED", "该次检查会超过画面帧预算", true
	case errors.Is(err, ErrRangeTooShort):
		return "RANGE_TOO_SHORT", "检查区间过短，无法提取至少 4 帧画面", true
	case errors.Is(err, ErrRangeInvalid):
		return "INVALID_RANGE", "主 Agent 给出的检查区间或间隔无效", true
	case errors.Is(err, ErrRangeOutOfBounds):
		return "RANGE_NOT_AUTHORIZED", "该时间区间尚未获得用户授权", true
	default:
		return "TOOL_FAILED", "画面提取失败", false
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
