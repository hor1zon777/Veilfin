// 网页模式传输层：按需加载（本地客户端模式不需要这些加密库）。
import { ClientHandshake, b64decode, fingerprint } from '../crypto/proto.js'
import * as idb from './idb.js'
import { RpcError } from './transport.js'

// ---------------- 网页模式 ----------------

const TOKEN_KEY = 'vf.token'
const PIN_KEY = 'vf.serverKey'
const CHUNK = 1 << 20
const OVERHEAD = 28
const CIPHER_CHUNK = CHUNK + OVERHEAD
const MAX_STREAMS = 64

function aadFor(id, idx, n) {
  const idb = new TextEncoder().encode(id)
  const aad = new Uint8Array(4 + idb.length + 9)
  aad.set([0x56, 0x46, 0x4d, 0x32], 0) // "VFM2"
  aad.set(idb, 4)
  new DataView(aad.buffer).setBigUint64(4 + idb.length, BigInt(idx))
  aad[aad.length - 1] = idx === n - 1 ? 1 : 0
  return aad
}

function plainSizeOf(cs) {
  if (cs === 0) return 0
  const n = Math.ceil(cs / CIPHER_CHUNK)
  const size = cs - n * OVERHEAD
  return size > (n - 1) * CHUNK ? size : -1
}

async function decryptSegment(key, id, ct) {
  const size = plainSizeOf(ct.length)
  if (size < 0) throw new Error('分片长度不正确（数据被篡改或损坏）')
  const n = Math.ceil(size / CHUNK)
  const out = new Uint8Array(size)
  for (let i = 0; i < n; i++) {
    const len = Math.min(CHUNK, size - i * CHUNK)
    const c = ct.subarray(i * CIPHER_CHUNK, i * CIPHER_CHUNK + len + OVERHEAD)
    let pt
    try {
      pt = await crypto.subtle.decrypt(
        { name: 'AES-GCM', iv: c.subarray(0, 12), additionalData: aadFor(id, i, n), tagLength: 128 },
        key,
        c.subarray(12),
      )
    } catch {
      throw new Error('分片校验失败（数据被篡改或损坏）')
    }
    out.set(new Uint8Array(pt), i * CHUNK)
  }
  return out.buffer
}

export class WebTransport {
  mode = 'web'

  constructor(cfg) {
    this.apiBase = (cfg.apiBase || '').replace(/\/$/, '')
    this.pubB64 = cfg.pubkey
    this.pub = b64decode(cfg.pubkey)
    this.fp = fingerprint(this.pub)
    this.sess = null
    this.offset = 0
    this.hsPromise = null
    this.authed = false
    this.blobTok = null
    this.registered = new Set()
    this.streams = new Map()
    this.warning = ''
    this.seq = 0
  }

  async init() {
    // 首次访问时记住服务端公钥，之后如果变化就拒绝连接（防止被替换）
    const pinned = localStorage.getItem(PIN_KEY)
    const cur = btoa(String.fromCharCode(...this.pub))
    if (pinned && pinned !== cur) {
      throw new RpcError(
        'key_changed',
        '服务端公钥与上次不同！可能遭到中间人攻击，或服务端重装后密钥变了。确认安全后，可在浏览器中清除本站数据再访问。',
      )
    }
    if (!pinned) {
      localStorage.setItem(PIN_KEY, cur)
      this.warning = '首次访问，已记住服务端公钥指纹 ' + this.fp + '，请与服务端启动日志核对。'
    }
    if (!('serviceWorker' in navigator) || !crypto?.subtle) {
      throw new RpcError('unsupported', '当前浏览器不支持 Service Worker 或 WebCrypto（需要 HTTPS，且不能是无痕模式的 Firefox），无法解密播放。')
    }
    await idb.put('meta', 'cfg', { apiBase: this.apiBase })
    navigator.serviceWorker.addEventListener('message', async (ev) => {
      if (ev.data?.type !== 'need-token') return
      try {
        const token = await this.blobToken(ev.data.force)
        ev.ports[0]?.postMessage({ token })
      } catch {
        ev.ports[0]?.postMessage({ token: null })
      }
    })
    const reg = await navigator.serviceWorker.register('/sw.js', { scope: '/', updateViaCache: 'none' })
    await reg.update().catch(() => {})
    const waitController = (ms, worker) =>
      new Promise((resolve) => {
        const done = () => {
          clearTimeout(t)
          navigator.serviceWorker.removeEventListener('controllerchange', done)
          worker?.removeEventListener('statechange', onState)
          resolve()
        }
        const onState = () => {
          if (worker.state === 'redundant') done()
        }
        const t = setTimeout(done, ms)
        navigator.serviceWorker.addEventListener('controllerchange', done, { once: true })
        worker?.addEventListener('statechange', onState)
      })
    // 若有新版 SW 正在安装或等待激活（服务端升级后），等它接管页面，避免停留在旧版 SW
    const pending = reg.installing || reg.waiting
    if (pending) await waitController(4000, pending)
    await navigator.serviceWorker.ready
    if (!navigator.serviceWorker.controller) {
      // 第一次安装或已有激活 SW 但尚未接管当前页面：请求接管并等待
      reg.active?.postMessage({ type: 'claim' })
      await waitController(4000)
    }
  }

  async handshake() {
    const hs = new ClientHandshake()
    const res = await fetch(this.apiBase + '/h', { method: 'POST', body: hs.hello })
    if (!res.ok) throw new RpcError('network', '握手失败：HTTP ' + res.status)
    const sess = hs.finish(new Uint8Array(await res.arrayBuffer()), this.pub)
    this.sess = sess
    this.offset = sess.serverTime - Date.now()
    this.authed = false
    this.blobTok = null
    const tok = localStorage.getItem(TOKEN_KEY)
    if (tok) {
      try {
        await this.send(sess, 'auth.resume', { token: tok })
        this.authed = true
      } catch (e) {
        if (e instanceof RpcError && e.code !== 'network') localStorage.removeItem(TOKEN_KEY)
        else throw e
      }
    }
    return sess
  }

  async session(force) {
    if (!force && this.sess && Date.now() - this.sess.created < 23 * 3600 * 1000) return this.sess
    if (!this.hsPromise) {
      this.hsPromise = this.handshake().finally(() => (this.hsPromise = null))
    }
    return this.hsPromise
  }

  envelope(sess, m, p) {
    const rid = `${Date.now().toString(36)}-${++this.seq}-${Math.random().toString(36).slice(2)}`
    const req = new TextEncoder().encode(JSON.stringify({ ts: Date.now() + this.offset, rid, m, p: p ?? {} }))
    return { rid, body: sess.seal(req) }
  }

  async send(sess, m, p) {
    const { rid, body } = this.envelope(sess, m, p)
    let res
    try {
      res = await fetch(this.apiBase + '/x', { method: 'POST', body })
    } catch {
      throw new RpcError('network', '无法连接服务端')
    }
    if (res.status === 401) throw new RpcError('session_gone', '会话已失效')
    if (!res.ok) throw new RpcError('network', '服务端返回 HTTP ' + res.status)
    let out
    try {
      out = JSON.parse(new TextDecoder().decode(sess.open(new Uint8Array(await res.arrayBuffer()))))
    } catch {
      throw new RpcError('tampered', '响应解密失败，数据可能被篡改')
    }
    if (out.rid !== rid) throw new RpcError('tampered', '响应与请求不匹配')
    if (!out.ok) throw new RpcError(out.code || 'error', out.err || '未知错误')
    return out.data
  }

  async call(m, p) {
    if (m === 'auth.logout') p = { token: localStorage.getItem(TOKEN_KEY) || '' }
    let sess = await this.session()
    let data
    try {
      data = await this.send(sess, m, p)
    } catch (e) {
      if (e.code !== 'session_gone') throw this.after(m, e)
      sess = await this.session(true)
      data = await this.send(sess, m, p)
    }
    if (m === 'auth.login' || m === 'setup.create' || m === 'auth.changePassword') {
      localStorage.setItem(TOKEN_KEY, data.token)
      this.authed = true
      data = data.me
    }
    this.after(m)
    return this.rewrite(data)
  }

  after(m, err) {
    // 改 PIN / 改密码 / 解锁失败（比如密码输错）时服务端什么都没变，不能清掉对象密钥，否则页面上的图片全部 404；
    // 退出和锁定则无论成败都要清，用户的意图就是让内容不再可见
    if (err && m !== 'auth.logout' && m !== 'auth.lock') return err
    if (m === 'auth.logout') {
      localStorage.removeItem(TOKEN_KEY)
      this.authed = false
    }
    if (m === 'auth.logout' || m === 'auth.lock' || m === 'auth.setPin' || m === 'auth.changePassword') {
      // 锁定 / 退出：丢弃所有对象密钥，并让 Service Worker 清空已解密的数据块
      this.registered.clear()
      this.streams.clear()
      idb.clear('blobs')
      navigator.serviceWorker.controller?.postMessage({ type: 'clear-cache' })
    }
    if (m === 'auth.unlock' || m === 'auth.lock' || m === 'auth.setPin' || m === 'auth.logout' || m === 'auth.changePassword') {
      this.blobTok = null
      idb.put('meta', 'token', null)
    }
    return err
  }

  beacon(m, p) {
    if (!this.sess) return
    try {
      const { body } = this.envelope(this.sess, m, p)
      fetch(this.apiBase + '/x', { method: 'POST', body, keepalive: true })
    } catch {}
  }

  async blobToken(force) {
    if (!force && this.blobTok && this.blobTok.exp * 1000 - Date.now() > 5 * 60 * 1000) return this.blobTok.token
    const sess = await this.session()
    const t = await this.send(sess, 'auth.blobToken')
    this.blobTok = t
    await idb.put('meta', 'token', t)
    return t.token
  }

  // rewrite 把对象引用替换成 /b/{id}、转码流引用替换成 /t/{会话ID}/index.m3u8，
  // 并把解密密钥（不可导出的 CryptoKey）存入 IndexedDB 供 Service Worker 使用。
  async rewrite(data) {
    const refs = []
    const streams = []
    const walk = (x) => {
      if (Array.isArray(x)) {
        for (let i = 0; i < x.length; i++) x[i] = walk(x[i])
        return x
      }
      if (x && typeof x === 'object') {
        if (typeof x.$b === 'string') {
          if (!this.registered.has(x.$b)) refs.push(x)
          return '/b/' + encodeURIComponent(x.$b)
        }
        if (typeof x.$s === 'string') {
          streams.push(x)
          return '/t/' + encodeURIComponent(x.$s) + '/index.m3u8'
        }
        for (const k of Object.keys(x)) x[k] = walk(x[k])
      }
      return x
    }
    data = walk(data)
    if (refs.length) {
      const entries = await Promise.all(
        refs.map(async (r) => [r.$b, { key: await crypto.subtle.importKey('raw', b64decode(r.k), 'AES-GCM', false, ['decrypt']), size: r.s, mime: r.t }]),
      )
      await idb.putMany('blobs', entries)
      for (const [id] of entries) this.registered.add(id)
    }
    if (streams.length) {
      // 转码流同时保存在内存和 IndexedDB：hls.js 在页面内直接读播放列表并解密分片（不依赖 Service Worker，
      // 避免旧版 SW 缓存或 SW 未接管导致 manifestLoadError）；IndexedDB 保留给原生 HLS 兜底。
      const entries = await Promise.all(
        streams.map(async (r) => [
          r.$s,
          { key: await crypto.subtle.importKey('raw', b64decode(r.k), 'AES-GCM', false, ['decrypt']), playlist: r.p },
        ]),
      )
      for (const [sid, info] of entries) {
        this.streams.delete(sid)
        this.streams.set(sid, info)
        while (this.streams.size > MAX_STREAMS) this.streams.delete(this.streams.keys().next().value)
      }
      await idb.putMany(
        'blobs',
        entries.map(([sid, info]) => ['t:' + sid, info]),
      )
    }
    if ((refs.length || streams.length) && !this.blobTok) await this.blobToken().catch(() => {})
    return data
  }

  async streamInfo(sid) {
    let info = this.streams.get(sid)
    if (!info) {
      info = await idb.get('blobs', 't:' + sid).catch(() => null)
      if (info) {
        this.streams.set(sid, info)
        while (this.streams.size > MAX_STREAMS) this.streams.delete(this.streams.keys().next().value)
      }
    }
    return info || null
  }

  async loadStream(rawUrl, signal) {
    const u = new URL(rawUrl, location.href)
    const m = /^\/t\/([^/]+)\/([^/]+)$/.exec(u.pathname)
    if (!m) return { ok: false, code: 404, text: '转码地址无效', url: u.href }
    const sid = decodeURIComponent(m[1])
    const name = m[2]
    const info = await this.streamInfo(sid)
    if (!info) return { ok: false, code: 404, text: '未知转码会话，请刷新页面', url: u.href }
    if (name === 'index.m3u8') {
      return { ok: true, code: 200, data: info.playlist, url: u.href }
    }
    if (!/^(init|0|[1-9][0-9]*)$/.test(name)) {
      return { ok: false, code: 404, text: '分片名非法', url: u.href }
    }
    let res = null
    let force = false
    for (let attempt = 0; attempt < 2 && !res; attempt++) {
      const token = await this.blobToken(force)
      if (signal?.aborted) throw new DOMException('aborted', 'AbortError')
      const r = await fetch(this.apiBase + '/s/' + encodeURIComponent(sid) + '/' + name, {
        headers: { 'X-VF-T': token },
        cache: 'no-store',
        signal,
      })
      if (r.status === 403) {
        r.body?.cancel().catch(() => {})
        force = true // 令牌失效或解锁状态变化，刷新后重试
        continue
      }
      res = r
    }
    if (!res) return { ok: false, code: 403, text: '无权访问', url: u.href }
    if (!res.ok) return { ok: false, code: res.status, text: '上游返回 HTTP ' + res.status, url: u.href }
    const ct = new Uint8Array(await res.arrayBuffer())
    const data = await decryptSegment(info.key, sid + '/' + name, ct)
    return { ok: true, code: 200, data, url: u.href }
  }

  // hlsLoader 返回供 hls.js 使用的加载器：在页面内直接提供播放列表并拉取 / 解密分片，不经过 Service Worker。
  hlsLoader(FallbackLoader) {
    const tr = this
    return class StreamLoader {
      constructor(hlsConfig) {
        this.hlsConfig = hlsConfig
        this.stats = {
          aborted: false,
          loaded: 0,
          retry: 0,
          total: 0,
          chunkCount: 0,
          bwEstimate: 0,
          loading: { start: 0, first: 0, end: 0 },
          parsing: { start: 0, end: 0 },
          buffering: { start: 0, first: 0, end: 0 },
        }
        this.context = null
        this.config = null
        this.callbacks = null
        this.ctrl = null
        this.reqTimer = null
        this.retryTimer = null
        this.fallback = null
      }
      load(context, config, callbacks) {
        const u = new URL(context.url, location.href)
        if (!u.pathname.startsWith('/t/') && FallbackLoader) {
          this.fallback = new FallbackLoader(this.hlsConfig)
          this.stats = this.fallback.stats
          this.fallback.load(context, config, callbacks)
          return
        }
        this.context = context
        this.config = config
        this.callbacks = callbacks
        this.stats.loading.start = performance.now()
        this.run()
      }
      run() {
        if (!this.config || !this.context || !this.callbacks) return
        const stats = this.stats
        stats.loading.first = 0
        stats.loaded = 0
        stats.aborted = false
        const ctrl = (this.ctrl = new AbortController())
        const policy = this.config.loadPolicy || {}
        const timeoutMs = policy.maxTimeToFirstByteMs || policy.maxLoadTimeMs || this.config.timeout || 60000
        clearTimeout(this.reqTimer)
        this.reqTimer = setTimeout(() => this.onTimeout(), timeoutMs)
        tr.loadStream(this.context.url, ctrl.signal)
          .then((r) => {
            if (this.ctrl !== ctrl || stats.aborted || !this.callbacks) return
            clearTimeout(this.reqTimer)
            if (r.ok) {
              const now = Math.max(performance.now(), stats.loading.start)
              stats.loading.first = now
              stats.loading.end = now
              const len = typeof r.data === 'string' ? r.data.length : r.data.byteLength
              stats.loaded = stats.total = len
              stats.bwEstimate = (len * 8000) / Math.max(now - stats.loading.start, 1)
              const cb = this.callbacks
              cb.onProgress?.(stats, this.context, r.data, null)
              cb.onSuccess({ url: r.url, data: r.data, code: r.code }, stats, this.context, null)
              return
            }
            if (this.tryRetry(policy.errorRetry, r.code)) return
            this.callbacks.onError({ code: r.code, text: r.text }, this.context, null, stats)
          })
          .catch((e) => {
            if (this.ctrl !== ctrl || stats.aborted || e?.name === 'AbortError' || !this.callbacks) return
            clearTimeout(this.reqTimer)
            if (this.tryRetry(policy.errorRetry, 0)) return
            this.callbacks.onError({ code: 0, text: e?.message || String(e) }, this.context, null, stats)
          })
      }
      onTimeout() {
        if (!this.callbacks || this.stats?.aborted) return
        const policy = this.config?.loadPolicy || {}
        if (this.tryRetry(policy.timeoutRetry, 0)) return
        this.abortInternal()
        this.callbacks.onTimeout(this.stats, this.context, null)
      }
      tryRetry(retryCfg, code) {
        if (!retryCfg || this.stats.retry >= (retryCfg.maxNumRetry || 0)) return false
        if (code && code >= 400 && code < 500 && code !== 408 && code !== 429) return false
        const delay = Math.min((retryCfg.retryDelayMs || 1000) * Math.pow(2, this.stats.retry), retryCfg.maxRetryDelayMs || 8000)
        this.stats.retry++
        this.abortInternal()
        this.retryTimer = setTimeout(() => this.run(), delay)
        return true
      }
      abortInternal() {
        clearTimeout(this.reqTimer)
        clearTimeout(this.retryTimer)
        if (this.ctrl) {
          const c = this.ctrl
          this.ctrl = null
          c.abort()
        }
      }
      abort() {
        if (this.fallback) return this.fallback.abort()
        if (this.stats) this.stats.aborted = true
        this.abortInternal()
        this.callbacks?.onAbort?.(this.stats, this.context, null)
      }
      destroy() {
        if (this.fallback) {
          this.fallback.destroy()
          this.fallback = null
        }
        this.callbacks = this.context = this.config = null
        this.abortInternal()
        this.stats = null
      }
      getCacheAge() {
        return this.fallback ? this.fallback.getCacheAge() : null
      }
      getResponseHeader(name) {
        return this.fallback ? this.fallback.getResponseHeader(name) : null
      }
    }
  }

  async status() {
    const st = {
      mode: 'web',
      server: location.origin + (this.apiBase ? ` → ${this.apiBase}` : ''),
      fingerprint: this.fp,
      publicKey: this.pubB64,
      serverUrl: this.apiBase || location.origin,
      suite: 'X25519+ML-KEM-768 / Ed25519 / XChaCha20-Poly1305 ⊂ AES-256-GCM',
      warning: this.warning,
      connected: false,
      authed: this.authed,
    }
    try {
      const s = await this.session()
      st.connected = true
      st.authed = this.authed
      st.sessionAge = Math.round((Date.now() - s.created) / 1000)
      st.sendEpoch = s.send.epoch
    } catch (e) {
      st.error = e.message
    }
    return st
  }
}
