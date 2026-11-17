export function fmtTime(sec) {
  if (!isFinite(sec) || sec < 0) sec = 0
  sec = Math.floor(sec)
  const h = Math.floor(sec / 3600)
  const m = Math.floor((sec % 3600) / 60)
  const s = sec % 60
  const mm = String(m).padStart(h ? 2 : 1, '0')
  return (h ? h + ':' : '') + mm + ':' + String(s).padStart(2, '0')
}

export function fmtDuration(sec) {
  if (!sec) return ''
  const h = Math.floor(sec / 3600)
  const m = Math.round((sec % 3600) / 60)
  if (h && m) return `${h} 小时 ${m} 分钟`
  if (h) return `${h} 小时`
  return `${Math.max(m, 1)} 分钟`
}

export function fmtSize(n) {
  if (!n) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  while (n >= 1024 && i < u.length - 1) {
    n /= 1024
    i++
  }
  return n.toFixed(i ? 1 : 0) + ' ' + u[i]
}

export function fmtDate(ts) {
  if (!ts) return ''
  const d = new Date(ts * 1000)
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
}

export function fmtDateTime(ts) {
  if (!ts) return ''
  const d = new Date(ts * 1000)
  return `${fmtDate(ts)} ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
}

export function resolutionLabel(w, h) {
  if (!w || !h) return ''
  const p = Math.min(w, h)
  if (w >= 3800 || p >= 2000) return '4K'
  if (w >= 2500 || p >= 1400) return '1440p'
  if (w >= 1900 || p >= 1000) return '1080p'
  if (w >= 1200 || p >= 700) return '720p'
  if (p >= 480) return '480p'
  return 'SD'
}

export function debounce(fn, ms) {
  let t
  return (...a) => {
    clearTimeout(t)
    t = setTimeout(() => fn(...a), ms)
  }
}

export const kindNames = { home: '自制视频', movie: '电影', jav: '日本影片', other: '其他' }
export const scraperNames = { none: '不刮削', tmdb: 'TMDB（电影）', metatube: 'MetaTube（番号）' }

// 生成稳定的占位渐变色（没有封面时使用）
export function placeholderStyle(seed) {
  let h = 0
  for (const ch of String(seed)) h = (h * 31 + ch.charCodeAt(0)) >>> 0
  const a = h % 360
  return { background: `linear-gradient(160deg, hsl(${a} 30% 26%), hsl(${(a + 40) % 360} 35% 13%))` }
}

export function randomId() {
  const b = new Uint8Array(12)
  crypto.getRandomValues(b)
  return Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('')
}

export function readFileBase64(file) {
  return new Promise((resolve, reject) => {
    const r = new FileReader()
    r.onload = () => resolve(String(r.result).split(',')[1])
    r.onerror = () => reject(r.error)
    r.readAsDataURL(file)
  })
}

// 相对时间：刚刚 / 5 分钟前 / 3 小时前 / 昨天 21:30 / 4 天前 / 2026-11-01
export function fmtAgo(ts) {
  if (!ts) return ''
  const diff = Date.now() / 1000 - ts
  if (diff < 60) return '刚刚'
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`
  const d = new Date(ts * 1000)
  const today = new Date()
  today.setHours(0, 0, 0, 0)
  const hm = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
  if (d >= today) return diff < 6 * 3600 ? `${Math.floor(diff / 3600)} 小时前` : `今天 ${hm}`
  if (d >= today - 86400000) return `昨天 ${hm}`
  const days = Math.floor((today - d) / 86400000) + 1
  if (days < 7) return `${days} 天前`
  return fmtDate(ts)
}

// 剩余时长（继续观看用）
export function fmtRemain(item) {
  const left = (item.duration || 0) - (item.position || 0)
  if (!item.duration || left <= 0) return ''
  return left < 60 ? '剩余不到 1 分钟' : `剩余 ${fmtDuration(left)}`
}

export function randomSeed() {
  return crypto.getRandomValues(new Uint32Array(1))[0]
}

// 复制文本到剪贴板。通过局域网 IP（非 HTTPS）访问时没有 Clipboard API，退回到 execCommand。
export async function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text)
      return
    } catch {}
  }
  const ta = document.createElement('textarea')
  ta.value = text
  ta.setAttribute('readonly', '')
  ta.className = 'offscreen'
  document.body.appendChild(ta)
  ta.select()
  let ok = false
  try {
    ok = document.execCommand('copy')
  } finally {
    ta.remove()
  }
  if (!ok) throw new Error('复制失败，请手动选中复制')
}

export function b64ToBytes(s) {
  const bin = atob(s)
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  return out
}

// 让浏览器把 Blob 保存成文件
export function saveBlob(blob, name) {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = name
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 60_000)
}
