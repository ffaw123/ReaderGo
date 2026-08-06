// 预览模式复用真实任务字段，让进度步骤与后端模式保持一致。
const succeededDemoJobs = [
  { task_type: 'rag_index', status: 'succeeded' },
  { task_type: 'summary', status: 'succeeded' },
  { task_type: 'embedding', status: 'succeeded' },
]

export const demoVideos = [
  {
    id: 'demo-product',
    title: '如何打造真正好用的 AI 产品',
    status: 'ready',
    duration_ms: 2_748_000,
    processing_duration_ms: 214_000,
    index_segment_count: 46,
    created_at: new Date(Date.now() - 1000 * 60 * 38).toISOString(),
    source_url: 'https://example.com/ai-product',
    jobs: succeededDemoJobs.map((job) => ({ ...job })),
    summary: `# 如何打造真正好用的 AI 产品

## 总体概览

本期访谈围绕 AI 产品的价值判断展开：优秀的产品不只是在旧流程上增加模型能力，而是重构用户完成任务的方式。

## 内容章节

### 1. 从真实问题出发

讲者首先强调，应先确认用户在哪个环节真实受阻，再决定哪些步骤值得交给 AI。

#### 本章要点

- 用低成本原型验证任务频率与痛点强度
- 避免做出能力很强但缺少稳定场景的功能

### 2. 设计可信赖的模型反馈

随后讨论转向信任设计，以及如何把模型的不确定性转化为用户能够理解和判断的产品反馈。`,
    conversationId: 'demo-conversation',
  },
  {
    id: 'demo-research',
    title: '从零建立个人研究工作流',
    status: 'processing',
    duration_ms: 1_935_000,
    processing_duration_ms: 104_000,
    index_segment_count: 32,
    created_at: new Date(Date.now() - 1000 * 60 * 60 * 20).toISOString(),
    source_url: 'https://example.com/research',
    jobs: [
      { task_type: 'rag_index', status: 'succeeded' },
      { task_type: 'summary', status: 'running' },
      { task_type: 'embedding', status: 'pending' },
    ],
    summary: '',
    conversationId: 'demo-research-conversation',
  },
  {
    id: 'demo-reading',
    title: '深度阅读：注意力与信息筛选',
    status: 'ready',
    duration_ms: 3_360_000,
    processing_duration_ms: 247_000,
    index_segment_count: 56,
    created_at: new Date(Date.now() - 1000 * 60 * 60 * 48).toISOString(),
    source_url: 'https://example.com/deep-reading',
    jobs: succeededDemoJobs.map((job) => ({ ...job })),
    summary: `# 深度阅读：注意力与信息筛选

## 总体概览

视频讨论了信息过载时代的阅读策略，以及如何通过问题驱动、间隔回顾和主动输出提高知识留存。

## 内容章节

### 1. 用问题筛选信息

阅读前先明确需要解决的问题，以此判断哪些信息值得投入注意力。

### 2. 通过主动输出巩固理解

将间隔回顾与主动表达结合，检验自己是否真正掌握内容。`,
    conversationId: 'demo-reading-conversation',
  },
]

export const demoMessages = [
  {
    id: 'message-1',
    role: 'assistant',
    content: '视频已经整理好了。你可以直接提问，我会结合转写内容与关键画面回答，并标注对应时间。',
    created_at: new Date(Date.now() - 1000 * 60 * 4).toISOString(),
  },
  {
    id: 'message-2',
    role: 'user',
    content: '视频里提到的“先验证问题，再设计能力”具体是什么意思？',
    created_at: new Date(Date.now() - 1000 * 60 * 3).toISOString(),
  },
  {
    id: 'message-3',
    role: 'assistant',
    content:
      '它强调不要从“模型能做什么”出发，而应先确认用户在哪个环节真实受阻。视频给出的做法是：先用低成本原型验证任务频率与痛点强度，再决定哪些步骤值得交给 AI。这样可以避免做出能力很强、但没有稳定使用场景的功能。',
    citations: [{ time: '18:42', label: '问题验证与原型' }, { time: '21:08', label: '能力边界' }],
    created_at: new Date(Date.now() - 1000 * 60 * 2).toISOString(),
  },
]
