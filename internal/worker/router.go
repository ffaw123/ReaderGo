package worker

import (
	"context"
	"readergo/internal/model"
	"readergo/internal/queue"
)

// RouterProcessor 按 TaskType 将消息分发给注册的 Processor；
// 未注册的任务类型回退到 PlaceholderProcessor，保证流水线不被未知类型阻塞。
type RouterProcessor struct {
	processors map[string]Processor
}

// NewRouterProcessor 创建路由器，参数按需传入已初始化的 Processor。
// 当前仅接入 ASR，后续依次添加 Vision、RAGIndex、Summary 即可。
func NewRouterProcessor(asr *ASRProcessor) *RouterProcessor {
	r := &RouterProcessor{
		processors: make(map[string]Processor),
	}
	if asr != nil {
		r.processors[model.TaskASR] = asr
	}
	return r
}

// Process 实现 Processor 接口，按 TaskType 路由到对应处理器；
// 未注册的阶段回退到 PlaceholderProcessor，避免阻塞流水线。
func (r *RouterProcessor) Process(ctx context.Context, job queue.VideoJobMessage) error {
	p, ok := r.processors[job.TaskType]
	if !ok {
		return PlaceholderProcessor{}.Process(ctx, job)
	}
	return p.Process(ctx, job)
}
