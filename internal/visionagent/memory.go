package visionagent

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

func trimMemories(values []Memory) []Memory {
	result := make([]Memory, 0, min(len(values), MaxMemoryTurns))
	remaining := MaxMemoryRunes
	for _, value := range values {
		if len(result) >= MaxMemoryTurns || remaining <= 0 {
			break
		}
		text := truncateRunes(strings.TrimSpace(value.Observation), remaining)
		if value.Status == StatusGrounded && text == "" {
			continue
		}
		value.Observation = text
		remaining -= utf8.RuneCountInString(text)
		result = append(result, value)
	}
	return result
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

func formatTimestamp(milliseconds int64) string {
	if milliseconds < 0 {
		milliseconds = 0
	}
	hours := milliseconds / 3_600_000
	minutes := (milliseconds % 3_600_000) / 60_000
	seconds := (milliseconds % 60_000) / 1_000
	millis := milliseconds % 1_000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, seconds, millis)
}
