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
  sendMessage: (conversationId, content, clientMessageId) =>
    request(`/api/conversations/${conversationId}/messages`, {
      method: 'POST',
      body: JSON.stringify({ content, client_message_id: clientMessageId }),
    }),
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
