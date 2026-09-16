<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import {
  ArrowUp,
  BookOpenText,
  Check,
  ChevronDown,
  CircleHelp,
  Clock3,
  Download,
  Eye,
  FileVideo,
  Film,
  Hourglass,
  Info,
  Link2,
  LoaderCircle,
  LogOut,
  Menu,
  MessageCircleMore,
  MoreHorizontal,
  PanelLeftClose,
  Pause,
  Play,
  Plus,
  RotateCcw,
  Search,
  Sparkles,
  Trash2,
  UploadCloud,
  UserRound,
  X,
  Zap,
} from 'lucide-vue-next'
import { api, calculateContentSHA256, createFileIdentity, sameFileIdentity, uploadFileParts } from '../services/api'
import { demoMessages, demoVideos } from '../lib/demo'

const router = useRouter()
// 这里只限制页面进入时的按需请求频率，不会每十秒自动轮询视频列表。
const videoListRefreshIntervalMs = 10 * 1000
const session = ref(null)
const previewMode = ref(sessionStorage.getItem('readergo_preview') === 'true')
const videos = ref([])
const selectedId = ref('')
const messages = ref([])
const summary = ref('')
const processingJobs = ref([])
const summaryOpen = ref(false)
const loadingVideos = ref(false)
const loadingMessages = ref(false)
const sending = ref(false)
const retryMessage = ref(null)
const uploadOpen = ref(false)
const accountOpen = ref(false)
const sidebarOpen = ref(false)
const search = ref('')
const draft = ref('')
const visionEnabled = ref(false)
const visionStart = ref('')
const visionEnd = ref('')
const visionAvailable = ref(false)
const visionProgress = ref('')
const visionTrace = ref([])
const toast = ref(null)
const chatScroll = ref(null)
const fileInput = ref(null)
const uploadSource = ref('url')
const uploadUrl = ref('')
const selectedFile = ref(null)
const dragActive = ref(false)
const uploadBusy = ref(false)
const uploadProgress = ref(0)
const uploadError = ref('')
const pendingUpload = ref(null)
const uploadCanPause = ref(false)
const uploadHashing = ref(false)
const uploadCancelBusy = ref(false)
const pauseSettled = ref(true)
const statusActionBusy = ref(false)
let statusTimer
let toastTimer
let uploadAbortController
let hashAbortController

const selectedVideo = computed(() => videos.value.find((video) => video.id === selectedId.value) || null)
const hasStreamingAssistant = computed(() => messages.value.some((message) => message.streaming))
const filteredVideos = computed(() => {
  const keyword = search.value.toLowerCase()
  return videos.value.filter((video) => video.title.toLowerCase().includes(keyword))
})
const userName = computed(() => session.value?.user?.display_name || session.value?.user?.email?.split('@')[0] || '探索者')
const userInitial = computed(() => userName.value.slice(0, 1).toUpperCase())
const isReady = computed(() => selectedVideo.value?.status === 'ready')
const canUseVision = computed(() => Boolean(
  !previewMode.value && visionAvailable.value && isReady.value && selectedVideo.value?.duration_ms > 0,
))
const activeProcessingJobs = computed(() =>
  previewMode.value ? selectedVideo.value?.jobs || [] : processingJobs.value,
)

function taskSucceeded(taskType) {
  return activeProcessingJobs.value.some((job) => job.task_type === taskType && job.status === 'succeeded')
}

const uploadStageSucceeded = computed(() => {
  const video = selectedVideo.value
  if (!video) return false

  const jobs = activeProcessingJobs.value
  const prepareJobs = jobs.filter((job) => job.task_type === 'prepare')
  // URL 视频以上游 prepare 任务为准；本地直传没有 prepare，进入后续任务即说明上传成功。
  if (prepareJobs.length) return prepareJobs.some((job) => job.status === 'succeeded')
  if (jobs.some((job) => job.task_type !== 'prepare')) return true
  return ['processing', 'paused', 'ready'].includes(video.status)
})

const processSteps = computed(() => {
  const pipelineReady = isReady.value
  return [
    { key: 'upload', label: '视频上传', done: uploadStageSucceeded.value || pipelineReady },
    // 产品展示中的“转写内容”按要求对应 rag_index 任务。
    { key: 'transcript', label: '转写内容', done: taskSucceeded('rag_index') || pipelineReady },
    { key: 'summary', label: '生成摘要', done: taskSucceeded('summary') || pipelineReady },
    { key: 'index', label: '建立索引', done: taskSucceeded('embedding') || pipelineReady },
  ]
})
const processPercent = computed(
  () => (processSteps.value.filter((step) => step.done).length / processSteps.value.length) * 100,
)
const indexSegmentDisplay = computed(() => {
  const count = Number(selectedVideo.value?.index_segment_count)
  const segmentsReady = taskSucceeded('rag_index') || isReady.value
  return segmentsReady && Number.isFinite(count) ? count.toLocaleString() : '—'
})
const canPauseSelected = computed(() => {
  const video = selectedVideo.value
  if (!video) return false
  if (video.status === 'processing') return true
  // URL prepare 属于后台流水线；本地 Multipart 尚未完成时不显示处理暂停。
  return video.status === 'uploading' && /^https?:\/\//i.test(video.source_url || '')
})
const statusMeta = computed(() => {
  const percent = processPercent.value
  const statuses = {
    ready: { label: '视频处理完成', detail: '所有内容已可检索', percent: 100, tone: 'success' },
    processing: {
      label: '正在处理视频',
      detail: summary.value ? '摘要已可查看，正在完成检索索引' : '正在解析内容、建立索引和生成摘要',
      percent,
      tone: 'working',
    },
    uploading: { label: '等待上传完成', detail: '文件分片正在传输', percent, tone: 'working' },
    paused: {
      label: pauseSettled.value ? '处理已暂停' : '正在暂停处理',
      detail: pauseSettled.value ? '点击继续后将从未完成的阶段恢复' : '正在安全停止运行中的任务',
      percent,
      tone: 'paused',
    },
    failed: {
      label: '处理失败',
      detail: selectedVideo.value?.error_message || '请检查视频源后重试',
      percent,
      tone: 'danger',
    },
  }
  return statuses[selectedVideo.value?.status] || statuses.uploading
})
const sourceDisabled = computed(() => pendingUpload.value || (uploadSource.value === 'file' && selectedFile.value))
const fileDisabled = computed(() => uploadSource.value === 'url' && uploadUrl.value.trim())
const summaryExcerpt = computed(() =>
  summary.value
    .replace(/^#{1,6}\s+.*$/gm, '')
    .replace(/^[-*]\s+/gm, '')
    .replace(/\s+/g, ' ')
    .trim(),
)
const summaryBlocks = computed(() => parseSummaryMarkdown(summary.value))

function notify(message, type = 'success') {
  clearTimeout(toastTimer)
  toast.value = { message, type }
  toastTimer = setTimeout(() => (toast.value = null), 3200)
}

function parseSession() {
  try {
    session.value = JSON.parse(localStorage.getItem('readergo_session') || 'null')
  } catch {
    session.value = null
  }
}

function cacheKey() {
  return `readergo_videos_${session.value?.user?.id || 'anonymous'}`
}

function videoListCacheKey() {
  return `readergo_video_list_${session.value?.user?.id || 'anonymous'}`
}

function pendingUploadKey() {
  return `readergo_pending_upload_${session.value?.user?.id || 'anonymous'}`
}

function loadPendingUpload() {
  try {
    pendingUpload.value = JSON.parse(localStorage.getItem(pendingUploadKey()) || 'null')
  } catch {
    pendingUpload.value = null
  }
  if (pendingUpload.value) {
    uploadSource.value = 'file'
    uploadProgress.value = pendingUpload.value.progress || 0
  }
}

function savePendingUpload(uploadSession, identity, title, contentSHA256) {
  pendingUpload.value = {
    upload_id: uploadSession.upload_id,
    video_id: uploadSession.video_id,
    filename: identity.filename,
    file_size: identity.file_size,
    file_last_modified_ms: identity.file_last_modified_ms,
    file_fingerprint: identity.file_fingerprint,
    content_sha256: contentSHA256,
    expires_at: uploadSession.expires_at,
    progress: uploadSession.progress || 0,
    title,
  }
  localStorage.setItem(pendingUploadKey(), JSON.stringify(pendingUpload.value))
}

function updatePendingProgress(progress) {
  uploadProgress.value = progress
  if (!pendingUpload.value) return
  pendingUpload.value.progress = progress
  localStorage.setItem(pendingUploadKey(), JSON.stringify(pendingUpload.value))
}

function clearPendingUpload() {
  localStorage.removeItem(pendingUploadKey())
  pendingUpload.value = null
}

function getCachedEntries() {
  try {
    return JSON.parse(localStorage.getItem(cacheKey()) || '[]')
  } catch {
    return []
  }
}

function getCachedVideoList() {
  try {
    const cached = JSON.parse(localStorage.getItem(videoListCacheKey()) || 'null')
    if (!cached || !Array.isArray(cached.items)) return { fetchedAt: 0, items: [] }
    return { fetchedAt: Number(cached.fetched_at) || 0, items: cached.items }
  } catch {
    return { fetchedAt: 0, items: [] }
  }
}

function saveCachedVideoList(items, fetchedAt) {
  localStorage.setItem(
    videoListCacheKey(),
    JSON.stringify({ fetched_at: fetchedAt, items }),
  )
}

function rememberVideo(video, conversationId = '') {
  const entries = getCachedEntries().filter((item) => item.id !== video.id)
  entries.unshift({ id: video.id, title: video.title, conversationId })
  localStorage.setItem(cacheKey(), JSON.stringify(entries))

  // 上传成功后直接把接口返回的视频写入列表缓存，不为更新列表再发送一次请求。
  const cached = getCachedVideoList()
  const items = [video, ...cached.items.filter((item) => item.id !== video.id)]
  saveCachedVideoList(items, cached.fetchedAt)
}

function getConversationId(video) {
  return video.conversationId || getCachedEntries().find((item) => item.id === video.id)?.conversationId || ''
}

function messageSourceLabel(message) {
  const labels = {
    preset: '快捷回复',
    general: '通用知识',
    general_unaware: '未参考视频',
    summary: '基于视频摘要',
    rag: '基于视频片段',
    clarify: '需要澄清',
    capability_pending: '内容处理中',
    capability_failed: '处理能力不可用',
    vision_agent: '基于指定画面',
  }
  return labels[message?.metadata?.mode] || 'ReaderGo 回答'
}

function parseVideoTime(value) {
  const matched = String(value || '').trim().match(/^(?:(\d+):)?(\d{1,2}):(\d{2})(?:\.(\d{1,3}))?$/)
  if (!matched) throw new Error('时间格式应为 MM:SS、HH:MM:SS 或带毫秒的形式')
  const hasHours = matched[1] !== undefined
  const hours = hasHours ? Number(matched[1]) : 0
  const minutes = Number(matched[2])
  const seconds = Number(matched[3])
  if (seconds >= 60 || (hasHours && minutes >= 60)) throw new Error('分钟或秒数超出有效范围')
  const milliseconds = Number((matched[4] || '').padEnd(3, '0') || 0)
  return ((hours * 60 + minutes) * 60 + seconds) * 1000 + milliseconds
}

function currentVisionRequest() {
  if (!visionEnabled.value) return { enabled: false }
  if (!canUseVision.value) throw new Error('当前视频暂时不能使用画面理解')
  const startMS = parseVideoTime(visionStart.value)
  const endMS = parseVideoTime(visionEnd.value)
  if (endMS <= startMS) throw new Error('结束时间必须晚于开始时间')
  if (endMS - startMS > 30_000) throw new Error('画面理解时间区间不能超过30秒')
  if (endMS > Number(selectedVideo.value.duration_ms || 0)) throw new Error('时间区间不能超过视频时长')
  return { enabled: true, range: { start_ms: startMS, end_ms: endMS } }
}

function upsertVisionTrace(trace) {
  if (!trace?.id) return
  const next = visionTrace.value.slice()
  const index = next.findIndex((item) => item.id === trace.id)
  if (index >= 0) next[index] = trace
  else next.push(trace)
  next.sort((left, right) => Number(left.step || 0) - Number(right.step || 0))
  visionTrace.value = next

  // 回答正文开始流式输出后，继续把轨迹更新到临时助手消息中。
  const assistant = messages.value.find((message) => message.streaming)
  if (assistant) {
    assistant.metadata = { ...(assistant.metadata || {}), vision_trace: next.map((item) => ({ ...item })) }
  }
}

function formatTraceTime(milliseconds) {
  const value = Math.max(0, Number(milliseconds) || 0)
  const hours = Math.floor(value / 3_600_000)
  const minutes = Math.floor((value % 3_600_000) / 60_000)
  const seconds = Math.floor((value % 60_000) / 1000)
  const millis = value % 1000
  const prefix = hours > 0 ? `${String(hours).padStart(2, '0')}:` : ''
  return `${prefix}${String(minutes).padStart(2, '0')}:${String(seconds).padStart(2, '0')}.${String(millis).padStart(3, '0')}`
}

function traceArgumentsLabel(argumentsValue) {
  if (!argumentsValue) return ''
  return `${formatTraceTime(argumentsValue.start_ms)}–${formatTraceTime(argumentsValue.end_ms)}，间隔 ${argumentsValue.interval_ms}ms`
}

function traceActionLabel(action) {
  return {
    INSPECT_INITIAL_RANGE: '初始查看',
    INSPECT_VIDEO_RANGE: '细化查看',
    FINAL: '完成检查',
  }[action] || '画面检查'
}

async function loadCapabilities() {
  if (previewMode.value) return
  try {
    const result = await api.getCapabilities()
    visionAvailable.value = Boolean(result?.vision?.enabled)
  } catch (error) {
    visionAvailable.value = false
    if (error.status !== 401) notify(error.message, 'error')
  }
}

function handleMessageAction(action) {
  if (action?.type === 'OPEN_SUMMARY' && summary.value) summaryOpen.value = true
}

async function loadVideos() {
  if (previewMode.value) {
    videos.value = demoVideos.map((video) => ({ ...video }))
    selectedId.value = videos.value[0].id
    return
  }
  loadingVideos.value = true
  const cached = getCachedVideoList()
  const now = Date.now()
  const cacheIsFresh = now >= cached.fetchedAt && now - cached.fetchedAt < videoListRefreshIntervalMs
  let items = cached.items
  try {
    if (!cacheIsFresh) {
      const result = await api.listVideos()
      items = Array.isArray(result?.items) ? result.items : []
      saveCachedVideoList(items, Date.now())
    }
    // 会话 ID 仍沿用原有本地索引；视频目录本身以服务端列表为准。
    const localEntries = new Map(getCachedEntries().map((entry) => [entry.id, entry]))
    videos.value = items.map((video) => ({
      ...video,
      conversationId: localEntries.get(video.id)?.conversationId || '',
    }))
    // 服务端按 created_at 倒序返回，因此第一条就是默认展示的最新视频。
    selectedId.value = videos.value[0]?.id || ''
  } catch (error) {
    // 网络临时不可用时保留最近列表；401 已由请求层清理登录态并跳转。
    videos.value = cached.items
    selectedId.value = videos.value[0]?.id || ''
    if (error.status !== 401) notify(error.message, 'error')
  } finally {
    loadingVideos.value = false
  }
}

function selectVideo(video) {
  selectedId.value = video.id
  sidebarOpen.value = false
}

async function loadSelectedContent() {
  if (!selectedVideo.value) {
    messages.value = []
    summary.value = ''
    return
  }
  if (previewMode.value) {
    summary.value = selectedVideo.value.summary || ''
    messages.value = selectedVideo.value.id === 'demo-product' ? demoMessages.map((item) => ({ ...item })) : []
    return
  }

  const conversationId = getConversationId(selectedVideo.value)
  loadingMessages.value = true
  const [summaryResult, messageResult] = await Promise.allSettled([
    api.getSummary(selectedVideo.value.id),
    conversationId ? api.listMessages(conversationId) : Promise.resolve({ items: [] }),
  ])
  summary.value = summaryResult.status === 'fulfilled' ? summaryResult.value.content : ''
  messages.value = messageResult.status === 'fulfilled' ? messageResult.value.items || [] : []
  loadingMessages.value = false
}

function summaryJobSucceeded(jobs) {
  return Array.isArray(jobs) && jobs.some((job) => job.task_type === 'summary' && job.status === 'succeeded')
}

async function loadCompletedSummary(videoId) {
  try {
    const result = await api.getSummary(videoId)
    // 用户切换视频时丢弃旧请求结果，避免把上一个视频的摘要显示到当前页面。
    if (selectedVideo.value?.id === videoId) summary.value = result.content || ''
  } catch {
    // 任务状态与摘要读取之间只存在很短的提交窗口，下一轮状态轮询会继续尝试。
  }
}

async function refreshStatus(force = false) {
  if (previewMode.value || !selectedVideo.value) return
  if (!force && ['ready', 'failed'].includes(selectedVideo.value.status)) return
  try {
    const videoId = selectedVideo.value.id
    const wasReady = selectedVideo.value.status === 'ready'
    const status = await api.getVideoStatus(videoId, true)
    if (selectedVideo.value?.id !== videoId) return
    selectedVideo.value.status = status.status
    selectedVideo.value.error_message = status.error_message || ''
    // 指标区只消费状态接口返回的真实流水线耗时与落库片段数量。
    selectedVideo.value.processing_duration_ms = status.processing_duration_ms ?? null
    selectedVideo.value.index_segment_count = status.index_segment_count ?? null
    // 保存各后台任务的真实状态，进度步骤不再依赖固定百分比推断。
    processingJobs.value = Array.isArray(status.jobs) ? status.jobs : []
    pauseSettled.value = status.pause_settled !== false
    // Summary 与 Embedding 并行；摘要任务一完成就立即读取，不等待视频总体进入 ready。
    if (!summary.value && summaryJobSucceeded(status.jobs)) await loadCompletedSummary(videoId)
    if (status.status === 'ready' && !wasReady) await loadSelectedContent()
  } catch {
    // 轮询失败不打断用户；显式操作时再展示错误。
  }
}

async function toggleProcessing() {
  const video = selectedVideo.value
  if (!video || statusActionBusy.value) return
  if (video.status === 'paused' && !pauseSettled.value) return

  statusActionBusy.value = true
  try {
    if (previewMode.value) {
      if (video.status === 'paused') {
        video.status = video.previewPausedFrom || 'processing'
        pauseSettled.value = true
        notify('已继续处理视频')
      } else {
        video.previewPausedFrom = video.status
        video.status = 'paused'
        pauseSettled.value = true
        notify('视频处理已暂停')
      }
      return
    }
    if (video.status === 'paused') {
      const result = await api.resumeVideo(video.id)
      video.status = result.status
      pauseSettled.value = true
      notify('已继续处理视频')
    } else {
      const result = await api.pauseVideo(video.id)
      video.status = result.status
      pauseSettled.value = result.pause_settled
      notify(result.pause_settled ? '视频处理已暂停' : '正在安全暂停视频处理')
    }
  } catch (error) {
    notify(error.message, 'error')
    await refreshStatus()
  } finally {
    statusActionBusy.value = false
  }
}

async function ensureConversation(video) {
  let conversationId = getConversationId(video)
  if (conversationId) return conversationId
  const conversation = await api.createConversation(video.id, video.title || '视频讨论')
  conversationId = conversation.id
  video.conversationId = conversationId
  rememberVideo(video, conversationId)
  return conversationId
}

async function sendMessage() {
  const content = draft.value.trim()
  if (!content || sending.value || !selectedVideo.value) return
  let vision
  try {
    // 发送前固定本次画面参数，后续切换开关不会改变正在执行的请求。
    vision = retryMessage.value?.content === content
      ? retryMessage.value.vision
      : currentVisionRequest()
  } catch (error) {
    notify(error.message, 'error')
    return
  }
  draft.value = ''
  sending.value = true
  visionProgress.value = vision.enabled ? '正在准备画面理解' : ''
  visionTrace.value = []
  const clientMessageId = retryMessage.value?.content === content
    ? retryMessage.value.id
    : crypto.randomUUID()
  const optimistic = {
    id: `local-${clientMessageId}`,
    role: 'user',
    content,
    created_at: new Date().toISOString(),
  }
  const streamingAssistantId = `stream-${clientMessageId}`
  messages.value.push(optimistic)
  await nextTick()
  scrollChat()

  try {
    if (previewMode.value) {
      await new Promise((resolve) => setTimeout(resolve, 700))
      messages.value.push({
        id: `demo-${Date.now()}`,
        role: 'assistant',
        content:
          '这是预览模式下的示例回答。连接 ReaderGo 后端后，这里会结合视频转写、关键画面与时间轴生成答案，并返回带时间戳的原片引用。',
        citations: [{ segment_id: 'demo-segment', start_ms: 744000, end_ms: 804000, label: '12:24' }],
        created_at: new Date().toISOString(),
      })
    } else {
      const conversationId = await ensureConversation(selectedVideo.value)
      // client_message_id 让网络重试返回同一组消息，不会重复调用模型。
      const response = await api.sendMessage(conversationId, content, clientMessageId, vision, (delta) => {
        if (!delta) return
        let assistant = messages.value.find((message) => message.id === streamingAssistantId)
        if (!assistant) {
          // 第一段到达时用临时消息替换等待动画，后续原位追加以形成打字效果。
          assistant = {
            id: streamingAssistantId,
            role: 'assistant',
            content: '',
            citations: [],
            metadata: { vision_trace: visionTrace.value.map((item) => ({ ...item })) },
            streaming: true,
            created_at: new Date().toISOString(),
          }
          messages.value.push(assistant)
        }
        assistant.content += delta
        nextTick().then(scrollChat)
      }, (progress) => {
        visionProgress.value = progress?.message || ''
      }, (trace) => {
        upsertVisionTrace(trace)
        nextTick().then(scrollChat)
      })
      messages.value = messages.value.map((message) =>
        message.id === optimistic.id ? response.user_message : message,
      )
      const streamedIndex = messages.value.findIndex((message) => message.id === streamingAssistantId)
      if (streamedIndex >= 0) messages.value[streamedIndex] = response.assistant_message
      else messages.value.push(response.assistant_message)
      retryMessage.value = null
    }
  } catch (error) {
    messages.value = messages.value.filter(
      (message) => message.id !== optimistic.id && message.id !== streamingAssistantId,
    )
    draft.value = content
    // 请求可能已在服务端完成；保持同一 ID 可安全取回首次结果。
    retryMessage.value = { id: clientMessageId, content, vision }
    notify(error.message, 'error')
  } finally {
    sending.value = false
    visionProgress.value = ''
    visionTrace.value = []
    await nextTick()
    scrollChat()
  }
}

function handleComposerKeydown(event) {
  if (event.key === 'Enter' && !event.shiftKey) {
    event.preventDefault()
    sendMessage()
  }
}

function scrollChat() {
  if (chatScroll.value) chatScroll.value.scrollTop = chatScroll.value.scrollHeight
}

async function clearContext() {
  if (previewMode.value) {
    messages.value = []
    notify('当前预览对话已清空')
    return
  }
  if (!selectedVideo.value || sending.value) return
  try {
    // 新建会话即可清空后续模型上下文，同时保留旧会话记录而不执行不可恢复删除。
    const conversation = await api.createConversation(selectedVideo.value.id, selectedVideo.value.title || '视频讨论')
    selectedVideo.value.conversationId = conversation.id
    rememberVideo(selectedVideo.value, conversation.id)
    messages.value = []
    retryMessage.value = null
    notify('已开始新的对话上下文')
  } catch (error) {
    notify(error.message, 'error')
  }
}

// 后端只生成受控标题、段落和列表；这里用 Vue 文本插值安全渲染，不注入模型返回的 HTML。
function parseSummaryMarkdown(markdown) {
  const blocks = []
  const lines = String(markdown || '').replace(/\r\n?/g, '\n').split('\n')
  let paragraph = []
  const flushParagraph = () => {
    const text = paragraph.join(' ').trim()
    if (text) blocks.push({ type: 'paragraph', text })
    paragraph = []
  }

  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index]
    const heading = /^(#{1,4})\s+(.+)$/.exec(line)
    if (heading) {
      flushParagraph()
      blocks.push({ type: 'heading', level: heading[1].length, text: heading[2].trim() })
      continue
    }
    if (/^-\s+/.test(line)) {
      flushParagraph()
      const items = []
      while (index < lines.length && /^-\s+/.test(lines[index])) {
        items.push(lines[index].replace(/^-\s+/, '').trim())
        index += 1
      }
      index -= 1
      blocks.push({ type: 'list', items })
      continue
    }
    if (!line.trim()) {
      flushParagraph()
      continue
    }
    paragraph.push(line.trim())
  }
  flushParagraph()
  return blocks
}

function downloadSummary() {
  if (!summary.value || !selectedVideo.value) return
  const safeTitle = (selectedVideo.value.title || '视频摘要').replace(/[\\/:*?"<>|]/g, '_').trim() || '视频摘要'
  const blob = new Blob([summary.value], { type: 'text/markdown;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = `${safeTitle}-摘要.md`
  document.body.appendChild(anchor)
  anchor.click()
  anchor.remove()
  URL.revokeObjectURL(url)
  notify('Markdown 摘要已开始下载')
}

async function openUpload() {
  uploadOpen.value = true
  sidebarOpen.value = false
  uploadError.value = ''
  if (!pendingUpload.value || previewMode.value) return
  uploadSource.value = 'file'
  try {
    const status = await api.getUploadStatus(pendingUpload.value.upload_id)
    pendingUpload.value.progress = status.progress
    uploadProgress.value = status.progress
    localStorage.setItem(pendingUploadKey(), JSON.stringify(pendingUpload.value))
  } catch (error) {
    if (error.status === 404 || error.status === 409) {
      clearPendingUpload()
      uploadError.value = '之前的上传会话已结束，请重新选择文件上传'
    } else {
      uploadError.value = '暂时无法读取断点，请稍后重试'
    }
  }
}

function closeUpload() {
  if (uploadBusy.value) return
  uploadOpen.value = false
  uploadUrl.value = ''
  selectedFile.value = null
  uploadProgress.value = pendingUpload.value?.progress || 0
  uploadError.value = ''
  uploadSource.value = pendingUpload.value ? 'file' : 'url'
}

function chooseUrl() {
  if (selectedFile.value || pendingUpload.value) return
  uploadSource.value = 'url'
}

function chooseFile() {
  if (uploadUrl.value.trim()) return
  uploadSource.value = 'file'
  fileInput.value?.click()
}

function setFile(file) {
  if (!file || uploadUrl.value.trim()) return
  if (!file.type.startsWith('video/')) {
    uploadError.value = '请选择常见视频格式文件'
    return
  }
  selectedFile.value = file
  uploadSource.value = 'file'
  uploadError.value = ''
}

function onDrop(event) {
  dragActive.value = false
  setFile(event.dataTransfer.files?.[0])
}

function onFileChange(event) {
  setFile(event.target.files?.[0])
  event.target.value = ''
}

function removeFile() {
  if (uploadBusy.value) return
  selectedFile.value = null
  uploadSource.value = pendingUpload.value ? 'file' : 'url'
}

function pauseLocalUpload() {
  if (!uploadAbortController) return
  // 只停止当前 PUT 和后续调度，不调用服务端取消接口，MinIO 已有分片继续保留。
  uploadAbortController.abort()
}

function cancelHashCalculation() {
  hashAbortController?.abort()
}

async function cancelPendingUpload() {
  if (!pendingUpload.value || uploadCancelBusy.value) {
    closeUpload()
    return
  }
  uploadCancelBusy.value = true
  uploadAbortController?.abort()
  try {
    await api.cancelUpload(pendingUpload.value.upload_id)
  } catch (error) {
    if (error.status !== 404 && error.status !== 409) {
      uploadError.value = error.message
      return
    }
  } finally {
    uploadCancelBusy.value = false
  }
  clearPendingUpload()
  selectedFile.value = null
  uploadProgress.value = 0
  uploadError.value = ''
  uploadSource.value = 'url'
  notify('未完成的上传已取消')
}

async function submitUpload() {
  uploadError.value = ''
  const url = uploadUrl.value.trim()
  if (!url && !selectedFile.value) {
    uploadError.value = '请粘贴视频网址或选择本地文件'
    return
  }
  if (url) {
    try {
      const parsed = new URL(url)
      if (!['http:', 'https:'].includes(parsed.protocol)) throw new Error()
    } catch {
      uploadError.value = '请输入有效的 http(s) 视频网址'
      return
    }
  }

  uploadBusy.value = true
  try {
    if (previewMode.value) {
      for (const progress of [18, 42, 68, 88, 100]) {
        await new Promise((resolve) => setTimeout(resolve, 180))
        uploadProgress.value = progress
      }
      const title = selectedFile.value?.name.replace(/\.[^.]+$/, '') || new URL(url).hostname
      const newVideo = {
        id: `demo-${Date.now()}`,
        title,
        status: 'processing',
        duration_ms: 0,
        created_at: new Date().toISOString(),
        source_url: url || `local://${selectedFile.value.name}`,
        summary: '',
        conversationId: `demo-conversation-${Date.now()}`,
      }
      videos.value.unshift(newVideo)
      selectedId.value = newVideo.id
      uploadBusy.value = false
      closeUpload()
      notify('视频已提交，正在开始分析')
      return
    }

    if (url) {
      uploadProgress.value = 36
      const result = await api.createVideo({ source_url: url, title: new URL(url).hostname })
      const video = result.video
      const conversation = await api.createConversation(video.id, video.title || '视频讨论')
      video.conversationId = conversation.id
      rememberVideo(video, conversation.id)
      videos.value.unshift(video)
      selectedId.value = video.id
      uploadProgress.value = 100
      uploadBusy.value = false
      closeUpload()
      notify('视频已提交，ReaderGo 正在后台处理')
    } else {
      const file = selectedFile.value
      const title = file.name.replace(/\.[^.]+$/, '')
      const identity = await createFileIdentity(file)
      let contentSHA256 = pendingUpload.value?.content_sha256 || ''
      let uploadSession
      if (pendingUpload.value) {
        if (!sameFileIdentity(identity, pendingUpload.value)) {
          throw new Error('所选文件与未完成上传不一致，请重新选择原文件或先取消旧上传')
        }
        try {
          uploadSession = await api.getUploadStatus(pendingUpload.value.upload_id)
        } catch (error) {
          if (error.status !== 404 && error.status !== 409) throw error
          clearPendingUpload()
        }
      }
      if (!uploadSession) {
        if (!contentSHA256) {
          hashAbortController = new AbortController()
          uploadHashing.value = true
          uploadProgress.value = 0
          try {
            contentSHA256 = await calculateContentSHA256(file, (progress) => (uploadProgress.value = progress), hashAbortController.signal)
          } finally {
            hashAbortController = null
            uploadHashing.value = false
          }
        }
        uploadSession = await api.createUpload(file, title, identity, contentSHA256)
        savePendingUpload(uploadSession, identity, title, contentSHA256)
      } else if (!sameFileIdentity(identity, uploadSession)) {
        throw new Error('服务端上传会话与所选文件不一致，已拒绝恢复')
      }

      uploadAbortController = new AbortController()
      uploadCanPause.value = true
      await uploadFileParts(file, uploadSession, updatePendingProgress, uploadAbortController.signal)
      // 所有分片已到达 MinIO 后进入不可中断的幂等完成阶段，避免把“暂停”误解为取消合并。
      uploadCanPause.value = false
      const completed = await api.completeUpload(uploadSession.upload_id)
      clearPendingUpload()
      const video = completed.video
      const conversation = await api.createConversation(video.id, video.title || title)
      video.conversationId = conversation.id
      rememberVideo(video, conversation.id)
      videos.value.unshift(video)
      selectedId.value = video.id
      uploadBusy.value = false
      closeUpload()
      notify('文件已上传完成，ReaderGo 正在后台处理')
    }
  } catch (error) {
    // 任一分片耗尽重试时立即终止同批并发 PUT，避免界面进入恢复态后后台仍继续传输。
    uploadAbortController?.abort()
    if (error.code === 'UPLOAD_RATE_LIMITED') {
      const retryAfterSeconds = Number(error.details?.retry_after_seconds) || 1
      uploadProgress.value = 0
      // 保留当前文件或 URL，用户等待令牌恢复后可以直接重新提交。
      uploadError.value = `上传过于频繁，请在 ${retryAfterSeconds} 秒后重试`
    } else if (error.code === 'UPLOAD_DUPLICATE' && error.details?.video_id) {
      try {
        const video = await api.getVideo(error.details.video_id)
        video.conversationId = getConversationId(video)
        rememberVideo(video, video.conversationId)
        videos.value = [video, ...videos.value.filter((item) => item.id !== video.id)]
        selectedId.value = video.id
        uploadProgress.value = 0
        uploadBusy.value = false
        closeUpload()
        notify('该视频已存在，已为你打开')
      } catch (loadError) {
        uploadError.value = loadError.message
      }
    } else if (error.name === 'AbortError' && error.message === '文件校验已取消') {
      uploadError.value = '文件校验已取消'
    } else if (error.name === 'AbortError') {
      uploadError.value = '上传已暂停，继续时会从 MinIO 中缺失的分片恢复'
    } else {
      uploadError.value = pendingUpload.value ? `${error.message}；可稍后继续上传` : error.message
    }
  } finally {
    hashAbortController = null
    uploadHashing.value = false
    uploadAbortController = null
    uploadCanPause.value = false
    uploadBusy.value = false
  }
}

function logout() {
  localStorage.removeItem('readergo_session')
  sessionStorage.removeItem('readergo_preview')
  router.push('/login')
}

function formatDuration(duration) {
  const milliseconds = Number(duration)
  if (!Number.isFinite(milliseconds) || milliseconds <= 0) return '—'

  // 处理时长保留到秒并累计总分钟，避免显示无必要的小时位。
  const totalSeconds = Math.floor(milliseconds / 1000)
  const minutes = Math.floor(totalSeconds / 60)
  const seconds = totalSeconds % 60
  return `${String(minutes).padStart(2, '0')}:${String(seconds).padStart(2, '0')}`
}

function formatRelative(date) {
  const minutes = Math.floor((Date.now() - new Date(date).getTime()) / 60000)
  if (minutes < 60) return `${Math.max(1, minutes)} 分钟前`
  if (minutes < 1440) return `${Math.floor(minutes / 60)} 小时前`
  if (minutes < 2880) return '昨天'
  return `${Math.floor(minutes / 1440)} 天前`
}

function statusLabel(status) {
  return { ready: '可提问', processing: '处理中', paused: '已暂停', failed: '失败', uploading: '上传中' }[status] || '处理中'
}

watch(
  selectedId,
  async () => {
    pauseSettled.value = true
    summaryOpen.value = false
    processingJobs.value = []
    visionEnabled.value = false
    visionStart.value = ''
    visionEnd.value = ''
    // 切换视频时立即清空旧摘要，避免新请求完成前短暂展示上一个视频的内容。
    summary.value = ''
    await loadSelectedContent()
    clearInterval(statusTimer)
    statusTimer = setInterval(refreshStatus, 5000)
    // 即使视频已经结束，也强制读取一次任务详情，以便逐项展示成功标记。
    await refreshStatus(true)
    await nextTick()
    scrollChat()
  },
)

onMounted(async () => {
  parseSession()
  loadPendingUpload()
  await Promise.all([loadVideos(), loadCapabilities()])
})

onBeforeUnmount(() => {
  // 离开页面时同时停止本地哈希 Worker 和正在进行的 MinIO PUT。
  hashAbortController?.abort()
  uploadAbortController?.abort()
  clearInterval(statusTimer)
  clearTimeout(toastTimer)
})
</script>

<template>
  <main class="workspace">
    <div v-if="sidebarOpen" class="mobile-backdrop" @click="sidebarOpen = false"></div>
    <aside class="sidebar" :class="{ 'sidebar--open': sidebarOpen }">
      <header class="sidebar__header">
        <RouterLink class="brand brand--light" to="/app">
          <span class="brand__mark"><Play :size="16" fill="currentColor" /></span>
          <span>ReaderGo</span>
        </RouterLink>
        <button class="sidebar-close" aria-label="关闭侧栏" @click="sidebarOpen = false">
          <PanelLeftClose :size="19" />
        </button>
      </header>

      <button class="upload-trigger" @click="openUpload">
        <span class="upload-trigger__icon"><Plus :size="20" /></span>
        <span>
          <strong>添加一段视频</strong>
          <small>网址或本地文件</small>
        </span>
        <ArrowUp :size="17" />
      </button>

      <section class="library">
        <div class="section-heading">
          <span>我的视频</span>
          <small>{{ videos.length }}</small>
        </div>
        <div class="sidebar-search">
          <Search :size="15" />
          <input v-model="search" aria-label="搜索视频" placeholder="搜索视频" />
          <button type="button" :disabled="!search" @click="search = ''">清除</button>
        </div>

        <div v-if="loadingVideos" class="sidebar-loading">
          <LoaderCircle :size="18" class="spin" /> 正在载入知识库
        </div>
        <div v-else-if="!filteredVideos.length" class="library-empty">
          <span><Film :size="21" /></span>
          <strong>知识库还是空的</strong>
          <p>添加第一段视频，开始提问。</p>
        </div>
        <div v-else class="video-list">
          <button
            v-for="video in filteredVideos"
            :key="video.id"
            class="video-item"
            :class="{ 'video-item--active': video.id === selectedId }"
            @click="selectVideo(video)"
          >
            <span class="video-thumb">
              <Play :size="14" fill="currentColor" />
            </span>
            <span class="video-item__content">
              <strong>{{ video.title || '未命名视频' }}</strong>
              <small>
                <span class="status-dot" :class="`status-dot--${video.status}`"></span>
                {{ statusLabel(video.status) }} · {{ formatRelative(video.created_at) }}
              </small>
            </span>
            <MoreHorizontal :size="17" />
          </button>
        </div>
      </section>

      <footer class="sidebar__footer">
        <button class="account-button" @click="accountOpen = !accountOpen">
          <span class="avatar">{{ userInitial }}</span>
          <span>
            <strong>{{ previewMode ? '预览访客' : userName }}</strong>
            <small>{{ previewMode ? '本地演示模式' : session?.user?.email }}</small>
          </span>
          <ChevronDown :size="16" />
        </button>
        <div v-if="accountOpen" class="account-menu">
          <button><UserRound :size="16" /> 账户设置</button>
          <button><CircleHelp :size="16" /> 帮助与反馈</button>
          <button class="danger-text" @click="logout"><LogOut :size="16" /> 退出登录</button>
        </div>
      </footer>
    </aside>

    <section class="main-panel">
      <header class="topbar">
        <button class="mobile-menu" aria-label="打开侧栏" @click="sidebarOpen = true">
          <Menu :size="21" />
        </button>
        <div class="breadcrumb">
          <span>视频空间</span>
          <span>/</span>
          <strong>{{ selectedVideo?.title || '开始使用 ReaderGo' }}</strong>
        </div>
        <div class="topbar-actions">
          <span v-if="previewMode" class="preview-badge">预览模式</span>
          <button class="ghost-button" :disabled="!selectedVideo" @click="clearContext">
            <RotateCcw :size="16" />
            清空上下文
          </button>
          <button class="icon-button topbar-help" aria-label="帮助"><CircleHelp :size="19" /></button>
        </div>
      </header>

      <div v-if="!selectedVideo" class="empty-workspace">
        <span class="empty-workspace__visual">
          <span><Play :size="26" fill="currentColor" /></span>
        </span>
        <span class="eyebrow">YOUR VIDEO KNOWLEDGE BASE</span>
        <h1>从一段视频开始</h1>
        <p>粘贴网址或上传本地文件，ReaderGo 会整理内容、生成摘要，并让你围绕视频自由提问。</p>
        <button class="primary-button" @click="openUpload"><Plus :size="18" /> 添加第一段视频</button>
      </div>

      <template v-else>
        <section class="insight-panel">
          <div class="insight-primary">
            <div class="video-title-row">
              <span class="video-title-icon"><FileVideo :size="21" /></span>
              <div>
                <span class="eyebrow">CURRENT VIDEO</span>
                <h1>{{ selectedVideo.title || '未命名视频' }}</h1>
              </div>
              <button class="icon-button"><MoreHorizontal :size="19" /></button>
            </div>

            <div class="status-card" :class="`status-card--${statusMeta.tone}`">
              <div class="status-card__top">
                <span class="status-illustration">
                  <Check v-if="isReady" :size="21" />
                  <Pause v-else-if="selectedVideo.status === 'paused'" :size="21" />
                  <LoaderCircle v-else-if="selectedVideo.status !== 'failed'" :size="21" class="spin" />
                  <X v-else :size="21" />
                </span>
                <div class="status-card__copy">
                  <strong>{{ statusMeta.label }}</strong>
                  <p>{{ statusMeta.detail }}</p>
                </div>
                <div class="status-card__actions">
                  <span class="status-percent">{{ statusMeta.percent }}%</span>
                  <button
                    v-if="canPauseSelected || selectedVideo.status === 'paused'"
                    class="status-control"
                    :disabled="statusActionBusy || (selectedVideo.status === 'paused' && !pauseSettled)"
                    @click="toggleProcessing"
                  >
                    <LoaderCircle v-if="statusActionBusy" :size="13" class="spin" />
                    <Play v-else-if="selectedVideo.status === 'paused'" :size="13" fill="currentColor" />
                    <Pause v-else :size="13" />
                    {{ selectedVideo.status === 'paused' ? (pauseSettled ? '继续处理' : '正在暂停') : '暂停处理' }}
                  </button>
                </div>
              </div>
              <div class="progress-track"><span :style="{ width: `${statusMeta.percent}%` }"></span></div>
              <div class="process-steps">
                <span v-for="step in processSteps" :key="step.key" :class="{ 'is-done': step.done }">
                  <Check v-if="step.done" :size="12" /> {{ step.label }}
                </span>
              </div>
            </div>
          </div>

          <div class="insight-secondary">
            <div class="metric-row">
              <div class="metric">
                <span><Clock3 :size="16" /> 处理时长</span>
                <strong>{{ formatDuration(selectedVideo.processing_duration_ms) }}</strong>
              </div>
              <div class="metric">
                <span><BookOpenText :size="16" /> 索引片段</span>
                <strong>{{ indexSegmentDisplay }}</strong>
              </div>
            </div>
            <article class="summary-card">
              <header>
                <span><Sparkles :size="15" /> AI 摘要</span>
                <div v-if="summary" class="summary-actions">
                  <button type="button" title="预览 Markdown 摘要" @click="summaryOpen = true"><Eye :size="13" /> 预览</button>
                  <button type="button" title="下载 Markdown 摘要" @click="downloadSummary"><Download :size="13" /> 下载</button>
                </div>
              </header>
              <p v-if="summary && !isReady" class="summary-early-note">摘要已生成，视频索引仍在处理中</p>
              <button v-if="summary" type="button" class="summary-preview-trigger" @click="summaryOpen = true">
                {{ summaryExcerpt }}
              </button>
              <div v-else class="summary-placeholder">
                <Hourglass :size="18" />
                <span>
                  <strong>摘要正在准备中</strong>
                  <small>摘要生成后即可查看，无需等待索引完成</small>
                </span>
              </div>
            </article>
          </div>
        </section>

        <section class="chat-panel">
          <header class="chat-header">
            <div>
              <span class="chat-header__icon"><MessageCircleMore :size="18" /></span>
              <span>
                <strong>与视频对话</strong>
                <small>{{ isReady ? '基于视频内容回答' : '处理期间仍可记录问题' }}</small>
              </span>
            </div>
            <span class="context-indicator"><span></span> 上下文已连接</span>
          </header>

          <div ref="chatScroll" class="messages">
            <div v-if="loadingMessages" class="messages-loading">
              <LoaderCircle :size="20" class="spin" /> 正在载入对话
            </div>
            <div v-else-if="!messages.length" class="chat-empty">
              <span><Sparkles :size="22" /></span>
              <h2>{{ isReady ? '从视频里发现更多' : '先记下你想问的问题' }}</h2>
              <p>{{ isReady ? '试着询问核心观点、具体细节，或让 ReaderGo 整理一份行动清单。' : '视频处理完成后，ReaderGo 会结合内容回答。' }}</p>
              <div class="suggestions">
                <button @click="draft = '用三点总结这段视频的核心内容'">总结核心观点</button>
                <button @click="draft = '视频中有哪些值得行动的建议？'">提取行动建议</button>
                <button @click="draft = '列出视频中最值得回看的三个片段'">寻找关键片段</button>
              </div>
            </div>
            <template v-else>
              <article v-for="message in messages" :key="message.id" class="message" :class="`message--${message.role}`">
                <span class="message-avatar">
                  <Sparkles v-if="message.role === 'assistant'" :size="16" />
                  <span v-else>{{ userInitial }}</span>
                </span>
                <div>
                  <header>
                    <strong>{{ message.role === 'assistant' ? 'ReaderGo' : '你' }}</strong>
                    <small v-if="message.role === 'assistant'">{{ messageSourceLabel(message) }}</small>
                  </header>
                  <div v-if="message.role === 'assistant' && message.metadata?.notice" class="message-notice">
                    <Info :size="12" />
                    <span>{{ message.metadata.notice }}</span>
                    <button
                      v-if="message.metadata.action"
                      type="button"
                      @click="handleMessageAction(message.metadata.action)"
                    >
                      {{ message.metadata.action.label }}
                    </button>
                    <button
                      v-if="message.metadata?.mode === 'summary' && message.metadata?.vision_requested && !message.metadata?.vision_used"
                      type="button"
                      @click="visionEnabled = false"
                    >
                      关闭画面理解
                    </button>
                  </div>
                  <p>{{ message.content }}</p>
                  <details v-if="message.role === 'assistant' && message.metadata?.vision_trace?.length" class="agent-trace">
                    <summary>
                      <span><Eye :size="13" /> 画面检查记录（{{ message.metadata.vision_trace.length }} 步）</span>
                      <ChevronDown :size="14" />
                    </summary>
                    <ol>
                      <li v-for="trace in message.metadata.vision_trace" :key="trace.id" :class="`agent-trace__step--${trace.status}`">
                        <span class="agent-trace__marker">
                          <LoaderCircle v-if="trace.status === 'running'" :size="12" class="spin" />
                          <Check v-else-if="trace.status === 'completed'" :size="12" />
                          <X v-else :size="12" />
                        </span>
                        <div>
                          <strong>{{ traceActionLabel(trace.action) }}</strong>
                          <p>{{ trace.summary }}</p>
                          <small v-if="trace.reason">动作说明：{{ trace.reason }}</small>
                          <small v-if="trace.arguments">参数：{{ traceArgumentsLabel(trace.arguments) }}</small>
                          <small v-if="trace.new_frame_timestamps_ms?.length">新增画面：{{ trace.new_frame_timestamps_ms.length }} 帧</small>
                          <small v-if="trace.action !== 'FINAL'">剩余预算：可细化 {{ trace.remaining_tool_calls }} 次，最多 {{ trace.remaining_frame_budget }} 帧</small>
                        </div>
                      </li>
                    </ol>
                    <p class="agent-trace__note">操作记录用于说明系统检查了哪些画面；画面结论仍请结合原片判断。</p>
                  </details>
                  <div v-if="message.citations?.length" class="citations">
                    <button v-for="citation in message.citations" :key="`${citation.segment_id}-${citation.start_ms}`" :title="`视频片段 ${citation.label}`">
                      <Play :size="11" fill="currentColor" /> {{ citation.label }} · {{ citation.source_type === 'vision_frame' ? '查看画面' : '相关片段' }}
                    </button>
                  </div>
                </div>
              </article>
              <article v-if="sending && !hasStreamingAssistant" class="message message--assistant">
                <span class="message-avatar"><Sparkles :size="16" /></span>
                <div>
                  <div class="typing"><i></i><i></i><i></i></div>
                  <small v-if="visionProgress" class="vision-progress">{{ visionProgress }}</small>
                  <div v-if="visionTrace.length" class="agent-trace agent-trace--live">
                    <strong class="agent-trace__live-title"><Eye :size="13" /> 正在检查画面</strong>
                    <ol>
                      <li v-for="trace in visionTrace" :key="trace.id" :class="`agent-trace__step--${trace.status}`">
                        <span class="agent-trace__marker">
                          <LoaderCircle v-if="trace.status === 'running'" :size="12" class="spin" />
                          <Check v-else-if="trace.status === 'completed'" :size="12" />
                          <X v-else :size="12" />
                        </span>
                        <div>
                          <strong>{{ traceActionLabel(trace.action) }}</strong>
                          <p>{{ trace.summary }}</p>
                          <small v-if="trace.reason">动作说明：{{ trace.reason }}</small>
                          <small v-if="trace.arguments">参数：{{ traceArgumentsLabel(trace.arguments) }}</small>
                          <small v-if="trace.action !== 'FINAL'">剩余预算：可细化 {{ trace.remaining_tool_calls }} 次，最多 {{ trace.remaining_frame_budget }} 帧</small>
                        </div>
                      </li>
                    </ol>
                  </div>
                </div>
              </article>
            </template>
          </div>

          <footer class="composer-wrap">
            <div class="vision-controls" :class="{ 'vision-controls--active': visionEnabled }">
              <label class="vision-toggle">
                <input v-model="visionEnabled" type="checkbox" :disabled="!canUseVision || sending" />
                <span aria-hidden="true"></span>
                <strong>画面理解</strong>
              </label>
              <small v-if="!canUseVision">{{ visionAvailable ? '视频处理完成后可用' : '画面理解服务未启用' }}</small>
              <template v-if="visionEnabled">
                <div class="vision-range-inputs">
                  <label>开始 <input v-model.trim="visionStart" inputmode="decimal" placeholder="01:20" :disabled="sending" /></label>
                  <span>至</span>
                  <label>结束 <input v-model.trim="visionEnd" inputmode="decimal" placeholder="01:30" :disabled="sending" /></label>
                </div>
                <p><Info :size="12" /> 画面理解已开启。请输入不超过 30 秒的时间区间，例如 01:20 到 01:30。系统首先每 5 秒查看一帧，并可能在该区间内继续细看。</p>
              </template>
            </div>
            <div class="composer" :class="{ 'composer--active': draft }">
              <textarea
                v-model="draft"
                rows="1"
                maxlength="4000"
                :placeholder="isReady ? '询问关于这段视频的任何问题…' : '先记下一个问题…'"
                @keydown="handleComposerKeydown"
              ></textarea>
              <div class="composer-footer">
                <span><Info :size="13" /> 回答可能存在误差，请结合原片判断</span>
                <button class="send-button" :disabled="!draft.trim() || sending" aria-label="发送" @click="sendMessage">
                  <LoaderCircle v-if="sending" :size="17" class="spin" />
                  <ArrowUp v-else :size="18" />
                </button>
              </div>
            </div>
            <p>Enter 发送 · Shift + Enter 换行</p>
          </footer>
        </section>
      </template>
    </section>

    <Teleport to="body">
      <div v-if="summaryOpen" class="modal-layer" @mousedown.self="summaryOpen = false">
        <section class="summary-modal" role="dialog" aria-modal="true" aria-labelledby="summary-title">
          <header class="modal-header">
            <div>
              <span class="modal-icon"><BookOpenText :size="21" /></span>
              <span>
                <h2 id="summary-title">视频章节摘要</h2>
                <p>{{ selectedVideo?.title || '未命名视频' }} · Markdown 文档</p>
              </span>
            </div>
            <button class="icon-button" aria-label="关闭摘要预览" @click="summaryOpen = false"><X :size="20" /></button>
          </header>
          <div class="markdown-preview">
            <template v-for="(block, index) in summaryBlocks" :key="`${block.type}-${index}`">
              <component :is="`h${block.level}`" v-if="block.type === 'heading'">{{ block.text }}</component>
              <p v-else-if="block.type === 'paragraph'">{{ block.text }}</p>
              <ul v-else-if="block.type === 'list'">
                <li v-for="item in block.items" :key="item">{{ item }}</li>
              </ul>
            </template>
          </div>
          <footer class="summary-modal__footer">
            <span>内容由 AI 基于视频证据生成，请结合原片判断。</span>
            <div>
              <button class="secondary-button" @click="summaryOpen = false">关闭</button>
              <button class="primary-button" @click="downloadSummary"><Download :size="16" /> 下载 .md</button>
            </div>
          </footer>
        </section>
      </div>

      <div v-if="uploadOpen" class="modal-layer" @mousedown.self="closeUpload">
        <section class="upload-modal" role="dialog" aria-modal="true" aria-labelledby="upload-title">
          <header class="modal-header">
            <div>
              <span class="modal-icon"><UploadCloud :size="21" /></span>
              <span>
                <h2 id="upload-title">添加视频</h2>
                <p>选择一种方式，ReaderGo 会在后台完成理解。</p>
              </span>
            </div>
            <button class="icon-button" :disabled="uploadBusy" aria-label="关闭" @click="closeUpload"><X :size="20" /></button>
          </header>

          <div class="upload-body">
            <label class="upload-label" :class="{ 'upload-label--muted': sourceDisabled }">
              <span><Link2 :size="15" /> 通过网址添加</span>
              <small>支持公开视频直链及 Bilibili 完整单视频页面，不支持短链</small>
            </label>
            <div class="url-input" :class="{ 'url-input--disabled': sourceDisabled }">
              <Link2 :size="18" />
              <input
                v-model="uploadUrl"
                type="url"
                :disabled="sourceDisabled || uploadBusy"
                placeholder="粘贴 Bilibili 或视频文件网址"
                @focus="chooseUrl"
                @input="uploadSource = 'url'"
              />
              <button v-if="uploadUrl && !uploadBusy" aria-label="清空网址" @click="uploadUrl = ''"><X :size="16" /></button>
            </div>

            <div class="source-divider"><span>或者，从本地上传</span></div>

            <div v-if="pendingUpload" class="resume-notice">
              <RotateCcw :size="17" />
              <span>
                <strong>存在未完成上传：{{ pendingUpload.filename }}</strong>
                <small>请重新选择同一文件，ReaderGo 将校验文件并只上传 MinIO 中缺失的分片。</small>
              </span>
            </div>

            <input ref="fileInput" class="visually-hidden" type="file" accept="video/*" @change="onFileChange" />
            <button
              type="button"
              class="dropzone"
              :class="{ 'dropzone--active': dragActive, 'dropzone--disabled': fileDisabled, 'dropzone--selected': selectedFile }"
              :disabled="fileDisabled || uploadBusy"
              @click="chooseFile"
              @dragenter.prevent="dragActive = true"
              @dragover.prevent="dragActive = true"
              @dragleave.prevent="dragActive = false"
              @drop.prevent="onDrop"
            >
              <template v-if="selectedFile">
                <span class="file-selected-icon"><FileVideo :size="25" /></span>
                <span class="file-copy">
                  <strong>{{ selectedFile.name }}</strong>
                  <small>{{ (selectedFile.size / 1024 / 1024).toFixed(1) }} MB · 已准备上传</small>
                </span>
                <span class="remove-file" @click.stop="removeFile"><Trash2 :size="17" /></span>
              </template>
              <template v-else>
                <span class="dropzone-icon"><UploadCloud :size="27" /></span>
                <strong>拖入视频文件，或点击选择</strong>
                <small>支持 MP4、MOV、WebM 等常见格式</small>
              </template>
            </button>

            <div v-if="uploadBusy || pendingUpload" class="upload-progress">
              <div>
                <span>{{ uploadHashing ? '正在校验视频内容' : uploadCanPause ? '正在上传视频分片' : pendingUpload ? '上传已暂停，可继续恢复' : '正在创建处理任务' }}</span>
                <strong>{{ uploadProgress }}%</strong>
              </div>
              <div class="progress-track"><span :style="{ width: `${Math.max(uploadProgress, 8)}%` }"></span></div>
            </div>
            <p v-if="uploadError" class="form-error upload-error">{{ uploadError }}</p>
          </div>

          <footer class="modal-footer">
            <span>
              <Zap :size="14" />
              {{ pendingUpload ? '关闭或刷新后，重新选择同一文件即可继续' : '提交后可以关闭页面，处理会继续进行' }}
            </span>
            <div>
              <button class="secondary-button" :disabled="uploadCancelBusy" @click="pendingUpload ? cancelPendingUpload() : uploadHashing ? cancelHashCalculation() : closeUpload()">
                {{ uploadCancelBusy ? '正在取消' : pendingUpload ? '取消上传' : uploadHashing ? '取消校验' : '取消' }}
              </button>
              <button v-if="uploadCanPause" class="primary-button" @click="pauseLocalUpload">
                <Pause :size="17" />
                暂停上传
              </button>
              <button
                v-else
                class="primary-button"
                :disabled="uploadBusy || uploadCancelBusy || (!uploadUrl.trim() && !selectedFile)"
                @click="submitUpload"
              >
                <LoaderCircle v-if="uploadBusy" :size="17" class="spin" />
                <RotateCcw v-else-if="pendingUpload" :size="17" />
                <UploadCloud v-else :size="17" />
                {{ uploadBusy ? '正在提交' : pendingUpload ? '继续上传' : '提交视频' }}
              </button>
            </div>
          </footer>
        </section>
      </div>

      <Transition name="toast">
        <div v-if="toast" class="toast" :class="`toast--${toast.type}`">
          <Check v-if="toast.type === 'success'" :size="16" />
          <Info v-else :size="16" />
          {{ toast.message }}
        </div>
      </Transition>
    </Teleport>
  </main>
</template>
