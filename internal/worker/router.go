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
// OCRProcessor 沿用现有 TaskVision 路由键，保持 RabbitMQ routing key、数据库任务类型和协调逻辑兼容。
func NewRouterProcessor(asr *ASRProcessor, ocr *OCRProcessor) *RouterProcessor {
	r := &RouterProcessor{
		processors: make(map[string]Processor),
	}
	if asr != nil {
		r.processors[model.TaskASR] = asr
	}
	if ocr != nil {
		r.processors[model.TaskVision] = ocr
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
