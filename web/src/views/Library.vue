<script setup>
import { ref, computed, watch, onMounted, onBeforeUnmount, onActivated, nextTick } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { rpc, toast, toastError } from '../store.js'
import { debounce, kindNames, randomSeed } from '../util.js'
import { setQueue } from '../queue.js'
import MediaCard from '../components/MediaCard.vue'
import Icon from '../components/Icon.vue'
import { useIndicator } from '../composables/indicator.js'

const props = defineProps({ id: String })
const route = useRoute()
const router = useRouter()

const lib = ref(null)
const items = ref([])
const folders = ref([])
const total = ref(0)
const loading = ref(false)
const done = ref(false)
const filters = ref({ genre: [], tag: [], actor: [], studio: [], series: [], years: [] })
const sentinel = ref(null)
const PAGE = 60

// 筛选条件保存在 URL 里，返回时可以恢复
const q = computed(() => route.query)
const browse = computed(() => q.value.view === 'folders')
const folder = computed(() => q.value.folder || '')
const searchText = ref(q.value.q || '')
const seg = ref(null)
const { style: thumbStyle } = useIndicator(seg, 'button.on', () => browse.value)
const firstLoad = ref(true)
const showFilters = ref(false)
const expanded = ref({}) // 哪些筛选分组展开了全部选项
const shuffling = ref(false)

const shape = computed(() => (lib.value?.kind === 'home' ? 'thumb' : 'poster'))
const crumbs = computed(() => {
  const parts = folder.value ? folder.value.split('/') : []
  return parts.map((name, i) => ({ name, path: parts.slice(0, i + 1).join('/') }))
})

// ---- 筛选分组 ----
const fixed = {
  status: { name: '观看状态', opts: [['unplayed', '未看'], ['inprogress', '观看中'], ['played', '已看']] },
  res: { name: '分辨率', opts: [['4k', '4K'], ['1080', '1080p'], ['720', '720p'], ['sd', '标清']] },
  dur: { name: '时长', opts: [['short', '20 分钟内'], ['medium', '20–60 分钟'], ['long', '1–2 小时'], ['xlong', '2 小时以上']] },
}
// URL 参数名 → 筛选项来源
const dynamic = [
  ['genre', '类型', 'genre'],
  ['label', '标签', 'tag'],
  ['actor', '演员', 'actor'],
  ['studio', '制作', 'studio'],
  ['series', '系列', 'series'],
  ['year', '年份', 'years'],
]
const FILTER_KEYS = [...Object.keys(fixed), ...dynamic.map((d) => d[0])]
const COLLAPSED = 18

const sections = computed(() => {
  const out = Object.entries(fixed).map(([key, f]) => ({ key, name: f.name, opts: f.opts }))
  for (const [key, name, src] of dynamic) {
    const list = (filters.value[src] || []).map((v) => [String(v), String(v)])
    if (list.length) out.push({ key, name, opts: list })
  }
  return out.map((s) => {
    const cur = q.value[s.key]
    let shown = s.opts
    const more = s.opts.length > COLLAPSED && !expanded.value[s.key]
    if (more) {
      shown = s.opts.slice(0, COLLAPSED)
      // 当前选中的值即使排在后面也要显示出来
      if (cur && !shown.some((o) => o[0] === cur)) shown = [...shown, s.opts.find((o) => o[0] === cur) || [cur, cur]]
    }
    return { ...s, shown, more, hidden: s.opts.length - shown.length }
  })
})

const active = computed(() =>
  sections.value
    .filter((s) => q.value[s.key])
    .map((s) => ({ key: s.key, text: `${s.name}：${s.opts.find((o) => o[0] === q.value[s.key])?.[1] || q.value[s.key]}` })),
)
// 区块还没加载出来时（例如直接打开带筛选条件的链接），也按 URL 计数
const activeCount = computed(() => FILTER_KEYS.filter((k) => q.value[k]).length)

function setQuery(patch) {
  const next = { ...route.query, ...patch }
  for (const k of Object.keys(next)) if (next[k] === '' || next[k] == null) delete next[k]
  router.replace({ query: next })
}
function pick(key, val) {
  setQuery({ [key]: q.value[key] === val ? '' : val })
}
function clearFilters() {
  setQuery(Object.fromEntries(FILTER_KEYS.map((k) => [k, ''])))
}
function setSort(v) {
  setQuery({ sort: v, seed: v === 'random' ? String(randomSeed()) : '' })
}
function reshuffle() {
  setQuery({ seed: String(randomSeed()) })
}
const applySearch = debounce((v) => setQuery({ q: v }), 350)
watch(searchText, applySearch)

// 当前页面的查询参数（列表和随机播放共用）
function params() {
  const x = q.value
  const p = {
    libraryId: Number(props.id),
    browse: browse.value,
    folder: folder.value,
    q: x.q || '',
    sort: x.sort || 'added',
    desc: x.order !== 'asc',
    seed: Number(x.seed) || 0,
  }
  for (const k of FILTER_KEYS) if (x[k]) p[k] = x[k]
  return p
}

let gen = 0 // 每次重新加载列表加一：还没返回的旧请求（翻页、静默刷新）据此丢弃结果
async function load(reset) {
  if (reset) {
    // 条件变了一定要重新加载，不能因为上一页还在加载就丢掉（否则列表和 URL 里的筛选条件对不上）
    gen++
    items.value = []
    folders.value = []
    done.value = false
  } else if (loading.value) return
  if (done.value) return
  const g = gen
  loading.value = true
  try {
    const r = await rpc('items.list', { ...params(), offset: items.value.length, limit: PAGE })
    if (g !== gen) return
    if (reset) folders.value = r.folders || []
    items.value.push(...r.items)
    total.value = r.total
    done.value = items.value.length >= r.total || r.items.length === 0
    firstLoad.value = false
  } catch (e) {
    if (g !== gen) return
    toastError(e)
    done.value = true
  } finally {
    if (g === gen) loading.value = false
  }
  await nextTick()
  checkSentinel()
}

// 随机播放：按当前的筛选条件（文件夹视图下包含子文件夹）打乱顺序，生成播放队列
async function shufflePlay() {
  if (shuffling.value) return
  shuffling.value = true
  try {
    const r = await rpc('items.ids', { ...params(), sort: 'random', seed: randomSeed() })
    if (!r.ids.length) {
      toast('没有可以播放的视频')
      return
    }
    const where = browse.value && folder.value ? folder.value.split('/').pop() : lib.value?.name || '媒体库'
    setQueue(r.ids, `随机播放 · ${where}${activeCount.value ? '（已筛选）' : ''}`)
    router.push({ path: `/play/${r.ids[0]}`, query: { queue: '1' } })
  } catch (e) {
    toastError(e)
  } finally {
    shuffling.value = false
  }
}

let io = null
function checkSentinel() {
  if (!sentinel.value || done.value) return
  const r = sentinel.value.getBoundingClientRect()
  if (r.top < window.innerHeight + 600) load(false)
}

// 兼容旧链接：?kind=actor&tag=X、?played=yes
function migrateQuery() {
  const x = route.query
  const patch = {}
  if (x.kind && x.tag) patch[{ tag: 'label' }[x.kind] || x.kind] = x.tag
  if (x.kind || x.tag) Object.assign(patch, { kind: '', tag: '' })
  if (x.played) Object.assign(patch, { played: '', status: x.played === 'yes' ? 'played' : 'unplayed' })
  if (x.sort === 'random' && !x.seed) patch.seed = String(randomSeed())
  if (Object.keys(patch).length) {
    setQuery(patch)
    return true
  }
  return false
}

onMounted(async () => {
  const migrated = migrateQuery()
  if (activeCount.value) showFilters.value = true
  try {
    const libs = await rpc('lib.list')
    lib.value = libs.find((l) => String(l.id) === props.id) || null
    filters.value = await rpc('items.filters', { libraryId: Number(props.id) })
  } catch (e) {
    toastError(e)
  }
  io = new IntersectionObserver((es) => es.some((e) => e.isIntersecting) && load(false), { rootMargin: '600px' })
  if (sentinel.value) io.observe(sentinel.value)
  if (!migrated) load(true) // 改写了 URL 的话由下面的 watch 触发加载
})
onBeforeUnmount(() => io?.disconnect())

// 页面被缓存：从详情页 / 播放页回来时，已加载的条目原地刷新（观看进度、已看标记、被删除的条目），
// 不清空列表，滚动位置保持不变。服务端单次最多返回 500 条，分批取。
let first = true
onActivated(async () => {
  if (first) return (first = false)
  const n = items.value.length
  if (!n || loading.value || route.path !== `/library/${props.id}`) return
  const g = gen
  try {
    const out = []
    let r = null
    while (out.length < n) {
      r = await rpc('items.list', { ...params(), offset: out.length, limit: Math.min(500, n - out.length) })
      if (g !== gen) return
      out.push(...r.items)
      if (!r.items.length) break
    }
    if (loading.value) return
    items.value = out
    total.value = r.total
    done.value = out.length >= r.total
  } catch {}
})
// 离开页面（过渡动画期间路由已经变了）时不要按新页面的参数重新加载
watch(() => route.query, () => route.path === `/library/${props.id}` && load(true), { deep: true })
</script>

<template>
  <div class="page">
    <h1 class="page-title">
      {{ lib?.name || '媒体库' }}
      <span class="muted count">{{ total }} 个{{ browse ? '（当前文件夹）' : '' }}</span>
    </h1>

    <div class="toolbar">
      <div ref="seg" class="seg">
        <span class="indicator seg-thumb" :style="thumbStyle" />
        <button :class="{ on: !browse }" @click="setQuery({ view: '', folder: '' })">全部</button>
        <button :class="{ on: browse }" @click="setQuery({ view: 'folders' })"><Icon name="folder" :size="16" /> 文件夹</button>
      </div>
      <input v-model="searchText" class="input search" placeholder="在此媒体库中搜索标题、番号、演员…" />
      <select class="input sel" :value="q.sort || 'added'" @change="setSort($event.target.value)">
        <option value="added">添加时间</option>
        <option value="title">标题</option>
        <option value="release">发行日期</option>
        <option value="duration">时长</option>
        <option value="rating">评分</option>
        <option value="lastPlayed">最近播放</option>
        <option value="random">随机</option>
      </select>
      <Transition name="swap-btn" mode="out-in">
        <button v-if="q.sort === 'random'" key="re" class="btn" title="重新打乱顺序" @click="reshuffle"><Icon name="shuffle" :size="18" /> 换一批</button>
        <button v-else key="ord" class="btn" :title="q.order === 'asc' ? '升序' : '降序'" @click="setQuery({ order: q.order === 'asc' ? '' : 'asc' })">
          {{ q.order === 'asc' ? '↑ 升序' : '↓ 降序' }}
        </button>
      </Transition>
      <button class="btn filter-btn" :class="{ open: showFilters, has: activeCount }" @click="showFilters = !showFilters">
        <Icon name="filter" :size="18" /> 筛选
        <Transition name="badge-pop"><span v-if="activeCount" :key="activeCount" class="fcount">{{ activeCount }}</span></Transition>
      </button>
      <div class="spacer" />
      <button class="btn primary shuffle-btn" :disabled="shuffling || (!total && !folders.length)" title="按当前筛选条件随机连续播放" @click="shufflePlay">
        <Icon name="shuffle" :size="18" /> 随机播放
      </button>
    </div>

    <!-- 筛选面板：用 grid 行高从 0fr 过渡到 1fr，展开收起都跟随弹簧曲线 -->
    <div class="fpanel-wrap" :class="{ open: showFilters }">
      <div class="fpanel-clip">
        <div class="fpanel">
          <div v-for="s in sections" :key="s.key" class="fsec">
            <div class="fname">{{ s.name }}</div>
            <div class="fopts">
              <button v-for="o in s.shown" :key="o[0]" class="fchip" :class="{ on: q[s.key] === o[0] }" @click="pick(s.key, o[0])">{{ o[1] }}</button>
              <button v-if="s.more" class="fchip more" @click="expanded[s.key] = true">更多 {{ s.hidden }} 个…</button>
              <button v-else-if="expanded[s.key]" class="fchip more" @click="expanded[s.key] = false">收起</button>
            </div>
          </div>
        </div>
      </div>
    </div>

    <TransitionGroup v-if="active.length" tag="div" name="achip" class="active-bar">
      <button v-for="a in active" :key="a.key" class="achip" :title="'移除筛选：' + a.text" @click="setQuery({ [a.key]: '' })">
        {{ a.text }} <Icon name="close" :size="14" />
      </button>
      <button key="__clear" class="clear-all" @click="clearFilters">清除全部</button>
    </TransitionGroup>

    <Transition name="slide-up">
      <TransitionGroup v-if="browse" tag="div" name="crumb" class="crumbs">
        <a key="root" @click="setQuery({ folder: '' })">{{ lib?.name }}</a>
        <span v-for="c in crumbs" :key="c.path" class="crumb">
          <Icon name="chevronRight" :size="16" class="muted" />
          <a @click="setQuery({ folder: c.path })">{{ c.name }}</a>
        </span>
      </TransitionGroup>
    </Transition>

    <div v-if="browse && folders.length" class="folders">
      <button v-for="(f, i) in folders" :key="f.path" class="folder rise" :style="{ '--i': i }" @click="setQuery({ folder: f.path })">
        <Icon name="folder" :size="34" class="fi" />
        <div class="fname" :title="f.name">{{ f.name }}</div>
        <div class="muted small">{{ f.count }} 个视频</div>
      </button>
    </div>

    <div v-if="firstLoad && loading" class="grid" :class="shape">
      <div v-for="n in 12" :key="n" class="skel skel-card" :class="shape" :style="{ animationDelay: n * 60 + 'ms' }" />
    </div>
    <div class="grid" :class="shape">
      <MediaCard v-for="(it, i) in items" :key="it.id" :item="it" :shape="shape" :index="i" />
    </div>
    <div v-if="!loading && !items.length && !folders.length" class="empty rise">
      <template v-if="q.q || activeCount">
        <p>没有符合条件的视频</p>
        <button v-if="activeCount" class="btn" @click="clearFilters">清除筛选条件</button>
      </template>
      <template v-else>{{ lib ? `这个${kindNames[lib.kind] || ''}媒体库还是空的` : '' }}</template>
    </div>
    <div ref="sentinel" class="sentinel" />
    <Transition name="fade">
      <div v-if="loading && !firstLoad" class="loading"><div class="spinner" /></div>
    </Transition>
  </div>
</template>

<style scoped>
.count { font-size: 15px; }
.toolbar { display: flex; flex-wrap: wrap; gap: 10px; margin-bottom: 14px; align-items: center; }
.search { width: 280px; max-width: 100%; }
.sel { width: auto; padding-right: 28px; }
/* iOS 分段控件：选中块在两个选项之间滑动 */
.seg { position: relative; display: inline-flex; background: var(--bg2); border: 1px solid var(--line); border-radius: 10px; padding: 3px; }
.seg button { position: relative; z-index: 1; padding: 6px 14px; display: inline-flex; align-items: center; gap: 4px; border-radius: 8px; color: var(--muted); transition: color .3s var(--ease-out), transform .35s var(--spring-bouncy); }
.seg button.on { color: #fff; }
.seg-thumb { left: 0; top: 0; border-radius: 8px; background: var(--accent); box-shadow: 0 2px 8px rgba(0,0,0,.35); }

.swap-btn-enter-active { transition: opacity .2s, transform .4s var(--spring-bouncy); }
.swap-btn-leave-active { transition: opacity .12s, transform .12s; }
.swap-btn-enter-from, .swap-btn-leave-to { opacity: 0; transform: scale(.85); }

.filter-btn { position: relative; }
.filter-btn.open, .filter-btn.has { color: var(--accent); }
.filter-btn.open { background: rgba(82,181,75,.14); }
.fcount { min-width: 18px; height: 18px; padding: 0 5px; border-radius: 9px; background: var(--accent); color: #fff; font-size: 11px; font-weight: 600;
  display: inline-flex; align-items: center; justify-content: center; }
.badge-pop-enter-active { transition: transform .45s var(--spring-bouncy), opacity .2s; }
.badge-pop-leave-active { transition: transform .15s, opacity .15s; position: absolute; right: 6px; }
.badge-pop-enter-from, .badge-pop-leave-to { transform: scale(0); opacity: 0; }
.shuffle-btn:active:not(:disabled) .icon { transform: rotate(180deg); }
.shuffle-btn .icon { transition: transform .5s var(--spring-bouncy); }

/* 筛选面板 */
.fpanel-wrap { display: grid; grid-template-rows: 0fr; opacity: 0; transition: grid-template-rows .55s var(--spring), opacity .3s var(--ease-out), margin .55s var(--spring); }
.fpanel-wrap.open { grid-template-rows: 1fr; opacity: 1; margin-bottom: 16px; }
.fpanel-clip { min-height: 0; overflow: hidden; }
.fpanel { display: grid; gap: 12px; padding: 16px 18px; border-radius: 14px; background: rgba(255,255,255,.035); border: 0.5px solid rgba(255,255,255,.08);
  transform: translate3d(0, -10px, 0) scale(.985); transform-origin: top center; transition: transform .55s var(--spring); }
.open .fpanel { transform: none; }
.fsec { display: flex; gap: 14px; align-items: flex-start; }
.fsec .fname { width: 64px; flex-shrink: 0; padding-top: 5px; color: var(--muted); font-size: 13px; white-space: nowrap; }
.fopts { display: flex; flex-wrap: wrap; gap: 6px; min-width: 0; }
.fchip { padding: 4px 12px; border-radius: 999px; background: var(--panel2); font-size: 13px; color: #ddd; max-width: 260px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  transition: background-color .25s var(--ease-out), color .25s, box-shadow .25s, transform .35s var(--spring-bouncy); }
.fchip:hover { background: #363636; }
.fchip:active { transform: scale(.92); transition-duration: .08s; }
.fchip.on { background: var(--accent); color: #fff; box-shadow: 0 2px 10px rgba(82,181,75,.35); }
.fchip.more { background: transparent; color: var(--accent); }

/* 已选条件 */
.active-bar { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; margin-bottom: 16px; }
.achip { display: inline-flex; align-items: center; gap: 4px; padding: 4px 8px 4px 12px; border-radius: 999px; font-size: 13px;
  background: rgba(82,181,75,.16); color: #cfeccc; border: 0.5px solid rgba(82,181,75,.35); transition: background-color .2s, transform .35s var(--spring-bouncy); }
.achip:hover { background: rgba(82,181,75,.28); }
.achip:active { transform: scale(.94); }
.clear-all { font-size: 13px; color: var(--muted); padding: 4px 6px; transition: color .2s; }
.clear-all:hover { color: #fff; }
.achip-enter-active { transition: opacity .25s, transform .45s var(--spring-bouncy); }
.achip-leave-active { transition: opacity .15s, transform .2s; position: absolute; }
.achip-enter-from, .achip-leave-to { opacity: 0; transform: scale(.7); }
.achip-move { transition: transform .45s var(--spring); }

.skel-card.poster { aspect-ratio: 2 / 3; }
.skel-card.thumb { aspect-ratio: 16 / 9; }
.crumb { display: inline-flex; align-items: center; gap: 6px; }
.crumb-enter-active { transition: opacity .3s var(--ease-out), transform .45s var(--spring); }
.crumb-leave-active { transition: opacity .15s; position: absolute; }
.crumb-enter-from { opacity: 0; transform: translateX(-10px); }
.crumb-leave-to { opacity: 0; }
.crumbs { display: flex; align-items: center; gap: 6px; margin-bottom: 14px; flex-wrap: wrap; }
.crumbs a { cursor: pointer; }
.crumbs a:hover { color: var(--accent); }
.folders { display: grid; grid-template-columns: repeat(auto-fill, minmax(170px, 1fr)); gap: 12px; margin-bottom: 24px; }
.folder { background: var(--panel); border-radius: 12px; padding: 14px; text-align: left; }
.folder:hover { box-shadow: 0 0 0 2px var(--accent), 0 12px 24px rgba(0,0,0,.35); transform: translateY(-2px); }
.folder .fi { transition: transform .45s var(--spring-bouncy); }
.folder:hover .fi { transform: scale(1.12) rotate(-6deg); }
.fi { color: #d9b44a; }
.folder .fname { margin-top: 6px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.empty .btn { margin-top: 12px; }
.small { font-size: 12px; }
.sentinel { height: 1px; }
@media (max-width: 600px) {
  .fsec { flex-direction: column; gap: 6px; }
  .fsec .fname { padding-top: 0; }
  .spacer { display: none; }
}
</style>
