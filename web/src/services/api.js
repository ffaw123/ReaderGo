const API_BASE = (import.meta.env.VITE_API_BASE_URL || '').replace(/\/$/, '')
const maxPartUploadRetries = 3
const initialPartRetryDelayMs = 400

function getSession() {
  try {
    return JSON.parse(localStorage.getItem('readergo_session') || 'null')
  } catch {
    return null
  }
}

async function request(path, options = {}) {
  const session = getSession()
  const headers = new Headers(options.headers || {})
  if (!(options.body instanceof FormData) && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }
  if (session?.access_token) headers.set('Authorization', `Bearer ${session.access_token}`)

  const response = await fetch(`${API_BASE}${path}`, { ...options, headers })
  const payload = response.status === 204 ? null : await response.json().catch(() => null)

  if (!response.ok) {
    if (response.status === 401 && !path.startsWith('/api/auth/')) {
      localStorage.removeItem('readergo_session')
      window.location.assign('/login')
    }
    throw new Error(payload?.error || `请求失败（${response.status}）`)
  }
  return payload
}

async function streamRequest(path, options, onDelta) {
  const session = getSession()
  const headers = new Headers(options.headers || {})
  headers.set('Content-Type', 'application/json')
  headers.set('Accept', 'text/event-stream')
  if (session?.access_token) headers.set('Authorization', `Bearer ${session.access_token}`)

  const response = await fetch(`${API_BASE}${path}`, { ...options, headers })
  if (!response.ok) {
    const payload = await response.json().catch(() => null)
    if (response.status === 401 && !path.startsWith('/api/auth/')) {
      localStorage.removeItem('readergo_session')
      window.location.assign('/login')
    }
    throw new Error(payload?.error || `请求失败（${response.status}）`)
  }
  if (!response.body) throw new Error('浏览器无法读取回答流')

  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  let result = null

  const consumeEvent = (block) => {
    let event = 'message'
    const data = []
    block.split(/\r?\n/).forEach((line) => {
      if (line.startsWith('event:')) event = line.slice(6).trim()
      if (line.startsWith('data:')) data.push(line.slice(5).trimStart())
    })
    if (!data.length) return
    const payload = JSON.parse(data.join('\n'))
    if (event === 'delta') onDelta?.(payload.content || '')
    if (event === 'done') result = payload
    if (event === 'error') throw new Error(payload.error || '回答生成失败，请重试')
  }

  const consumeBuffer = (finished = false) => {
    // SSE 事件以空行分隔；兼容服务端或代理使用 LF/CRLF 两种换行。
    while (true) {
      const lfIndex = buffer.indexOf('\n\n')
      const crlfIndex = buffer.indexOf('\r\n\r\n')
      const indexes = [lfIndex, crlfIndex].filter((index) => index >= 0)
      if (!indexes.length) break
      const index = Math.min(...indexes)
      const separatorLength = index === crlfIndex ? 4 : 2
      consumeEvent(buffer.slice(0, index))
      buffer = buffer.slice(index + separatorLength)
    }
    if (finished && buffer.trim()) {
      consumeEvent(buffer)
      buffer = ''
    }
  }

  let finished = false
  try {
    while (true) {
      const { value, done } = await reader.read()
      if (done) break
      buffer += decoder.decode(value, { stream: true })
      consumeBuffer()
    }
    buffer += decoder.decode()
    consumeBuffer(true)
    finished = true
  } finally {
    if (!finished) await reader.cancel().catch(() => {})
    reader.releaseLock()
  }
  if (!result) throw new Error('回答流意外中断，请重试')
  return result
}

export const api = {
  login: (body) => request('/api/auth/login', { method: 'POST', body: JSON.stringify(body) }),
  register: (body) => request('/api/auth/register', { method: 'POST', body: JSON.stringify(body) }),
  createVideo: (body) => request('/api/videos', { method: 'POST', body: JSON.stringify(body) }),
  getVideo: (id) => request(`/api/videos/${id}`),
  getVideoStatus: (id, details = false) => request(`/api/videos/${id}/status${details ? '?details=true' : ''}`),
  pauseVideo: (id) => request(`/api/videos/${id}/pause`, { method: 'POST' }),
  resumeVideo: (id) => request(`/api/videos/${id}/resume`, { method: 'POST' }),
  getSummary: (id) => request(`/api/videos/${id}/summary`),
  createConversation: (videoId, title) =>
    request(`/api/videos/${videoId}/conversations`, {
      method: 'POST',
      body: JSON.stringify({ title }),
    }),
  listMessages: (conversationId) => request(`/api/conversations/${conversationId}/messages`),
  sendMessage: (conversationId, content, clientMessageId, onDelta) =>
    streamRequest(
      `/api/conversations/${conversationId}/messages`,
      {
        method: 'POST',
        body: JSON.stringify({ content, client_message_id: clientMessageId }),
      },
      onDelta,
    ),
  createUpload: (file, title) =>
    request('/api/uploads', {
      method: 'POST',
      body: JSON.stringify({
        filename: file.name,
        content_type: file.type || 'application/octet-stream',
        file_size: file.size,
        title,
      }),
    }),
  getUploadParts: (uploadId, partNumbers) =>
    request(`/api/uploads/${uploadId}/parts`, {
      method: 'POST',
      body: JSON.stringify({ part_numbers: partNumbers }),
    }),
  completeUpload: (uploadId, parts) =>
    request(`/api/uploads/${uploadId}/complete`, {
      method: 'POST',
      body: JSON.stringify({ parts }),
    }),
}

function waitForRetry(delayMs) {
  return new Promise((resolve) => window.setTimeout(resolve, delayMs))
}

function isRetryablePartUploadStatus(status) {
  return status === 408 || status === 429 || status >= 500
}

async function uploadPartWithRetry(url, part, partNumber) {
  let lastError

  // 首次请求之外，最多额外重试 3 次；每次等待时间按指数增长。
  for (let retry = 0; retry <= maxPartUploadRetries; retry += 1) {
    try {
      const response = await fetch(url, { method: 'PUT', body: part })
      if (!response.ok) {
        const error = new Error(`第 ${partNumber} 个分片上传失败（HTTP ${response.status}）`)
        error.retryable = isRetryablePartUploadStatus(response.status)
        throw error
      }

      const etag = response.headers.get('etag')
      if (!etag) {
        const error = new Error('对象存储未返回 ETag；请检查 MinIO CORS 是否暴露 ETag 响应头')
        error.retryable = false
        throw error
      }
      return etag
    } catch (error) {
      lastError = error
      if (retry === maxPartUploadRetries || error.retryable === false) break
      await waitForRetry(initialPartRetryDelayMs * 2 ** retry)
    }
  }

  throw new Error(`第 ${partNumber} 个分片上传失败，已重试 ${maxPartUploadRetries} 次：${lastError.message}`)
}

export async function uploadFileParts(file, session, onProgress) {
  const knownParts = new Map(session.parts.map((part) => [part.part_number, part.url]))
  const missingNumbers = []
  for (let number = 1; number <= session.part_count; number += 1) {
    if (!knownParts.has(number)) missingNumbers.push(number)
  }

  for (let index = 0; index < missingNumbers.length; index += 16) {
    const batch = missingNumbers.slice(index, index + 16)
    const result = await api.getUploadParts(session.upload_id, batch)
    result.parts.forEach((part) => knownParts.set(part.part_number, part.url))
  }

  let uploadedBytes = 0
  const completedParts = new Array(session.part_count)
  const queue = Array.from({ length: session.part_count }, (_, index) => index + 1)
  const workers = Array.from({ length: Math.min(4, queue.length) }, async () => {
    while (queue.length) {
      const partNumber = queue.shift()
      const start = (partNumber - 1) * session.part_size
      const end = Math.min(start + session.part_size, file.size)
      const part = file.slice(start, end)
      const etag = await uploadPartWithRetry(knownParts.get(partNumber), part, partNumber)
      completedParts[partNumber - 1] = { part_number: partNumber, etag }
      uploadedBytes += part.size
      onProgress(Math.round((uploadedBytes / file.size) * 100))
    }
  })
  await Promise.all(workers)
  return completedParts
}
