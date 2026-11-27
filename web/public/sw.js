// Service Worker（网页模式）：拦截 /b/{id}，从服务端（或 CDN）拉取密文块，用 WebCrypto 解密后交给 <video>/<img>。
// 支持 Range 请求和流式输出，拖动进度条时只拉取需要的块：
//   - 边收边解密：上游每到一整块就解密交出，拖动后不必等整组下载完；
//   - 顺序播放时保持两个上游请求交替进行，提前拉取后面约 16 MiB，掩盖往返延迟；
//   - 播放请求被浏览器取消（拖动、切换视频）后，没人再需要的上游请求立即中止，不和新位置抢带宽；
//   - 同一视频只有最新的播放请求预取：拖动时（尤其 iOS 系统播放器连续跳转、旧请求不一定会被取消）
//     旧请求立即放掉还没用到的预取，跳转后的第一次上游请求只拉一块，开始顺序读取后再预取。
// 另外拦截 /t/{会话ID}/{index.m3u8|init|序号}：播放时转码的 HLS 播放列表和分片，整段拉取、解密后返回。
// 密钥和访问令牌由页面写入 IndexedDB（结构与 src/api/idb.js 一致）。
'use strict'

const CHUNK = 1 << 20
const OVERHEAD = 28
const CIPHER_CHUNK = CHUNK + OVERHEAD
const GROUP = 4 // 每次向上游拉取的块数（按 4 块对齐，便于合并重复请求，也利于 CDN 分段缓存）
const AHEAD = 16 // 顺序播放时预取到当前位置之后的块数（约 16 MiB）
const MAX_INFLIGHT = 2 // 每个播放请求同时进行的上游请求数
const FIRST = 1 // 新播放请求（跳转）第一次向上游拉取的块数
const CACHE_LIMIT = 96 // 解密块缓存（约 96 MiB）

self.addEventListener('install', () => self.skipWaiting())
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()))

// 页面锁定受限内容或退出登录时清空解密缓存，并中止进行中的上游请求
self.addEventListener('message', (e) => {
  if (e.data?.type === 'clear-cache') {
    cache.clear()
    tokenMemo = null
    for (const job of new Set(pending.values())) job.cancel()
  } else if (e.data?.type === 'claim') {
    e.waitUntil?.(self.clients.claim().catch(() => {}))
  }
})

self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url)
  if (url.origin === self.location.origin && url.pathname.startsWith('/b/')) {
    e.respondWith(serve(e.request, decodeURIComponent(url.pathname.slice(3))))
  } else if (url.origin === self.location.origin && url.pathname.startsWith('/t/')) {
    e.respondWith(serveStream(e.request, url.pathname))
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
      req.onsuccess = () => {
        const db = req.result
        db.onclose = () => (dbp = null)
        db.onversionchange = () => {
          db.close()
          dbp = null
        }
        resolve(db)
      }
      req.onerror = () => {
        dbp = null
        reject(req.error)
      }
    })
  }
  return dbp
}
async function idbGet(store, key) {
  for (let attempt = 0; attempt < 2; attempt++) {
    const db = await openDB()
    try {
      return await new Promise((resolve, reject) => {
        const req = db.transaction(store).objectStore(store).get(key)
        req.onsuccess = () => resolve(req.result)
        req.onerror = () => reject(req.error)
      })
    } catch (e) {
      dbp = null
      if (attempt === 1) throw e
    }
  }
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

// 读到的令牌在内存里留一份，每次上游请求不必再读 IndexedDB
let tokenMemo = null
const fresh = (t) => t && t.exp * 1000 - Date.now() > 60 * 1000
async function getToken(force) {
  if (!force) {
    if (fresh(tokenMemo)) return tokenMemo.token
    const t = await idbGet('meta', 'token')
    if (fresh(t)) {
      tokenMemo = t
      return t.token
    }
  }
  tokenMemo = null
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
      // 让持有它的播放请求接着往后预取（pump 内部只允许最新的播放请求预取）
      for (const s of this.holders) s.pump()
    }
  }
}

// 每个视频当前打开的播放请求，用来判断谁是最新的
const streams = new Map() // id → Set<Stream>

// Stream 是浏览器发来的一次 /b/ 请求（明文范围到 end 为止）。
class Stream {
  constructor(id, info, base, end) {
    Object.assign(this, { id, info, base })
    this.lastIdx = Math.floor(end / CHUNK)
    this.held = new Set()
    this.closed = false
    this.served = 0 // 已交出的块数
    this.pos = -1 // 最近交出的块序号
    this.want = null // 正在等待的上游请求（不是预取）
    this.claimed = false
    let set = streams.get(id)
    if (!set) streams.set(id, (set = new Set()))
    set.add(this)
  }

  // 新的播放请求（通常是拖动）认领了自己要的第一块之后，其他请求放掉还没用到的预取，把带宽让给新位置。
  // 先认领再让：要的块正好在别人的预取里时（例如前进 10 秒），直接接着用，不会被中止后重拉
  claim() {
    if (this.claimed) return
    this.claimed = true
    for (const s of streams.get(this.id) || []) if (s !== this) s.yieldPrefetch()
  }

  newest() {
    let last = null
    for (const s of streams.get(this.id) || []) last = s
    return last === this
  }

  yieldPrefetch() {
    for (const job of [...this.held]) if (job !== this.want) this.release(job)
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
    const set = streams.get(this.id)
    set?.delete(this)
    if (set && !set.size) streams.delete(this.id)
  }

  // 从 idx 开始发起上游请求：到对齐的组尾为止，遇到已缓存或正在拉取的块就停下，不重复拉取。
  // 还没交出过数据的新请求（跳转）只拉 FIRST 块：拖动中途的位置很快会被放弃，少浪费带宽
  start(idx) {
    let end = Math.min(Math.floor(idx / GROUP) * GROUP + GROUP - 1, this.lastIdx, numChunks(this.info) - 1)
    if (this.served === 0) end = Math.min(end, idx + FIRST - 1)
    let last = idx
    while (last < end && !cache.has(this.id + ':' + (last + 1)) && !pending.has(this.id + ':' + (last + 1))) last++
    return new Job(this.id, this.info, idx, last, this.base)
  }

  async chunk(idx) {
    const k = this.id + ':' + idx
    for (let attempt = 0; ; attempt++) {
      const c = cacheGet(k)
      if (c) {
        this.claim()
        return c
      }
      if (this.closed) throw new Error('请求已取消')
      const job = pending.get(k) || this.start(idx)
      this.hold(job)
      this.want = job
      this.claim()
      try {
        return await job.waits.get(idx).promise
      } catch (e) {
        // 别的播放请求放弃了这个上游请求，或网络中断：重试一次
        if (this.closed || attempt >= 1) throw e
      } finally {
        if (this.want === job) this.want = null
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

  // 浏览器开始读取（已交出数据）后，在当前位置之后 AHEAD 块内保持最多 MAX_INFLIGHT 个上游请求。
  // 只有这个视频最新的播放请求预取，旧请求只按需拉取
  pump() {
    if (this.closed || this.served < 1 || !this.newest()) return
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
  let info, cfg
  try {
    info = await idbGet('blobs', id)
    if (!info) return new Response('未知对象，请刷新页面', { status: 404 })
    cfg = (await idbGet('meta', 'cfg')) || {}
  } catch (e) {
    return new Response(String(e?.message || e), { status: 502 })
  }
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

// ---- 播放时转码（HLS） ----

// 由密文大小推出明文大小（每块固定开销 OVERHEAD 字节），不合法时返回 -1
function plainSizeOf(cs) {
  if (cs === 0) return 0
  const n = Math.ceil(cs / CIPHER_CHUNK)
  const size = cs - n * OVERHEAD
  return size > (n - 1) * CHUNK ? size : -1
}

async function serveStream(req, path) {
  const m = /^\/t\/([^/]+)\/([^/]+)$/.exec(path)
  if (!m) return new Response(null, { status: 404 })
  const sid = decodeURIComponent(m[1])
  const name = m[2]
  const noStore = { 'Cache-Control': 'no-store' }
  try {
    const info = await idbGet('blobs', 't:' + sid)
    if (!info) return new Response('未知转码会话，请刷新页面', { status: 404, headers: noStore })
    if (name === 'index.m3u8') {
      return new Response(info.playlist, { headers: { 'Content-Type': 'application/vnd.apple.mpegurl', ...noStore } })
    }
    if (!/^(init|0|[1-9][0-9]*)$/.test(name)) return new Response(null, { status: 404, headers: noStore })
    const cfg = (await idbGet('meta', 'cfg')) || {}
    const base = cfg.apiBase || ''
    let res = null
    let force = false
    for (let attempt = 0; attempt < 2 && !res; attempt++) {
      const token = await getToken(force)
      if (!token) return new Response('无法获取访问令牌，请重新登录', { status: 403 })
      const r = await fetch(base + '/s/' + encodeURIComponent(sid) + '/' + name, {
        headers: { 'X-VF-T': token },
        cache: 'no-store',
        signal: req.signal,
      })
      if (r.status === 403) {
        r.body?.cancel().catch(() => {})
        force = true // 令牌失效或解锁状态变化，刷新后重试
        continue
      }
      res = r
    }
    if (!res) return new Response('无权访问', { status: 403 })
    if (!res.ok) return new Response(null, { status: res.status, headers: noStore })
    const ct = new Uint8Array(await res.arrayBuffer())
    const size = plainSizeOf(ct.length)
    if (size < 0) throw new Error('分片长度不正确')
    const id = sid + '/' + name
    const n = Math.ceil(size / CHUNK)
    const out = new Uint8Array(size)
    for (let i = 0; i < n; i++) {
      const len = Math.min(CHUNK, size - i * CHUNK)
      const c = ct.subarray(i * CIPHER_CHUNK, i * CIPHER_CHUNK + len + OVERHEAD)
      let pt
      try {
        pt = await crypto.subtle.decrypt(
          { name: 'AES-GCM', iv: c.subarray(0, 12), additionalData: aadFor(id, i, n), tagLength: 128 },
          info.key,
          c.subarray(12),
        )
      } catch {
        throw new Error('分片校验失败（数据被篡改或损坏）')
      }
      out.set(new Uint8Array(pt), i * CHUNK)
    }
    return new Response(out, { headers: { 'Content-Type': 'video/mp4', 'Content-Length': String(size), ...noStore } })
  } catch (e) {
    if (e?.name === 'AbortError') return new Response(null, { status: 499 })
    return new Response(String(e.message || e), { status: 502 })
  }
}
