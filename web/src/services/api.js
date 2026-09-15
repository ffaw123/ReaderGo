const API_BASE = (import.meta.env.VITE_API_BASE_URL || '').replace(/\/$/, '')
const maxPartUploadRetries = 3
const initialPartRetryDelayMs = 400
const fingerprintSampleSize = 64 * 1024

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
    const error = new Error(payload?.error || `请求失败（${response.status}）`)
    error.status = response.status
    error.code = payload?.code
    error.details = payload
    throw error
  }
  return payload
}

async function streamRequest(path, options, onDelta, onProgress) {
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
    if (event === 'progress') onProgress?.(payload)
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
    if (!finished) await reader.cancel().catch(() => { })
    reader.releaseLock()
  }
  if (!result) throw new Error('回答流意外中断，请重试')
  return result
}

export const api = {
  login: (body) => request('/api/auth/login', { method: 'POST', body: JSON.stringify(body) }),
  register: (body) => request('/api/auth/register', { method: 'POST', body: JSON.stringify(body) }),
  listVideos: () => request('/api/videos'),
  createVideo: (body) => request('/api/videos', { method: 'POST', body: JSON.stringify(body) }),
  getVideo: (id) => request(`/api/videos/${id}`),
  getVideoStatus: (id, details = false) => request(`/api/videos/${id}/status${details ? '?details=true' : ''}`),
  pauseVideo: (id) => request(`/api/videos/${id}/pause`, { method: 'POST' }),
  resumeVideo: (id) => request(`/api/videos/${id}/resume`, { method: 'POST' }),
  getSummary: (id) => request(`/api/videos/${id}/summary`),
  getCapabilities: () => request('/api/capabilities'),
  createConversation: (videoId, title) =>
    request(`/api/videos/${videoId}/conversations`, {
      method: 'POST',
      body: JSON.stringify({ title }),
    }),
  listMessages: (conversationId) => request(`/api/conversations/${conversationId}/messages`),
  sendMessage: (conversationId, content, clientMessageId, vision, onDelta, onProgress) =>
    streamRequest(
      `/api/conversations/${conversationId}/messages`,
      {
        method: 'POST',
        body: JSON.stringify({ content, client_message_id: clientMessageId, vision }),
      },
      onDelta,
      onProgress,
    ),
  createUpload: (file, title, identity, contentSHA256) =>
    request('/api/uploads', {
      method: 'POST',
      body: JSON.stringify({
        filename: file.name,
        content_type: file.type || 'application/octet-stream',
        file_size: file.size,
        file_last_modified_ms: identity.file_last_modified_ms,
        file_fingerprint: identity.file_fingerprint,
        content_sha256: contentSHA256,
        title,
      }),
    }),
  getUploadStatus: (uploadId, signal) => request(`/api/uploads/${uploadId}`, { signal }),
  getUploadParts: (uploadId, partNumbers, signal) =>
    request(`/api/uploads/${uploadId}/parts`, {
      method: 'POST',
      body: JSON.stringify({ part_numbers: partNumbers }),
      signal,
    }),
  completeUpload: (uploadId) => request(`/api/uploads/${uploadId}/complete`, { method: 'POST' }),
  cancelUpload: (uploadId) => request(`/api/uploads/${uploadId}`, { method: 'DELETE' }),
}

function abortError() {
  return new DOMException('上传已暂停', 'AbortError')
}

function throwIfAborted(signal) {
  if (signal?.aborted) throw abortError()
}

function waitForRetry(delayMs, signal) {
  return new Promise((resolve, reject) => {
    throwIfAborted(signal)
    const timer = window.setTimeout(() => {
      signal?.removeEventListener('abort', onAbort)
      resolve()
    }, delayMs)
    const onAbort = () => {
      window.clearTimeout(timer)
      reject(abortError())
    }
    signal?.addEventListener('abort', onAbort, { once: true })
  })
}

function isRetryablePartUploadStatus(status) {
  return status === 408 || status === 429 || status >= 500
}

async function uploadPartWithRetry(url, part, partNumber, signal) {
  let lastError

  // 首次请求之外，最多额外重试 3 次；每次等待时间按指数增长。
  for (let retry = 0; retry <= maxPartUploadRetries; retry += 1) {
    throwIfAborted(signal)
    try {
      const response = await fetch(url, { method: 'PUT', body: part, signal })
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
      if (signal?.aborted || error.name === 'AbortError') throw abortError()
      lastError = error
      if (retry === maxPartUploadRetries || error.retryable === false) break
      await waitForRetry(initialPartRetryDelayMs * 2 ** retry, signal)
    }
  }

  throw new Error(`第 ${partNumber} 个分片上传失败，已重试 ${maxPartUploadRetries} 次：${lastError.message}`)
}

export async function createFileIdentity(file) {
  if (!globalThis.crypto?.subtle) throw new Error('当前浏览器环境不支持文件指纹校验')
  const first = new Uint8Array(await file.slice(0, fingerprintSampleSize).arrayBuffer())
  const lastStart = Math.max(first.length, file.size - fingerprintSampleSize)
  const last = new Uint8Array(await file.slice(lastStart).arrayBuffer())
  const sizeBytes = new TextEncoder().encode(`readergo-v1:${file.size}:`)
  const sample = new Uint8Array(sizeBytes.length + first.length + last.length)
  sample.set(sizeBytes)
  sample.set(first, sizeBytes.length)
  sample.set(last, sizeBytes.length + first.length)
  const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', sample))
  return {
    filename: file.name,
    file_size: file.size,
    file_last_modified_ms: file.lastModified,
    file_fingerprint: Array.from(digest, (value) => value.toString(16).padStart(2, '0')).join(''),
  }
}

export function sameFileIdentity(left, right) {
  return Boolean(
    left &&
    right &&
    left.filename === right.filename &&
    left.file_size === right.file_size &&
    left.file_last_modified_ms === right.file_last_modified_ms &&
    left.file_fingerprint === right.file_fingerprint,
  )
}

// 完整内容哈希在独立 Worker 中分块计算，避免大文件占满主线程或一次性进入内存。
export function calculateContentSHA256(file, onProgress, signal) {
  return new Promise((resolve, reject) => {
    const worker = new Worker(new URL('../workers/sha256.worker.js', import.meta.url), { type: 'module' })
    let settled = false
    const finish = (callback) => {
      if (settled) return
      settled = true
      signal?.removeEventListener('abort', onAbort)
      worker.terminate()
      callback()
    }
    const onAbort = () => finish(() => reject(new DOMException('文件校验已取消', 'AbortError')))

    if (signal?.aborted) {
      onAbort()
      return
    }
    signal?.addEventListener('abort', onAbort, { once: true })
    worker.onmessage = ({ data }) => {
      if (data.type === 'progress') onProgress?.(data.progress)
      if (data.type === 'complete') finish(() => resolve(data.sha256))
      if (data.type === 'error') finish(() => reject(new Error(data.message || '无法计算文件 SHA-256')))
    }
    worker.onerror = () => finish(() => reject(new Error('文件校验线程意外退出')))
    worker.postMessage({ file })
  })
}

export async function uploadFileParts(file, session, onProgress, signal) {
  throwIfAborted(signal)
  if (session.object_completed) {
    onProgress(100)
    return
  }

  const completed = new Map((session.completed_parts || []).map((part) => [part.part_number, part]))
  let uploadedBytes = Array.from(completed.values()).reduce((total, part) => total + part.size, 0)
  onProgress(Math.round((uploadedBytes / file.size) * 100))

  // 恢复时只申请缺失分片的新 URL，并重新上传缺失部分
  const missingNumbers = []
  for (let number = 1; number <= session.part_count; number += 1) {
    if (!completed.has(number)) missingNumbers.push(number)
  }

  const initialURLs = new Map(
    (session.parts || [])
      .filter((part) => new Date(part.expires_at).getTime() > Date.now() + 30_000)
      .map((part) => [part.part_number, part.url]),
  )

  // 按16个一批获取预签名url，一批的内部4个分片并发上传到MinIO，上传4次。
  // 完成后再获取下一批url
  for (let index = 0; index < missingNumbers.length; index += 16) {
    throwIfAborted(signal)
    const batch = missingNumbers.slice(index, index + 16)
    const urls = new Map()
    const needURLs = []
    batch.forEach((partNumber) => {
      if (initialURLs.has(partNumber)) urls.set(partNumber, initialURLs.get(partNumber))
      else needURLs.push(partNumber)
    })
    if (needURLs.length) {
      const result = await api.getUploadParts(session.upload_id, needURLs, signal)
      result.parts.forEach((part) => urls.set(part.part_number, part.url))
    }

    const queue = [...batch]
    const workers = Array.from({ length: Math.min(4, queue.length) }, async () => {
      while (queue.length) {
        throwIfAborted(signal)
        const partNumber = queue.shift()
        const start = (partNumber - 1) * session.part_size
        const end = Math.min(start + session.part_size, file.size)
        const part = file.slice(start, end)
        const etag = await uploadPartWithRetry(urls.get(partNumber), part, partNumber, signal)
        // 当前页面内仅保存进度提示；恢复和最终完成仍会重新查询 MinIO。
        completed.set(partNumber, { part_number: partNumber, etag, size: part.size })
        uploadedBytes += part.size
        onProgress(Math.round((uploadedBytes / file.size) * 100))
      }
    })
    await Promise.all(workers)
  }
}
