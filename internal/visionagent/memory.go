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

func buildCombinedRetrievalText(windows []RetrievalWindow, memories []Memory, current Decision) string {
	var baseBuilder strings.Builder
	for _, window := range windows {
		if baseBuilder.Len() > 0 {
			baseBuilder.WriteString("\n\n")
		}
		fmt.Fprintf(&baseBuilder, "[原始检索窗口 %s-%s segment_id=%s]\n%s",
			formatTimestamp(window.StartMS), formatTimestamp(window.EndMS), window.SegmentID, strings.TrimSpace(window.RetrievalText))
	}

	var memoryBuilder strings.Builder
	for _, memory := range memories {
		if memory.Status != StatusGrounded || !overlapsAnyWindow(memory, windows) {
			continue
		}
		fmt.Fprintf(&memoryBuilder, "\n\n[历史画面观察 %s-%s]\n%s",
			formatTimestamp(memory.StartMS), formatTimestamp(memory.EndMS), memory.Observation)
	}
	currentText := ""
	if current.EvidenceStatus == "GROUNDED" {
		currentText = "\n\n[本轮画面观察]\n" + current.Observation
	}

	// 先为本轮观察和相关历史观察保留预算，原始检索文本使用剩余空间。
	currentText = truncateRunes(currentText, MaxCombinedContextRunes)
	remaining := MaxCombinedContextRunes - utf8.RuneCountInString(currentText)
	memoryText := truncateRunes(memoryBuilder.String(), remaining)
	remaining -= utf8.RuneCountInString(memoryText)
	baseText := truncateRunes(baseBuilder.String(), remaining)
	return baseText + memoryText + currentText
}

func overlapsAnyWindow(memory Memory, windows []RetrievalWindow) bool {
	for _, window := range windows {
		if memory.StartMS < window.EndMS && memory.EndMS > window.StartMS {
			return true
		}
	}
	return false
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
