// 播放队列（随机播放等）：保存在 sessionStorage，刷新播放页后仍然有效，关闭标签页即清除。
const KEY = 'vf.queue'

export function setQueue(ids, label) {
  sessionStorage.setItem(KEY, JSON.stringify({ ids, label }))
}

export function getQueue() {
  try {
    const q = JSON.parse(sessionStorage.getItem(KEY) || 'null')
    return q && Array.isArray(q.ids) ? q : null
  } catch {
    return null
  }
}

// 当前条目在队列中的位置和后续条目
export function queueAt(id) {
  const q = getQueue()
  if (!q) return null
  const i = q.ids.indexOf(Number(id))
  if (i < 0) return null
  return { label: q.label, index: i, total: q.ids.length, rest: q.ids.slice(i + 1, i + 51) }
}
