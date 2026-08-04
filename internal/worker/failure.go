package worker

import (
	"errors"
	"net/http"

	"github.com/openai/openai-go"
)

type failureAction uint8

const (
	failureActionRetry failureAction = iota
	failureActionFailWithDeadLetter
	failureActionFailWithoutDeadLetter
)

// terminalProcessError 表示重复执行也无法恢复的业务错误。
// deadLetter=false 专用于用户输入问题，避免把预期内的无效视频送入运维死信队列。
type terminalProcessError struct {
	err        error
	deadLetter bool
}

func (e *terminalProcessError) Error() string { return e.err.Error() }
func (e *terminalProcessError) Unwrap() error { return e.err }

func userInputProcessError(err error) error {
	if err == nil {
		return nil
	}
	return &terminalProcessError{err: err, deadLetter: false}
}

// permanentProcessError 表示任务级重试无法改善的处理器或模型契约错误，需要进入 DLQ 排查。
func permanentProcessError(err error) error {
	if err == nil {
		return nil
	}
	return &terminalProcessError{err: err, deadLetter: true}
}

// decideFailureAction 同时考虑错误性质和当前任务次数，保证不可恢复错误不会消耗任务级重试。
func decideFailureAction(processErr error, currentAttempt, maxAttempts int) failureAction {
	var terminalErr *terminalProcessError
	if errors.As(processErr, &terminalErr) {
		if terminalErr.deadLetter {
			return failureActionFailWithDeadLetter
		}
		return failureActionFailWithoutDeadLetter
	}

	var apiErr *openai.Error
	if errors.As(processErr, &apiErr) && isPermanentModelAPIStatus(apiErr.StatusCode) {
		return failureActionFailWithDeadLetter
	}
	if currentAttempt < maxAttempts {
		return failureActionRetry
	}
	return failureActionFailWithDeadLetter
}

// isPermanentModelAPIStatus 只拦截明确无法通过重试恢复的模型接口错误。
// 408、409、429 和 5xx 会落入默认重试路径；400/422 对应无效参数，404 通常对应模型不存在。
func isPermanentModelAPIStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}
