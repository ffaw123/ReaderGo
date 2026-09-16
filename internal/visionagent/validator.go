package visionagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

var (
	ErrRangeRequired                = errors.New("vision range is required")
	ErrRangeInvalid                 = errors.New("vision range is invalid")
	ErrRangeTooShort                = errors.New("vision range is too short for four frames")
	ErrRangeTooLong                 = errors.New("vision range exceeds 30 seconds")
	ErrRangeOutOfBounds             = errors.New("vision range exceeds video duration")
	ErrNotReady                     = errors.New("vision source or retrieval context is not ready")
	ErrUnavailable                  = errors.New("vision agent is unavailable")
	ErrInvalidOutput                = errors.New("vision agent returned invalid output")
	ErrOutputTruncated              = errors.New("vision model output was truncated")
	ErrToolBudget                   = errors.New("vision tool call budget exhausted")
	ErrFrameBudget                  = errors.New("vision frame budget exceeded")
	ErrRangeConfirmationUnavailable = errors.New("vision range confirmation is unavailable")
)

func ValidateRange(value Range, durationMS int64) error {
	if value.StartMS < 0 || value.EndMS <= value.StartMS {
		return ErrRangeInvalid
	}
	if value.EndMS-value.StartMS > MaxRangeMS {
		return ErrRangeTooLong
	}
	if durationMS <= 0 || value.EndMS > durationMS {
		return ErrRangeOutOfBounds
	}
	if value.EndMS-value.StartMS <= int64(MinObservationFrames-1)*MinIntervalMS {
		return ErrRangeTooShort
	}
	return nil
}

func frameTimestamps(value InspectVideoRangeInput) ([]int64, error) {
	if value.StartMS < 0 || value.EndMS <= value.StartMS {
		return nil, ErrRangeInvalid
	}
	if value.IntervalMS < MinIntervalMS {
		return nil, fmt.Errorf("interval_ms must be at least %d: %w", MinIntervalMS, ErrRangeInvalid)
	}
	durationMS := value.EndMS - value.StartMS
	// 中文注释：将模型给出的间隔夹在可生成 4～18 帧的范围内，同时保证发送给视觉模型的 fps 不低于 0.1。
	minIntervalMS := max(MinIntervalMS, (durationMS+int64(MaxUniqueFrames)-1)/int64(MaxUniqueFrames))
	maxIntervalMS := min(MaxIntervalMS, (durationMS-1)/int64(MinObservationFrames-1))
	if minIntervalMS > maxIntervalMS {
		return nil, ErrRangeTooShort
	}
	intervalMS := min(max(value.IntervalMS, minIntervalMS), maxIntervalMS)
	count := (durationMS + intervalMS - 1) / intervalMS
	result := make([]int64, 0, count)
	for timestamp := value.StartMS; timestamp < value.EndMS; timestamp += intervalMS {
		result = append(result, timestamp)
	}
	if len(result) < MinObservationFrames || len(result) > MaxUniqueFrames {
		return nil, ErrFrameBudget
	}
	return result, nil
}

func decodeToolArguments(content string) (InspectVideoRangeInput, error) {
	var arguments InspectVideoRangeInput
	if err := decodeStrict(content, &arguments, "vision tool arguments"); err != nil {
		return arguments, err
	}
	if _, err := frameTimestamps(arguments); err != nil {
		return arguments, err
	}
	return arguments, nil
}

func decodeFinalDecision(content string, observations map[string]ToolResult) (FinalDecision, error) {
	var decision FinalDecision
	if err := decodeStrict(content, &decision, "orchestrator final output"); err != nil {
		return decision, fmt.Errorf("%v: %w", err, ErrInvalidOutput)
	}
	decision.Answer = strings.TrimSpace(decision.Answer)
	if decision.Action != ActionFinal || decision.Answer == "" || utf8.RuneCountInString(decision.Answer) > MaxFinalAnswerRunes {
		return decision, ErrInvalidOutput
	}
	if decision.EvidenceStatus != "GROUNDED" && decision.EvidenceStatus != "NO_EVIDENCE" {
		return decision, ErrInvalidOutput
	}
	slices.Sort(decision.UsedObservationIDs)
	decision.UsedObservationIDs = slices.Compact(decision.UsedObservationIDs)
	if decision.EvidenceStatus == "NO_EVIDENCE" {
		if len(decision.UsedObservationIDs) != 0 {
			return decision, ErrInvalidOutput
		}
		return decision, nil
	}
	if len(decision.UsedObservationIDs) == 0 {
		return decision, ErrInvalidOutput
	}
	if observations == nil {
		return decision, nil
	}
	for _, id := range decision.UsedObservationIDs {
		observation, ok := observations[id]
		if !ok || !observation.OK || strings.TrimSpace(observation.Observation) == "" {
			return decision, fmt.Errorf("unknown or unusable observation_id %q: %w", id, ErrInvalidOutput)
		}
	}
	return decision, nil
}

func safeOrchestratorError(err error) (string, string) {
	message := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, ErrOutputTruncated):
		return "ORCHESTRATOR_OUTPUT_TRUNCATED", "主 Agent 输出被截断"
	case strings.Contains(message, "unknown field"):
		return "ORCHESTRATOR_UNKNOWN_FIELD", "主 Agent 返回了未允许的字段"
	case strings.Contains(message, "invalid character") || strings.Contains(message, "cannot unmarshal") ||
		strings.Contains(message, "unexpected eof") || strings.Contains(message, "trailing data"):
		return "ORCHESTRATOR_INVALID_JSON", "主 Agent 最终 JSON 格式无效"
	case strings.Contains(message, "observation_id"):
		return "ORCHESTRATOR_INVALID_OBSERVATION_ID", "主 Agent 引用了无效的观察 ID"
	case errors.Is(err, ErrInvalidOutput):
		return "ORCHESTRATOR_INVALID_OUTPUT", "主 Agent 最终结果不符合证据约束"
	default:
		return "ORCHESTRATOR_ERROR", "主 Agent 调用失败"
	}
}

func decodeObservation(content string) (Observation, error) {
	text := strings.TrimSpace(content)
	if text == "" || utf8.RuneCountInString(text) > MaxObservationRunes {
		return Observation{}, ErrInvalidOutput
	}
	// 中文注释：观察器协议是纯文本，拒绝 JSON 和代码块，避免残缺结构被误当作视觉事实传给主 Agent。
	if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") || strings.HasPrefix(text, "```") {
		return Observation{}, ErrInvalidOutput
	}
	return Observation{Observation: text}, nil
}

func safeObserverError(err error) (string, string) {
	switch {
	case errors.Is(err, ErrOutputTruncated):
		return "VISION_OUTPUT_TRUNCATED", "视觉观察器输出被截断"
	case errors.Is(err, ErrInvalidOutput):
		return "VISION_INVALID_OUTPUT", "视觉观察器未返回有效的纯文本观察"
	default:
		return "VISION_OBSERVER_ERROR", "视觉观察器调用失败"
	}
}

func decodeStrict(content string, target any, label string) error {
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("%s contains trailing data", label)
	}
	return nil
}

func within(outer Range, inner InspectVideoRangeInput) bool {
	return inner.StartMS >= outer.StartMS && inner.EndMS <= outer.EndMS
}
