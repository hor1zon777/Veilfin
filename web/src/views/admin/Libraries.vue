<script setup>
import { ref, onMounted, onBeforeUnmount } from 'vue'
import { rpc, toast, toastError } from '../../store.js'
import { kindNames, scraperNames, fmtDateTime } from '../../util.js'
import Modal from '../../components/Modal.vue'
import LibraryForm from '../../components/LibraryForm.vue'
import Icon from '../../components/Icon.vue'
import ScanProgress from '../../components/ScanProgress.vue'

const libs = ref([])
const edit = ref(null)
const del = ref(null)
const busy = ref(false)

async function load() {
  try {
    libs.value = await rpc('admin.libs.list')
  } catch (e) {
    toastError(e)
  }
}

// ---- 扫描进度 ----
// progress：媒体库 ID → 服务端返回的进度。只显示进行中的，以及在本页面上看着它结束的（结束后保留几秒）
const progress = ref({})
const recent = ref({}) // 媒体库 ID → 结束时间（本地），用于“完成”状态停留一会儿再收起
const isActive = (p) => p && p.phase !== 'done' && p.phase !== 'failed'
let timer = 0
let alive = true

async function loadProgress() {
  clearTimeout(timer)
  try {
    const list = await rpc('admin.libs.progress')
    const next = {}
    for (const p of list) next[p.libraryId] = p
    let finished = false
    for (const [id, p] of Object.entries(next)) {
      if (!isActive(p) && isActive(progress.value[id])) {
        recent.value[id] = Date.now()
        finished = true
        const l = libs.value.find((x) => x.id === Number(id))
        if (p.phase === 'failed') toastError(new Error(`「${l?.name || '媒体库'}」扫描失败：${p.err}`))
      }
    }
    progress.value = next
    if (finished) load() // 刷新视频数和上次扫描时间
  } catch {}
  if (!alive) return
  // 有扫描进行中时快速刷新；空闲时慢一点，也能看到目录监听触发的扫描
  const busyNow = Object.values(progress.value).some(isActive)
  timer = setTimeout(loadProgress, busyNow ? 700 : 4000)
}

// 进行中的一直显示；完成的显示 6 秒，失败的一直显示到下次扫描
function shown(id) {
  const p = progress.value[id]
  if (!p) return null
  if (isActive(p) || p.phase === 'failed') return p
  const t = recent.value[id]
  return t && Date.now() - t < 6000 ? p : null
}
// 收起动画期间仍要渲染最后一次的内容，否则内容瞬间消失，没有东西可以收起
const lastShown = new Map()
function display(id) {
  const p = shown(id)
  if (p) lastShown.set(id, p)
  return p || lastShown.get(id)
}
// 让“完成”状态到时间后收起
let tick = 0
const now = ref(Date.now())
function startTick() {
  tick = setInterval(() => (now.value = Date.now()), 1000)
}

onMounted(() => {
  load()
  loadProgress()
  startTick()
})
onBeforeUnmount(() => {
  alive = false
  clearTimeout(timer)
  clearInterval(tick)
})

function openNew() {
  edit.value = { id: 0, name: '', kind: 'home', restricted: false, scraper: 'none', trickplay: false, paths: [''], sortOrder: libs.value.length }
}
function openEdit(l) {
  edit.value = { ...l, paths: [...l.paths] }
}
async function save() {
  busy.value = true
  try {
    const l = { ...edit.value, paths: edit.value.paths.filter((p) => p.trim()) }
    await rpc(l.id ? 'admin.libs.update' : 'admin.libs.create', l)
    toast('已保存，开始扫描', 'ok')
    edit.value = null
    load()
    setTimeout(loadProgress, 200)
  } catch (e) {
    toastError(e)
  } finally {
    busy.value = false
  }
}

async function scan(l, refresh) {
  // 先在本地显示“排队中”，按钮立刻有反馈
  progress.value = { ...progress.value, [l.id]: { libraryId: l.id, phase: 'queued', refresh, found: 0, added: 0, removed: 0, total: 0, done: 0, failed: 0, trickTotal: 0, trickDone: 0 } }
  try {
    await rpc('admin.libs.scan', { id: l.id, refresh })
  } catch (e) {
    toastError(e)
    const next = { ...progress.value }
    delete next[l.id]
    progress.value = next
  }
  setTimeout(loadProgress, 150)
}

async function doDelete() {
  try {
    await rpc('admin.libs.delete', { id: del.value.id })
    del.value = null
    load()
  } catch (e) {
    toastError(e)
  }
}

</script>

<template>
  <div class="admin-body">
    <div class="row head">
      <span class="muted">媒体库目录在服务器上，支持 mp4 / m4v / mov / webm。</span>
      <div class="spacer" />
      <button class="btn primary" @click="openNew">添加媒体库</button>
    </div>
    <div v-if="!libs.length" class="card-panel empty">还没有媒体库，点击右上角添加。</div>
    <div class="libs">
      <div v-for="(l, i) in libs" :key="l.id" class="card-panel lib" :class="{ scanning: isActive(progress[l.id]) }" :style="{ '--i': i }">
        <div class="lhead">
          <h2>{{ l.name }} <Icon v-if="l.restricted" name="lock" :size="16" class="muted" /></h2>
          <span class="muted">{{ kindNames[l.kind] }} · {{ l.count }} 个视频 · {{ scraperNames[l.scraper] }}{{ l.trickplay ? ' · 生成进度条预览图' : '' }}</span>
        </div>
        <div v-for="p in l.paths" :key="p" class="path">{{ p }}</div>
        <div class="muted small">上次扫描：{{ l.lastScanAt ? fmtDateTime(l.lastScanAt) : '从未' }}</div>
        <div class="expand" :class="{ open: now && shown(l.id) }">
          <div>
            <ScanProgress v-if="display(l.id)" :p="display(l.id)" />
          </div>
        </div>
        <div class="row ops">
          <button class="btn" :disabled="isActive(progress[l.id])" @click="scan(l, false)">
            <Icon name="refresh" :size="18" class="spin-icon" :class="{ on: isActive(progress[l.id]) }" />
            {{ isActive(progress[l.id]) ? '扫描中' : '扫描' }}
          </button>
          <button class="btn" :disabled="isActive(progress[l.id])" @click="scan(l, true)">刷新元数据</button>
          <button class="btn" @click="openEdit(l)"><Icon name="edit" :size="18" /> 编辑</button>
          <div class="spacer" />
          <button class="btn ghost err" @click="del = l">删除</button>
        </div>
      </div>
    </div>

    <Modal v-if="edit" :title="edit.id ? '编辑媒体库' : '添加媒体库'" width="600px" @close="edit = null">
      <LibraryForm :lib="edit" @submit="save" />
      <template #footer>
        <button class="btn ghost" @click="edit = null">取消</button>
        <button class="btn primary" :disabled="busy" @click="save">保存并扫描</button>
      </template>
    </Modal>

    <Modal v-if="del" title="删除媒体库" @close="del = null">
      <p>确定删除媒体库「{{ del.name }}」？只会删除数据库中的条目、观看记录和缓存的图片，<b>不会删除硬盘上的视频文件</b>。</p>
      <template #footer>
        <button class="btn ghost" @click="del = null">取消</button>
        <button class="btn danger" @click="doDelete">删除</button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.head { margin-bottom: 14px; }
.libs { display: grid; grid-template-columns: repeat(auto-fill, minmax(420px, 1fr)); gap: 14px; }
@media (max-width: 500px) { .libs { grid-template-columns: 1fr; } }
@media (max-width: 600px) {
  .head .btn { width: 100%; justify-content: center; }
  .ops { gap: 6px; }
  .ops .btn { padding: 0 10px; }
}
.lhead { margin-bottom: 8px; }
.lhead h2 { font-size: 18px; display: flex; align-items: center; gap: 6px; }
.path { font-family: Consolas, monospace; font-size: 12.5px; color: #bbb; word-break: break-all; padding: 2px 0; }
.small { font-size: 12.5px; margin-top: 6px; }
.ops { margin-top: 14px; }
/* 扫描中：卡片边缘有一圈缓慢呼吸的绿光 */
.lib { position: relative; }
.lib::after { content: ''; position: absolute; inset: 0; border-radius: inherit; pointer-events: none; opacity: 0; transition: opacity 0.5s var(--ease-out);
  box-shadow: 0 0 0 1px rgba(82, 181, 75, 0.6), 0 0 24px rgba(82, 181, 75, 0.2); }
.lib.scanning::after { animation: glow 2.4s ease-in-out infinite; }
@keyframes glow { 0%, 100% { opacity: 0.35; } 50% { opacity: 1; } }
/* 进度区域展开 / 收起：用 grid 行高从 0fr 过渡到 1fr，高度自适应 */
.expand { display: grid; grid-template-rows: 0fr; opacity: 0; transition: grid-template-rows 0.45s var(--spring), opacity 0.3s var(--ease-out); }
.expand.open { grid-template-rows: 1fr; opacity: 1; }
.expand > div { overflow: hidden; min-height: 0; }
.spin-icon { transition: transform 0.3s var(--spring); }
.spin-icon.on { animation: spin 0.9s linear infinite; }
</style>
