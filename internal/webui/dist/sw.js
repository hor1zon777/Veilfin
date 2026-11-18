// Service Worker（网页模式）：拦截 /b/{id}，从服务端（或 CDN）拉取密文块，用 WebCrypto 解密后交给 <video>/<img>。
// 支持 Range 请求和流式输出，拖动进度条时只拉取需要的块：
//   - 边收边解密：上游每到一整块就解密交出，拖动后不必等整组下载完；
//   - 顺序播放时保持两个上游请求交替进行，提前拉取后面约 16 MiB，掩盖往返延迟；
//   - 播放请求被浏览器取消（拖动、切换视频）后，没人再需要的上游请求立即中止，不和新位置抢带宽。
// 密钥和访问令牌由页面写入 IndexedDB（结构与 src/api/idb.js 一致）。
'use strict'

const CHUNK = 1 << 20
const OVERHEAD = 28
const CIPHER_CHUNK = CHUNK + OVERHEAD
const GROUP = 4 // 每次向上游拉取的块数（按 4 块对齐，便于合并重复请求，也利于 CDN 分段缓存）
const AHEAD = 16 // 顺序播放时预取到当前位置之后的块数（约 16 MiB）
const MAX_INFLIGHT = 2 // 每个播放请求同时进行的上游请求数
const CACHE_LIMIT = 96 // 解密块缓存（约 96 MiB）

self.addEventListener('install', () => self.skipWaiting())
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()))

// 页面锁定受限内容或退出登录时清空解密缓存，并中止进行中的上游请求
self.addEventListener('message', (e) => {
  if (e.data?.type === 'clear-cache') {
    cache.clear()
    for (const job of new Set(pending.values())) job.cancel()
  }
})

self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url)
  if (url.origin === self.location.origin && url.pathname.startsWith('/b/')) {
    e.respondWith(serve(e.request, decodeURIComponent(url.pathname.slice(3))))
  }
})

// ---- IndexedDB ----
let dbp = null
function openDB() {
  if (!dbp) {
    dbp = new Promise((resolve, reject) => {
      const req = indexedDB.open('veilfin', 1) // 与 src/api/idb.js 一致
      req.onupgradeneeded = () => {
        const db = req.result
        if (!db.objectStoreNames.contains('blobs')) db.createObjectStore('blobs')
        if (!db.objectStoreNames.contains('meta')) db.createObjectStore('meta')
      }
      req.onsuccess = () => resolve(req.result)
      req.onerror = () => reject(req.error)
    })
  }
  return dbp
}
async function idbGet(store, key) {
  const db = await openDB()
  return new Promise((resolve, reject) => {
    const req = db.transaction(store).objectStore(store).get(key)
    req.onsuccess = () => resolve(req.result)
    req.onerror = () => reject(req.error)
  })
}

// ---- 解密块缓存（LRU） ----
const cache = new Map()
function cacheGet(k) {
  const v = cache.get(k)
  if (v) {
    cache.delete(k)
    cache.set(k, v)
  }
  return v
}
function cachePut(k, v) {
  cache.set(k, v)
  while (cache.size > CACHE_LIMIT) cache.delete(cache.keys().next().value)
}

// ---- 访问令牌 ----
async function askClientForToken(force) {
  const clients = await self.clients.matchAll({ type: 'window' })
  for (const c of clients) {
    const ch = new MessageChannel()
    const reply = new Promise((resolve) => {
      ch.port1.onmessage = (ev) => resolve(ev.data)
      setTimeout(() => resolve(null), 15000)
    })
    c.postMessage({ type: 'need-token', force }, [ch.port2])
    const r = await reply
    if (r && r.token) return r.token
  }
  return null
}

async function getToken(force) {
  if (!force) {
    const t = await idbGet('meta', 'token')
    if (t && t.exp * 1000 - Date.now() > 60 * 1000) return t.token
  }
  return askClientForToken(force)
}

// ---- 拉取与解密 ----
function aadFor(id, idx, n) {
  const idb = new TextEncoder().encode(id)
  const aad = new Uint8Array(4 + idb.length + 9)
  aad.set([0x56, 0x46, 0x4d, 0x32], 0) // "VFM2"
  aad.set(idb, 4)
  new DataView(aad.buffer).setBigUint64(4 + idb.length, BigInt(idx))
  aad[aad.length - 1] = idx === n - 1 ? 1 : 0
  return aad
}

function numChunks(info) {
  return Math.ceil(info.size / CHUNK)
}
function plainLen(info, i) {
  return Math.min(CHUNK, info.size - i * CHUNK)
}

// 进行中的上游请求，按块登记：`${id}:${块序号}` → Job。同一块不会被重复拉取。
const pending = new Map()

function deferred() {
  let resolve, reject
  const promise = new Promise((res, rej) => ((resolve = res), (reject = rej)))
  promise.catch(() => {}) // 没人等待时被拒绝，也不报“未处理的 Promise 拒绝”
  return { promise, resolve, reject }
}

// Job 是一次上游 Range 请求，覆盖第 first 到 last 块，边收边解密。
// holders 是需要它的播放请求，全部放弃后立即中止。
class Job {
  constructor(id, info, first, last, base) {
    Object.assign(this, { id, info, first, last, base })
    this.ctrl = new AbortController()
    this.holders = new Set()
    this.done = false
    this.waits = new Map()
    for (let i = first; i <= last; i++) {
      this.waits.set(i, deferred())
      pending.set(id + ':' + i, this)
    }
    this.run()
  }

  unregister(i) {
    const k = this.id + ':' + i
    if (pending.get(k) === this) pending.delete(k)
  }

  cancel() {
    if (this.done) return
    for (let i = this.first; i <= this.last; i++) this.unregister(i) // 立即注销，之后需要的人重新发起请求
    this.ctrl.abort()
  }

  async run() {
    const { id, info, first, last } = this
    const n = numChunks(info)
    const start = first * CIPHER_CHUNK
    const end = last * CIPHER_CHUNK + plainLen(info, last) + OVERHEAD - 1
    let i = first
    try {
      let res = null
      let force = false
      for (let attempt = 0; attempt < 2 && !res; attempt++) {
        const token = await getToken(force)
        if (!token) throw new Error('无法获取访问令牌，请重新登录')
        if (this.ctrl.signal.aborted) throw new DOMException('aborted', 'AbortError')
        const r = await fetch(this.base + '/m/' + encodeURIComponent(id), {
          headers: { Range: `bytes=${start}-${end}`, 'X-VF-T': token },
          cache: 'no-store',
          signal: this.ctrl.signal,
        })
        if (r.status === 403) {
          r.body?.cancel().catch(() => {})
          force = true // 令牌失效或解锁状态变化，刷新后重试
          continue
        }
        res = r
      }
      if (!res) throw new Error('无权访问（受限内容可能需要先输入 PIN）')
      if (res.status !== 206 && res.status !== 200) throw new Error('上游返回 HTTP ' + res.status)
      const cr = res.headers.get('Content-Range')
      if (res.status === 206 && cr && !cr.startsWith(`bytes ${start}-`)) throw new Error('上游返回的 Content-Range 不匹配')
      // 上游忽略 Range 返回了整个对象：跳过前面不需要的部分
      let skip = res.status === 200 ? start : 0
      const total = end - start + 1
      const buf = new Uint8Array(total)
      let filled = 0
      let pos = 0
      const reader = res.body.getReader()
      try {
        while (i <= last) {
          const { done, value } = await reader.read()
          if (done) break
          let v = value
          if (skip) {
            if (v.length <= skip) {
              skip -= v.length
              continue
            }
            v = v.subarray(skip)
            skip = 0
          }
          const take = Math.min(v.length, total - filled)
          buf.set(v.subarray(0, take), filled)
          filled += take
          // 每凑齐一整块就解密交出
          while (i <= last) {
            const clen = plainLen(info, i) + OVERHEAD
            if (filled < pos + clen) break
            const ct = buf.subarray(pos, pos + clen)
            let pt
            try {
              pt = new Uint8Array(
                await crypto.subtle.decrypt(
                  { name: 'AES-GCM', iv: ct.subarray(0, 12), additionalData: aadFor(id, i, n), tagLength: 128 },
                  info.key,
                  ct.subarray(12),
                ),
              )
            } catch {
              throw new Error(`第 ${i} 块校验失败（数据被篡改或损坏）`)
            }
            cachePut(id + ':' + i, pt)
            this.unregister(i)
            this.waits.get(i).resolve(pt)
            pos += clen
            i++
          }
        }
      } finally {
        reader.cancel().catch(() => {})
      }
      if (i <= last) throw new Error('上游数据长度不符')
    } catch (e) {
      const err = e?.name === 'AbortError' ? new Error('请求已取消') : e
      for (; i <= last; i++) this.waits.get(i).reject(err)
    } finally {
      this.done = true
      for (let k = first; k <= last; k++) this.unregister(k)
      // 让持有它的播放请求接着往后预取
      for (const s of this.holders) s.pump()
    }
  }
}

// Stream 是浏览器发来的一次 /b/ 请求（明文范围到 end 为止）。
class Stream {
  constructor(id, info, base, end) {
    Object.assign(this, { id, info, base })
    this.lastIdx = Math.floor(end / CHUNK)
    this.held = new Set()
    this.closed = false
    this.served = 0 // 已交出的块数
    this.pos = -1 // 最近交出的块序号
  }

  hold(job) {
    if (job.done || this.held.has(job)) return false
    job.holders.add(this)
    this.held.add(job)
    return true
  }

  release(job) {
    job.holders.delete(this)
    this.held.delete(job)
    if (!job.done && job.holders.size === 0) job.cancel()
  }

  close() {
    if (this.closed) return
    this.closed = true
    for (const job of [...this.held]) this.release(job)
  }

  // 从 idx 开始发起上游请求：到对齐的组尾为止，遇到已缓存或正在拉取的块就停下，不重复拉取
  start(idx) {
    const end = Math.min(Math.floor(idx / GROUP) * GROUP + GROUP - 1, this.lastIdx, numChunks(this.info) - 1)
    let last = idx
    while (last < end && !cache.has(this.id + ':' + (last + 1)) && !pending.has(this.id + ':' + (last + 1))) last++
    return new Job(this.id, this.info, idx, last, this.base)
  }

  async chunk(idx) {
    const k = this.id + ':' + idx
    for (let attempt = 0; ; attempt++) {
      const c = cacheGet(k)
      if (c) return c
      if (this.closed) throw new Error('请求已取消')
      const job = pending.get(k) || this.start(idx)
      this.hold(job)
      try {
        return await job.waits.get(idx).promise
      } catch (e) {
        // 别的播放请求放弃了这个上游请求，或网络中断：重试一次
        if (this.closed || attempt >= 1) throw e
      }
    }
  }

  // 交出第 idx 块后调用：释放已经越过的上游请求，并保持后面的预取
  advance(idx) {
    this.served++
    this.pos = idx
    for (const job of [...this.held]) if (job.last < idx) this.release(job)
    this.pump()
  }

  // 顺序读取（已交出两块以上）时，在当前位置之后 AHEAD 块内保持最多 MAX_INFLIGHT 个上游请求
  pump() {
    if (this.closed || this.served < 2) return
    let inflight = 0
    for (const job of this.held) if (!job.done) inflight++
    const limit = Math.min(this.pos + AHEAD, this.lastIdx)
    let j = this.pos + 1
    while (j <= limit && inflight < MAX_INFLIGHT) {
      const k = this.id + ':' + j
      if (cache.has(k)) {
        j++
        continue
      }
      const job = pending.get(k) || this.start(j)
      if (this.hold(job)) inflight++
      j = job.last + 1
    }
  }
}

function parseRange(h, size) {
  if (!h) return null
  const m = /^bytes=(\d*)-(\d*)$/.exec(h.trim())
  if (!m) return 'invalid'
  let start, end
  if (m[1] === '') {
    const suffix = parseInt(m[2], 10)
    if (!suffix) return 'invalid'
    start = Math.max(size - suffix, 0)
    end = size - 1
  } else {
    start = parseInt(m[1], 10)
    end = m[2] === '' ? size - 1 : Math.min(parseInt(m[2], 10), size - 1)
  }
  if (start > end || start >= size) return 'invalid'
  return [start, end]
}

async function serve(req, id) {
  const info = await idbGet('blobs', id)
  if (!info) return new Response('未知对象，请刷新页面', { status: 404 })
  const cfg = (await idbGet('meta', 'cfg')) || {}
  const base = cfg.apiBase || ''
  const size = info.size
  const range = parseRange(req.headers.get('Range'), size)
  if (range === 'invalid') return new Response(null, { status: 416, headers: { 'Content-Range': `bytes */${size}` } })
  const [start, end] = range || [0, size - 1]

  const st = new Stream(id, info, base, end)
  req.signal?.addEventListener('abort', () => st.close())

  // 先取第一块：出错时可以直接返回错误状态，而不是一个中途断掉的流
  let firstChunk
  try {
    firstChunk = await st.chunk(Math.floor(start / CHUNK))
  } catch (e) {
    st.close()
    return new Response(String(e.message || e), { status: 502 })
  }

  let pos = start
  const body = new ReadableStream({
    async pull(ctrl) {
      try {
        const idx = Math.floor(pos / CHUNK)
        const chunk = firstChunk || (await st.chunk(idx))
        firstChunk = null
        const from = pos - idx * CHUNK
        const to = Math.min(chunk.length, end - idx * CHUNK + 1)
        ctrl.enqueue(chunk.slice(from, to))
        pos = idx * CHUNK + to
        if (pos > end) {
          ctrl.close()
          st.close()
        } else {
          st.advance(idx)
        }
      } catch (e) {
        st.close()
        ctrl.error(e)
      }
    },
    // 浏览器放弃了这个请求（拖动、切换视频、离开页面）
    cancel() {
      st.close()
    },
  })
  const headers = {
    'Content-Type': info.mime,
    'Content-Length': String(end - start + 1),
    'Accept-Ranges': 'bytes',
    'Cache-Control': 'no-store',
  }
  if (range) {
    headers['Content-Range'] = `bytes ${start}-${end}/${size}`
    return new Response(body, { status: 206, headers })
  }
  return new Response(body, { status: 200, headers })
}
