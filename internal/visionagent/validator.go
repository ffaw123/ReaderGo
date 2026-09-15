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
	ErrRangeRequired    = errors.New("vision range is required")
	ErrRangeInvalid     = errors.New("vision range is invalid")
	ErrRangeTooLong     = errors.New("vision range exceeds 30 seconds")
	ErrRangeOutOfBounds = errors.New("vision range exceeds video duration")
	ErrNotReady         = errors.New("vision source or retrieval context is not ready")
	ErrUnavailable      = errors.New("vision agent is unavailable")
	ErrInvalidOutput    = errors.New("vision agent returned invalid output")
	ErrToolBudget       = errors.New("vision tool call budget exhausted")
	ErrFrameBudget      = errors.New("vision frame budget exceeded")
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
	return nil
}

func frameTimestamps(value InspectVideoRangeInput) ([]int64, error) {
	if value.StartMS < 0 || value.EndMS <= value.StartMS {
		return nil, ErrRangeInvalid
	}
	if value.IntervalMS < MinIntervalMS {
		return nil, fmt.Errorf("interval_ms must be at least %d: %w", MinIntervalMS, ErrRangeInvalid)
	}
	count := (value.EndMS - value.StartMS + value.IntervalMS - 1) / value.IntervalMS
	if count > MaxUniqueFrames {
		return nil, ErrFrameBudget
	}
	result := make([]int64, 0, count)
	for timestamp := value.StartMS; timestamp < value.EndMS; timestamp += value.IntervalMS {
		result = append(result, timestamp)
	}
	return result, nil
}

func decodeDecision(content string, sampled map[int64]Frame) (Decision, error) {
	var decision Decision
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decision); err != nil {
		return decision, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return decision, errors.New("vision decision contains trailing data")
	}
	switch decision.Action {
	case ActionCallTool:
		if decision.Tool == nil || decision.Tool.Name != ToolName || decision.EvidenceStatus != "" || decision.Observation != "" || len(decision.UsedFrameTimestampsMS) != 0 {
			return decision, ErrInvalidOutput
		}
	case ActionFinal:
		if decision.Tool != nil || (decision.EvidenceStatus != "GROUNDED" && decision.EvidenceStatus != "NO_EVIDENCE") {
			return decision, ErrInvalidOutput
		}
		observation := strings.TrimSpace(decision.Observation)
		if decision.EvidenceStatus == "GROUNDED" {
			if observation == "" || utf8.RuneCountInString(observation) > MaxObservationRunes || len(decision.UsedFrameTimestampsMS) == 0 {
				return decision, ErrInvalidOutput
			}
			for _, timestamp := range decision.UsedFrameTimestampsMS {
				if _, ok := sampled[timestamp]; !ok {
					return decision, fmt.Errorf("used frame %d was not sampled: %w", timestamp, ErrInvalidOutput)
				}
			}
			slices.Sort(decision.UsedFrameTimestampsMS)
			decision.UsedFrameTimestampsMS = slices.Compact(decision.UsedFrameTimestampsMS)
			decision.Observation = observation
		} else if observation != "" || len(decision.UsedFrameTimestampsMS) != 0 {
			return decision, ErrInvalidOutput
		}
	default:
		return decision, ErrInvalidOutput
	}
	return decision, nil
}

func decodeFinalAnswer(content string, sampled map[int64]Frame) (FinalAnswer, error) {
	var answer FinalAnswer
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&answer); err != nil {
		return answer, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return answer, errors.New("vision answer contains trailing data")
	}
	answer.Answer = strings.TrimSpace(answer.Answer)
	if answer.Answer == "" {
		return answer, ErrInvalidOutput
	}
	if len(sampled) > 0 && len(answer.UsedFrameTimestampsMS) == 0 {
		return answer, fmt.Errorf("grounded answer omitted frame citations: %w", ErrInvalidOutput)
	}
	for _, timestamp := range answer.UsedFrameTimestampsMS {
		if _, ok := sampled[timestamp]; !ok {
			return answer, fmt.Errorf("answer cited unsampled frame %d: %w", timestamp, ErrInvalidOutput)
		}
	}
	slices.Sort(answer.UsedFrameTimestampsMS)
	answer.UsedFrameTimestampsMS = slices.Compact(answer.UsedFrameTimestampsMS)
	return answer, nil
}

func within(outer Range, inner InspectVideoRangeInput) bool {
	return inner.StartMS >= outer.StartMS && inner.EndMS <= outer.EndMS
}
