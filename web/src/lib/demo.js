export const demoVideos = [
  {
    id: 'demo-product',
    title: '如何打造真正好用的 AI 产品',
    status: 'ready',
    duration_ms: 2_748_000,
    created_at: new Date(Date.now() - 1000 * 60 * 38).toISOString(),
    source_url: 'https://example.com/ai-product',
    summary:
      '本期访谈围绕 AI 产品的价值判断展开：优秀的产品不只是在旧流程上增加模型能力，而是重构用户完成任务的方式。讨论还涉及原型验证、信任设计，以及如何把模型的不确定性转化为可理解的产品反馈。',
    conversationId: 'demo-conversation',
  },
  {
    id: 'demo-research',
    title: '从零建立个人研究工作流',
    status: 'processing',
    duration_ms: 1_935_000,
    created_at: new Date(Date.now() - 1000 * 60 * 60 * 20).toISOString(),
    source_url: 'https://example.com/research',
    summary: '',
    conversationId: 'demo-research-conversation',
  },
  {
    id: 'demo-reading',
    title: '深度阅读：注意力与信息筛选',
    status: 'ready',
    duration_ms: 3_360_000,
    created_at: new Date(Date.now() - 1000 * 60 * 60 * 48).toISOString(),
    source_url: 'https://example.com/deep-reading',
    summary: '视频讨论了信息过载时代的阅读策略，以及如何通过问题驱动、间隔回顾和主动输出提高知识留存。',
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
