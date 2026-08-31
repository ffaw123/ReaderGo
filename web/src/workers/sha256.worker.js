import { createSHA256 } from 'hash-wasm'

const hashChunkSize = 8 * 1024 * 1024

self.onmessage = async ({ data }) => {
  try {
    const file = data.file
    const hasher = await createSHA256()
    hasher.init()

    for (let offset = 0; offset < file.size; offset += hashChunkSize) {
      const end = Math.min(offset + hashChunkSize, file.size)
      const chunk = new Uint8Array(await file.slice(offset, end).arrayBuffer())
      // Worker 内按块更新哈希，主页面仅接收轻量进度消息。
      hasher.update(chunk)
      self.postMessage({ type: 'progress', progress: Math.round((end / file.size) * 100) })
    }

    self.postMessage({ type: 'complete', sha256: hasher.digest('hex') })
  } catch (error) {
    self.postMessage({ type: 'error', message: error?.message || '无法计算文件 SHA-256' })
  }
}
