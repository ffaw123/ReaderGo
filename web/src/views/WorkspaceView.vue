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
import { api, uploadFileParts } from '../services/api'
import { demoMessages, demoVideos } from '../lib/demo'

const router = useRouter()
const session = ref(null)
const previewMode = ref(sessionStorage.getItem('readergo_preview') === 'true')
const videos = ref([])
const selectedId = ref('')
const messages = ref([])
const summary = ref('')
const loadingVideos = ref(false)
const loadingMessages = ref(false)
const sending = ref(false)
const uploadOpen = ref(false)
const accountOpen = ref(false)
const sidebarOpen = ref(false)
const search = ref('')
const draft = ref('')
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
const pauseSettled = ref(true)
const statusActionBusy = ref(false)
let statusTimer
let toastTimer

const selectedVideo = computed(() => videos.value.find((video) => video.id === selectedId.value) || null)
const filteredVideos = computed(() => {
  const keyword = search.value.toLowerCase()
  return videos.value.filter((video) => video.title.toLowerCase().includes(keyword))
})
const userName = computed(() => session.value?.user?.display_name || session.value?.user?.email?.split('@')[0] || '探索者')
const userInitial = computed(() => userName.value.slice(0, 1).toUpperCase())
const isReady = computed(() => selectedVideo.value?.status === 'ready')
const canPauseSelected = computed(() => {
  const video = selectedVideo.value
  if (!video) return false
  if (video.status === 'processing') return true
  // URL prepare 属于后台流水线；本地 Multipart 尚未完成时不显示处理暂停。
  return video.status === 'uploading' && /^https?:\/\//i.test(video.source_url || '')
})
const statusMeta = computed(() => {
  const statuses = {
    ready: { label: '分析完成', detail: '所有内容已可检索', percent: 100, tone: 'success' },
    processing: { label: '正在处理视频', detail: '正在解析内容、建立索引和生成摘要', percent: 64, tone: 'working' },
    uploading: { label: '等待上传完成', detail: '文件分片正在传输', percent: 16, tone: 'working' },
    paused: {
      label: pauseSettled.value ? '处理已暂停' : '正在暂停处理',
      detail: pauseSettled.value ? '点击继续后将从未完成的阶段恢复' : '正在安全停止运行中的任务',
      percent: 64,
      tone: 'paused',
    },
    failed: {
      label: '处理失败',
      detail: selectedVideo.value?.error_message || '请检查视频源后重试',
      percent: 100,
      tone: 'danger',
    },
  }
  return statuses[selectedVideo.value?.status] || statuses.uploading
})
const sourceDisabled = computed(() => uploadSource.value === 'file' && selectedFile.value)
const fileDisabled = computed(() => uploadSource.value === 'url' && uploadUrl.value.trim())

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

function getCachedEntries() {
  try {
    return JSON.parse(localStorage.getItem(cacheKey()) || '[]')
  } catch {
    return []
  }
}

function rememberVideo(video, conversationId = '') {
  const entries = getCachedEntries().filter((item) => item.id !== video.id)
  entries.unshift({ id: video.id, title: video.title, conversationId })
  localStorage.setItem(cacheKey(), JSON.stringify(entries.slice(0, 50)))
}

function getConversationId(video) {
  return video.conversationId || getCachedEntries().find((item) => item.id === video.id)?.conversationId || ''
}

async function loadVideos() {
  if (previewMode.value) {
    videos.value = demoVideos.map((video) => ({ ...video }))
    selectedId.value = videos.value[0].id
    return
  }
  loadingVideos.value = true
  const entries = getCachedEntries()
  const loaded = await Promise.all(
    entries.map(async (entry) => {
      try {
        const video = await api.getVideo(entry.id)
        return { ...video, conversationId: entry.conversationId }
      } catch {
        return null
      }
    }),
  )
  videos.value = loaded.filter(Boolean)
  selectedId.value = videos.value[0]?.id || ''
  loadingVideos.value = false
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

async function refreshStatus() {
  if (previewMode.value || !selectedVideo.value || ['ready', 'failed'].includes(selectedVideo.value.status)) return
  try {
    const status = await api.getVideoStatus(selectedVideo.value.id, true)
    selectedVideo.value.status = status.status
    selectedVideo.value.error_message = status.error_message || ''
    pauseSettled.value = status.pause_settled !== false
    if (status.status === 'ready') await loadSelectedContent()
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
  draft.value = ''
  sending.value = true
  const optimistic = {
    id: `local-${Date.now()}`,
    role: 'user',
    content,
    created_at: new Date().toISOString(),
  }
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
          '这是预览模式下的示例回答。连接 ReaderGo 后端后，这里会结合视频转写、关键画面与时间轴生成答案，并返回可点击的原片引用。',
        citations: [{ time: '12:24', label: '相关片段' }],
        created_at: new Date().toISOString(),
      })
    } else {
      const conversationId = await ensureConversation(selectedVideo.value)
      const saved = await api.sendMessage(conversationId, content)
      messages.value = messages.value.map((message) => (message.id === optimistic.id ? saved : message))
      notify('问题已提交，回答生成接口接入后会显示在这里')
    }
  } catch (error) {
    messages.value = messages.value.filter((message) => message.id !== optimistic.id)
    draft.value = content
    notify(error.message, 'error')
  } finally {
    sending.value = false
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

function clearContext() {
  messages.value = []
  notify(previewMode.value ? '当前预览对话已清空' : '已清空当前页面上下文；服务端删除接口待接入')
}

function openUpload() {
  uploadOpen.value = true
  sidebarOpen.value = false
  uploadError.value = ''
}

function closeUpload() {
  if (uploadBusy.value) return
  uploadOpen.value = false
  uploadUrl.value = ''
  selectedFile.value = null
  uploadProgress.value = 0
  uploadError.value = ''
  uploadSource.value = 'url'
}

function chooseUrl() {
  if (selectedFile.value) return
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
  uploadSource.value = 'url'
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
      const uploadSession = await api.createUpload(file, title)
      const parts = await uploadFileParts(file, uploadSession, (progress) => (uploadProgress.value = progress))
      const completed = await api.completeUpload(uploadSession.upload_id, parts)
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
    uploadError.value = error.message
  } finally {
    uploadBusy.value = false
  }
}

function logout() {
  localStorage.removeItem('readergo_session')
  sessionStorage.removeItem('readergo_preview')
  router.push('/login')
}

function formatDuration(duration) {
  if (!duration) return '等待识别'
  const totalMinutes = Math.round(duration / 60000)
  return `${Math.floor(totalMinutes / 60)}:${String(totalMinutes % 60).padStart(2, '0')}`
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
    await loadSelectedContent()
    clearInterval(statusTimer)
    statusTimer = setInterval(refreshStatus, 5000)
    await refreshStatus()
    await nextTick()
    scrollChat()
  },
)

onMounted(async () => {
  parseSession()
  await loadVideos()
})

onBeforeUnmount(() => {
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
        <label class="sidebar-search">
          <Search :size="15" />
          <input v-model="search" placeholder="搜索视频" />
          <kbd>⌘ K</kbd>
        </label>

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
                <span class="is-done"><Check :size="12" /> 获取视频</span>
                <span :class="{ 'is-done': statusMeta.percent >= 45 }"><Check v-if="statusMeta.percent >= 45" :size="12" /> 转写内容</span>
                <span :class="{ 'is-done': statusMeta.percent >= 75 }"><Check v-if="statusMeta.percent >= 75" :size="12" /> 理解画面</span>
                <span :class="{ 'is-done': isReady }"><Check v-if="isReady" :size="12" /> 建立索引</span>
              </div>
            </div>
          </div>

          <div class="insight-secondary">
            <div class="metric-row">
              <div class="metric">
                <span><Clock3 :size="16" /> 时长</span>
                <strong>{{ formatDuration(selectedVideo.duration_ms) }}</strong>
              </div>
              <div class="metric">
                <span><BookOpenText :size="16" /> 内容片段</span>
                <strong>{{ isReady ? '128' : '—' }}</strong>
              </div>
              <div class="metric">
                <span><Zap :size="16" /> 可引用节点</span>
                <strong>{{ isReady ? '36' : '—' }}</strong>
              </div>
            </div>
            <article class="summary-card">
              <header>
                <span><Sparkles :size="15" /> AI 摘要</span>
                <small v-if="summary">自动生成</small>
              </header>
              <p v-if="summary">{{ summary }}</p>
              <div v-else class="summary-placeholder">
                <Hourglass :size="18" />
                <span>
                  <strong>摘要正在准备中</strong>
                  <small>处理完成后会在这里提炼核心内容</small>
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
                    <small>{{ message.role === 'assistant' ? '基于视频内容' : '刚刚' }}</small>
                  </header>
                  <p>{{ message.content }}</p>
                  <div v-if="message.citations?.length" class="citations">
                    <button v-for="citation in message.citations" :key="citation.time">
                      <Play :size="11" fill="currentColor" /> {{ citation.time }} · {{ citation.label }}
                    </button>
                  </div>
                </div>
              </article>
              <article v-if="sending && previewMode" class="message message--assistant">
                <span class="message-avatar"><Sparkles :size="16" /></span>
                <div class="typing"><i></i><i></i><i></i></div>
              </article>
            </template>
          </div>

          <footer class="composer-wrap">
            <div class="composer" :class="{ 'composer--active': draft }">
              <textarea
                v-model="draft"
                rows="1"
                maxlength="10000"
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
              <small>支持公开可访问的 HTTP / HTTPS 视频链接</small>
            </label>
            <div class="url-input" :class="{ 'url-input--disabled': sourceDisabled }">
              <Link2 :size="18" />
              <input
                v-model="uploadUrl"
                type="url"
                :disabled="sourceDisabled || uploadBusy"
                placeholder="粘贴 YouTube、Bilibili 或视频文件网址"
                @focus="chooseUrl"
                @input="uploadSource = 'url'"
              />
              <button v-if="uploadUrl && !uploadBusy" aria-label="清空网址" @click="uploadUrl = ''"><X :size="16" /></button>
            </div>

            <div class="source-divider"><span>或者，从本地上传</span></div>

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

            <div v-if="uploadBusy" class="upload-progress">
              <div><span>{{ selectedFile ? '正在上传视频分片' : '正在创建处理任务' }}</span><strong>{{ uploadProgress }}%</strong></div>
              <div class="progress-track"><span :style="{ width: `${Math.max(uploadProgress, 8)}%` }"></span></div>
            </div>
            <p v-if="uploadError" class="form-error upload-error">{{ uploadError }}</p>
          </div>

          <footer class="modal-footer">
            <span><Zap :size="14" /> 提交后可以关闭页面，处理会继续进行</span>
            <div>
              <button class="secondary-button" :disabled="uploadBusy" @click="closeUpload">取消</button>
              <button class="primary-button" :disabled="uploadBusy || (!uploadUrl.trim() && !selectedFile)" @click="submitUpload">
                <LoaderCircle v-if="uploadBusy" :size="17" class="spin" />
                <UploadCloud v-else :size="17" />
                {{ uploadBusy ? '正在提交' : '提交视频' }}
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
