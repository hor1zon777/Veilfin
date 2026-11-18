<script setup>
import { ref, computed, onMounted, onBeforeUnmount, watch } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { state, rpc, beacon, toastError } from '../store.js'
import { fmtTime, randomId } from '../util.js'
import { queueAt } from '../queue.js'
import Icon from '../components/Icon.vue'

const props = defineProps({ id: String })
const router = useRouter()
const route = useRoute()

const root = ref(null)
const video = ref(null)
const bar = ref(null)
const data = ref(null)
const err = ref('')

const playing = ref(false)
const waiting = ref(true)
const cur = ref(0)
const dur = ref(0)
const buffered = ref([])
const volume = ref(Number(localStorage.getItem('vf.volume') ?? 1))
const muted = ref(localStorage.getItem('vf.muted') === '1')
const rate = ref(1)
const fullscreen = ref(false)
// 移动端（触屏为主的设备）全屏时交给系统播放器：iOS 用 webkitEnterFullscreen，其他用 <video> 元素自身全屏 + 系统控件
const mobile = matchMedia('(pointer: coarse)').matches
const nativeFs = ref(false) // 正在使用系统全屏播放
const showUI = ref(true)
const menu = ref('') // '' | 'subs' | 'speed'
const subIndex = ref(-1)
const hover = ref(null) // { x, t }
const ended = ref(false)
const countdown = ref(0)
const flash = ref('') // 快捷键提示
const bubble = ref(null) // 中央反馈气泡 { icon, key }
const queue = ref(null) // 播放队列（随机播放）中的位置 { label, index, total, rest }
const resumed = ref(0) // 自动续播的起点（秒），用于显示“从头播放”提示
// 画面旋转角度：一直累加（可以超过 360°），这样每次都朝同一方向转，动画不会倒着绕回去。
// 按视频记在本机，竖拍 / 方向录错的视频转一次以后都保持。
const ROT_KEY = `vf.rot.${props.id}`
const rot = ref(Number(localStorage.getItem(ROT_KEY)) || 0)
const sideways = computed(() => Math.abs(rot.value / 90) % 2 === 1)
let resumeTimer = null
let bubbleSeq = 0
function showBubble(icon, text) {
  bubble.value = { icon, text, key: ++bubbleSeq }
  clearTimeout(showBubble.t)
  showBubble.t = setTimeout(() => (bubble.value = null), 600)
}

const psid = randomId()
const prevTitle = document.title // 离开播放页时恢复
let hideTimer = null
let reportTimer = null
let countdownTimer = null
let lastReport = 0

const pct = computed(() => (dur.value ? (cur.value / dur.value) * 100 : 0))
const trick = computed(() => data.value?.trickplay)

async function load() {
  try {
    queue.value = route.query.queue ? queueAt(props.id) : null
    const p = { id: Number(props.id) }
    if (queue.value) p.queue = queue.value.rest
    data.value = await rpc('play.open', p)
    // 管理后台可以设置播放时不把标签页标题改成视频名称
    if (!state.me?.keepPageTitle) document.title = data.value.item.title
    const d = data.value.item.duration
    if (d) dur.value = d
  } catch (e) {
    err.value = e.message
  }
}

function onLoaded() {
  const v = video.value
  if (!v) return
  v.volume = volume.value
  v.muted = muted.value
  if (isFinite(v.duration)) dur.value = v.duration
  // 起点：链接里指定了 t（包括 t=0 表示从头播放）就用它，否则从上次看到的位置继续
  const it = data.value.item
  const auto = route.query.t === undefined
  const t = auto ? it.position || 0 : Number(route.query.t) || 0
  if (t > 0 && t < v.duration - 5) {
    v.currentTime = t
    if (auto) {
      resumed.value = t
      clearTimeout(resumeTimer)
      resumeTimer = setTimeout(() => (resumed.value = 0), 7000)
    }
  }
  v.play().catch(() => {
    // 浏览器禁止有声自动播放时，静音后再试
    v.muted = true
    muted.value = true
    v.play().catch(() => {})
  })
}

function report(final) {
  const v = video.value
  if (!v || !data.value || !(v.currentTime > 0)) return
  const p = { id: data.value.item.id, psid, position: v.currentTime, duration: v.duration || dur.value }
  if (final) beacon('play.report', p)
  else rpc('play.report', p).catch(() => {})
  lastReport = Date.now()
}

function onTime() {
  const v = video.value
  if (!v) return // 卸载过程中仍可能触发事件
  cur.value = v.currentTime
  if (playing.value && Date.now() - lastReport > 10000) report()
}
function onProgress() {
  const v = video.value
  if (!v) return
  const out = []
  for (let i = 0; i < v.buffered.length; i++) out.push([v.buffered.start(i), v.buffered.end(i)])
  buffered.value = out
}
function onPlay() {
  playing.value = true
  ended.value = false
  cancelNext() // 播完后又重新播放：取消“下一个”的倒计时，否则会被强行跳走
  poke()
}
function onPause() {
  playing.value = false
  showUI.value = true
  report()
}
function onEnded() {
  // 系统全屏里看不到“下一集”倒计时，先退出全屏
  if (nativeFs.value) {
    if (document.fullscreenElement) document.exitFullscreen().catch(() => {})
    else video.value?.webkitExitFullscreen?.()
  }
  playing.value = false
  ended.value = true
  showUI.value = true
  report()
  if (data.value?.next) {
    countdown.value = 10
    clearInterval(countdownTimer) // 重播后再次播完：先清掉旧的，不然旧定时器丢了句柄、一直在跑
    countdownTimer = setInterval(() => {
      countdown.value--
      if (countdown.value <= 0) playNext()
    }, 1000)
  }
}
function onError() {
  const code = video.value?.error?.code
  err.value = code === 4 ? '浏览器不支持这个视频的格式或编码（例如 HEVC），无法播放' : '播放出错：视频数据加载失败或校验未通过'
}

function rotate(dir = 1) {
  rot.value += dir * 90
  const norm = ((rot.value % 360) + 360) % 360
  if (norm) localStorage.setItem(ROT_KEY, norm)
  else localStorage.removeItem(ROT_KEY)
  showFlash(norm ? `旋转 ${norm}°` : '恢复原始方向')
}

function restart() {
  resumed.value = 0
  seek(0)
  video.value?.play().catch(() => {})
  showBubble('replay', '从头播放')
}

function toggle() {
  const v = video.value
  if (!v) return
  if (v.paused || v.ended) {
    v.play()
    showBubble('play')
  } else {
    v.pause()
    showBubble('pause')
  }
}
function seek(t) {
  const v = video.value
  if (!v) return
  v.currentTime = Math.max(0, Math.min(t, (v.duration || dur.value) - 0.5))
  cur.value = v.currentTime
}
function skip(d) {
  if (!video.value) return
  seek(video.value.currentTime + d)
  showBubble(d > 0 ? 'forward' : 'rewind', (d > 0 ? '+' : '') + d + ' 秒')
}
function setVolume(v) {
  volume.value = Math.max(0, Math.min(1, v))
  if (video.value) video.value.volume = volume.value
  if (volume.value > 0 && muted.value) toggleMute(false)
  localStorage.setItem('vf.volume', volume.value)
}
function toggleMute(force) {
  muted.value = typeof force === 'boolean' ? force : !muted.value
  if (video.value) video.value.muted = muted.value
  localStorage.setItem('vf.muted', muted.value ? '1' : '0')
}
function setRate(r) {
  rate.value = r
  video.value.playbackRate = r
  menu.value = ''
  showFlash(r + '×')
}
function setSub(i) {
  subIndex.value = i
  const tracks = video.value.textTracks
  for (let j = 0; j < tracks.length; j++) tracks[j].mode = j === i ? 'showing' : 'disabled'
  menu.value = ''
}
async function toggleFullscreen() {
  try {
    if (document.fullscreenElement) await document.exitFullscreen()
    else if (mobile) await enterNativeFullscreen()
    else await root.value.requestFullscreen()
  } catch {}
}
async function enterNativeFullscreen() {
  const v = video.value
  if (!v) return
  menu.value = ''
  if (v.requestFullscreen) {
    // 元素自身全屏时打开 controls，由系统显示播放控件
    v.controls = true
    nativeFs.value = true
    try {
      await v.requestFullscreen({ navigationUI: 'hide' })
    } catch (e) {
      leaveNativeFullscreen()
      if (!v.webkitEnterFullscreen) throw e
      v.webkitEnterFullscreen()
      return
    }
    // 横屏视频顺便锁定横屏（不支持的浏览器忽略）
    if (v.videoWidth > v.videoHeight) screen.orientation?.lock?.('landscape').catch(() => {})
  } else if (v.webkitEnterFullscreen) {
    // iPhone 不支持元素全屏，只能进入系统播放器
    v.webkitEnterFullscreen()
  }
}
function leaveNativeFullscreen() {
  if (!nativeFs.value) return
  nativeFs.value = false
  if (video.value) video.value.controls = false
  try {
    screen.orientation?.unlock?.()
  } catch {}
}
function onFsChange() {
  fullscreen.value = !!document.fullscreenElement
  if (!document.fullscreenElement) leaveNativeFullscreen()
}
// iOS 系统播放器的进入 / 退出事件
function onWebkitBegin() {
  nativeFs.value = true
  fullscreen.value = true
}
function onWebkitEnd() {
  nativeFs.value = false
  fullscreen.value = false
  poke()
}
// 系统全屏时，点击 / 双击交给系统控件处理。
// 移动端点击画面只切换控制栏的显示 / 隐藏，不暂停（暂停用中间的播放键）
let uiBeforeTouch = true // 这次触摸之前控制栏是否已经显示（touchstart 会先把它唤出来）
function onTouch() {
  uiBeforeTouch = showUI.value
  poke()
}
function onVideoClick() {
  if (nativeFs.value) return
  if (!mobile) return toggle()
  if (uiBeforeTouch) {
    clearTimeout(hideTimer)
    menu.value = ''
    showUI.value = false
  } else poke()
}
function onVideoDblclick() {
  if (!nativeFs.value) toggleFullscreen()
}
function showFlash(s) {
  flash.value = s
  clearTimeout(showFlash.t)
  showFlash.t = setTimeout(() => (flash.value = ''), 700)
}

// 控制栏自动隐藏
function poke() {
  showUI.value = true
  clearTimeout(hideTimer)
  hideTimer = setTimeout(() => {
    if (playing.value && !menu.value) showUI.value = false
  }, 3000)
}

// 进度条
function barTime(e) {
  const r = bar.value.getBoundingClientRect()
  const x = Math.max(0, Math.min(e.clientX - r.left, r.width))
  return { x, t: (x / r.width) * dur.value, w: r.width }
}
let dragging = false
function onBarDown(e) {
  dragging = true
  seek(barTime(e).t)
  window.addEventListener('pointermove', onBarDrag)
  window.addEventListener('pointerup', onBarUp, { once: true })
}
function onBarDrag(e) {
  if (dragging) {
    const b = barTime(e)
    cur.value = b.t
    hover.value = b
  }
}
function onBarUp(e) {
  dragging = false
  window.removeEventListener('pointermove', onBarDrag)
  if (bar.value) seek(barTime(e).t)
}
function onBarMove(e) {
  hover.value = barTime(e)
  loadSprites() // 播放还没开始就先悬停：立即开始预载
}

// 进度条预览图预载：开始播放后在后台逐张拉取雪碧图，转成 blob: URL 留在内存里，
// 悬停时立即显示，不会因为解密缓存被视频数据挤掉而反复重新下载。悬停到的那张优先。
const spriteUrls = ref([])
let spriteStarted = false
let spriteWant = -1
let spriteAbort = null
let spriteTimer = null
let spriteList = [] // 创建过的 blob: URL，离开页面时释放
async function loadSprites() {
  const tp = trick.value
  if (spriteStarted || !tp?.sprites?.length) return
  spriteStarted = true
  clearTimeout(spriteTimer)
  spriteAbort = new AbortController()
  const urls = new Array(tp.sprites.length).fill(null)
  spriteList = urls
  try {
    for (;;) {
      const k = spriteWant >= 0 && !urls[spriteWant] ? spriteWant : urls.findIndex((u) => !u)
      if (k < 0) break
      const res = await fetch(tp.sprites[k], { signal: spriteAbort.signal })
      if (!res.ok) throw new Error('HTTP ' + res.status)
      const blob = await res.blob()
      if (spriteAbort.signal.aborted) break
      urls[k] = URL.createObjectURL(blob)
      // 先解码一次，第一次显示时不闪
      const img = new Image()
      img.src = urls[k]
      await img.decode().catch(() => {})
      spriteUrls.value = urls.slice()
    }
  } catch {
    // 预载失败（例如离开页面）：悬停时退回直接引用原地址
  }
  spriteUrls.value = urls.slice()
}
function onPlayingOnce() {
  waiting.value = false
  // 等起播的缓冲先完成，再在后台预载
  if (!spriteStarted && !spriteTimer) spriteTimer = setTimeout(loadSprites, 1500)
}

// 进度条预览图：雪碧图中第 n 张
const preview = computed(() => {
  const tp = trick.value
  const h = hover.value
  if (!tp || !h || !tp.sprites?.length) return null
  const per = tp.cols * tp.rows
  const n = Math.min(Math.floor(h.t / tp.interval), tp.count - 1)
  const sheet = Math.floor(n / per)
  const k = n % per
  if (!tp.sprites[sheet]) return null
  spriteWant = sheet
  return {
    backgroundImage: `url("${spriteUrls.value[sheet] || tp.sprites[sheet]}")`,
    backgroundPosition: `-${(k % tp.cols) * tp.width}px -${Math.floor(k / tp.cols) * tp.height}px`,
    width: tp.width + 'px',
    height: tp.height + 'px',
  }
})
const hoverLeft = computed(() => {
  const h = hover.value
  if (!h) return 0
  const half = (trick.value?.width || 60) / 2
  return Math.max(half, Math.min(h.x, h.w - half))
})

function onKey(e) {
  if (e.target.tagName === 'INPUT' || e.ctrlKey || e.metaKey || e.altKey) return
  const v = video.value
  if (!v) return
  switch (e.key) {
    case ' ':
    case 'k':
      toggle()
      break
    case 'ArrowLeft':
    case 'j':
      skip(e.shiftKey ? -60 : -10)
      break
    case 'ArrowRight':
    case 'l':
      skip(e.shiftKey ? 60 : 10)
      break
    case 'ArrowUp':
      setVolume(volume.value + 0.1)
      showFlash('音量 ' + Math.round(volume.value * 100) + '%')
      break
    case 'ArrowDown':
      setVolume(volume.value - 0.1)
      showFlash('音量 ' + Math.round(volume.value * 100) + '%')
      break
    case 'm':
      toggleMute()
      showFlash(muted.value ? '静音' : '取消静音')
      break
    case 'f':
      toggleFullscreen()
      break
    case 'r':
    case 'R':
      rotate(e.shiftKey ? -1 : 1)
      break
    case 'c':
      if (data.value?.subtitles.length) setSub(subIndex.value + 1 >= data.value.subtitles.length ? -1 : subIndex.value + 1)
      break
    case 'Escape':
      if (menu.value) menu.value = ''
      else if (!document.fullscreenElement) back()
      break
    default:
      if (/^[0-9]$/.test(e.key)) seek((Number(e.key) / 10) * dur.value)
      else return
  }
  e.preventDefault()
  poke()
}

function back() {
  report()
  if (window.history.state?.back) router.back()
  else router.replace(`/item/${props.id}`)
}
function playNext() {
  clearInterval(countdownTimer)
  countdownTimer = null
  if (data.value?.next) router.replace({ path: `/play/${data.value.next.id}`, query: queue.value ? { queue: '1' } : {} })
}
function cancelNext() {
  clearInterval(countdownTimer)
  countdownTimer = null
  countdown.value = 0
}
function onUnload() {
  report(true)
}

onMounted(() => {
  load()
  document.addEventListener('keydown', onKey)
  document.addEventListener('fullscreenchange', onFsChange)
  window.addEventListener('pagehide', onUnload)
  reportTimer = setInterval(() => playing.value && report(), 15000)
})
onBeforeUnmount(() => {
  report(true)
  document.removeEventListener('keydown', onKey)
  document.removeEventListener('fullscreenchange', onFsChange)
  window.removeEventListener('pagehide', onUnload)
  clearInterval(reportTimer)
  clearInterval(countdownTimer)
  clearTimeout(hideTimer)
  clearTimeout(resumeTimer)
  clearTimeout(spriteTimer)
  spriteAbort?.abort()
  spriteStarted = true // 卸载后不再开始预载
  for (const u of spriteList) if (u) URL.revokeObjectURL(u)
  if (document.fullscreenElement) document.exitFullscreen().catch(() => {})
  if (video.value?.webkitDisplayingFullscreen) video.value.webkitExitFullscreen()
  leaveNativeFullscreen()
  document.title = prevTitle
})
watch(menu, (m) => m && (showUI.value = true))
</script>

<template>
  <div ref="root" class="player" :class="{ 'hide-ui': !showUI, mobile }" @mousemove="poke" @touchstart.passive="onTouch">
    <video
      v-if="data"
      ref="video"
      :class="{ sideways }"
      :style="{ '--rot': rot + 'deg' }"
      :src="data.video"
      playsinline
      preload="auto"
      @loadedmetadata="onLoaded"
      @timeupdate="onTime"
      @progress="onProgress"
      @play="onPlay"
      @pause="onPause"
      @ended="onEnded"
      @waiting="waiting = true"
      @playing="onPlayingOnce"
      @canplay="waiting = false"
      @error="onError"
      @click="onVideoClick"
      @dblclick="onVideoDblclick"
      @webkitbeginfullscreen="onWebkitBegin"
      @webkitendfullscreen="onWebkitEnd"
    >
      <track v-for="(s, i) in data.subtitles" :key="i" kind="subtitles" :src="s.src" :srclang="s.lang || 'und'" :label="s.label" />
    </video>

    <Transition name="fade">
    <div v-if="err" class="center-msg err">
      <p>{{ err }}</p>
      <button class="btn" @click="back">返回</button>
    </div>
    <div v-else-if="waiting && !ended" class="center-msg"><div class="spinner" /></div>
    </Transition>
    <Transition name="flash">
      <div v-if="flash" :key="flash" class="flash">{{ flash }}</div>
    </Transition>
    <Transition name="bubble">
      <div v-if="bubble" :key="bubble.key" class="bubble">
        <Icon :name="bubble.icon" :size="40" />
        <span v-if="bubble.text">{{ bubble.text }}</span>
      </div>
    </Transition>

    <Transition name="slide-up">
    <div v-if="ended && data?.next && countdown > 0" class="next-card">
      <div class="muted">{{ queue ? '随机播放 · 下一个' : '即将播放下一个' }}（{{ countdown }}）</div>
      <div class="next-title">{{ data.next.title }}</div>
      <div class="row">
        <button class="btn primary" @click="playNext"><Icon name="play" /> 立即播放</button>
        <button class="btn" @click="cancelNext">取消</button>
      </div>
      <div class="count-bar"><div :style="{ animationDuration: '10s' }" /></div>
    </div>
    </Transition>

    <Transition name="slide-up">
    <div v-if="resumed && !err && !ended" class="resume-chip">
      <span>已从 {{ fmtTime(resumed) }} 继续播放</span>
      <button class="btn" @click.stop="restart"><Icon name="replay" :size="18" /> 从头播放</button>
    </div>
    </Transition>

    <div class="top ui">
      <button class="icon-btn" title="返回 (Esc)" @click="back"><Icon name="back" /></button>
      <div class="ptitle">
        <div>{{ data?.item.title }}</div>
        <div v-if="data?.item.code && data.item.code !== data.item.title" class="muted small">{{ data.item.code }}</div>
      </div>
      <div class="spacer" />
      <div v-if="queue" class="qinfo" :title="queue.label"><Icon name="shuffle" :size="16" /> {{ queue.label }} · {{ queue.index + 1 }}/{{ queue.total }}</div>
    </div>

    <div class="bottom ui" @click.stop>
      <!-- 移动端：时间放在进度条上方、左对齐，按钮整行居中 -->
      <div v-if="mobile" class="m-time">{{ fmtTime(cur) }} / {{ fmtTime(dur) }}</div>
      <div ref="bar" class="bar" @pointerdown="onBarDown" @pointermove="onBarMove" @pointerleave="hover = null">
        <div class="track">
          <div v-for="(b, i) in buffered" :key="i" class="buf" :style="{ left: (b[0] / dur) * 100 + '%', width: ((b[1] - b[0]) / dur) * 100 + '%' }" />
          <div class="fill" :style="{ width: pct + '%' }" />
          <div class="knob" :style="{ left: pct + '%' }" />
        </div>
        <Transition name="tip">
        <div v-if="hover" class="tip" :style="{ left: hoverLeft + 'px' }">
          <div v-if="preview" class="thumb" :style="preview" />
          <div class="tip-time">{{ fmtTime(hover.t) }}</div>
        </div>
        </Transition>
      </div>

      <div class="controls">
        <button class="icon-btn" title="后退 10 秒 (←)" @click="skip(-10)"><Icon name="rewind" /></button>
        <button class="icon-btn big" :title="playing ? '暂停 (空格)' : '播放 (空格)'" @click="toggle">
          <Transition name="swap" mode="out-in"><Icon :key="playing ? 'p' : 's'" :name="playing ? 'pause' : 'play'" :size="34" /></Transition>
        </button>
        <button class="icon-btn" title="前进 10 秒 (→)" @click="skip(10)"><Icon name="forward" /></button>
        <button v-if="data?.next" class="icon-btn" :title="'下一个：' + data.next.title" @click="playNext"><Icon name="next" /></button>
        <template v-if="!mobile">
          <div class="time">{{ fmtTime(cur) }} / {{ fmtTime(dur) }}</div>
          <div class="spacer" />
        </template>
        <!-- 移动端用系统音量键调音量，只在被静音（例如自动播放被限制）时显示取消静音的按钮 -->
        <div v-if="!mobile || muted" class="vol">
          <button class="icon-btn" title="静音 (M)" @click="toggleMute()"><Icon :name="muted || volume === 0 ? 'mute' : 'volume'" /></button>
          <input v-if="!mobile" type="range" min="0" max="1" step="0.05" :value="muted ? 0 : volume" @input="setVolume(Number($event.target.value))" />
        </div>
        <div v-if="data?.subtitles.length" class="pop">
          <button class="icon-btn" :class="{ on: subIndex >= 0 }" title="字幕 (C)" @click="menu = menu === 'subs' ? '' : 'subs'"><Icon name="subtitles" /></button>
          <Transition name="pop"><div v-if="menu === 'subs'" class="menu up">
            <button :class="{ sel: subIndex < 0 }" @click="setSub(-1)">关闭字幕</button>
            <button v-for="(s, i) in data.subtitles" :key="i" :class="{ sel: subIndex === i }" @click="setSub(i)">{{ s.label }}</button>
          </div></Transition>
        </div>
        <div class="pop">
          <button class="icon-btn" title="播放速度" @click="menu = menu === 'speed' ? '' : 'speed'"><Icon name="speed" /></button>
          <Transition name="pop"><div v-if="menu === 'speed'" class="menu up">
            <button v-for="r in [0.5, 0.75, 1, 1.25, 1.5, 2]" :key="r" :class="{ sel: rate === r }" @click="setRate(r)">{{ r }}×</button>
          </div></Transition>
        </div>
        <button class="icon-btn" :class="{ on: rot % 360 !== 0 }" title="旋转画面 (R，Shift+R 逆时针)" @click="rotate(1)">
          <Icon name="rotate" class="rot-icon" :style="{ '--rot': rot + 'deg' }" />
        </button>
        <button class="icon-btn" title="全屏 (F)" @click="toggleFullscreen"><Icon :name="fullscreen ? 'exitFullscreen' : 'fullscreen'" /></button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.player { position: fixed; inset: 0; background: #000; overflow: hidden; user-select: none; }
.player.hide-ui { cursor: none; }
/* 视频始终居中；横躺（90° / 270°）时宽高对调，旋转后正好铺满屏幕。宽高和角度一起按弹簧曲线过渡 */
video { position: absolute; left: 50%; top: 50%; width: 100vw; height: 100vh; height: 100dvh; display: block; background: #000;
  transform: translate(-50%, -50%) rotate(var(--rot, 0deg));
  transition: transform .6s var(--spring), width .6s var(--spring), height .6s var(--spring); }
video.sideways { width: 100vh; width: 100dvh; height: 100vw; }
.rot-icon { transform: rotate(var(--rot, 0deg)); transition: transform .6s var(--spring-bouncy); }
video::cue { background: rgba(0,0,0,.55); font-size: 1.15em; }
.ui { position: absolute; left: 0; right: 0; transition: opacity .45s var(--ease-out), transform .6s var(--spring); z-index: 2; }
.hide-ui .ui { opacity: 0; pointer-events: none; }
.hide-ui .top { transform: translate3d(0, -12px, 0); }
.hide-ui .bottom { transform: translate3d(0, 12px, 0); }
.top { top: 0; display: flex; align-items: center; gap: 10px; padding: 14px 18px 40px; background: linear-gradient(rgba(0,0,0,.75), transparent); }
.ptitle { font-size: 18px; }
.small { font-size: 12px; }
.bottom { bottom: 0; padding: 40px 22px 14px; background: linear-gradient(transparent, rgba(0,0,0,.85)); }
.bar { position: relative; height: 22px; display: flex; align-items: center; cursor: pointer; touch-action: none; }
.track { position: relative; width: 100%; height: 4px; background: rgba(255,255,255,.2); border-radius: 3px; transition: height .3s var(--spring); }
.bar:hover .track { height: 6px; }
.buf { position: absolute; top: 0; bottom: 0; background: rgba(255,255,255,.35); }
.fill { position: absolute; left: 0; top: 0; bottom: 0; background: var(--accent); border-radius: 2px; }
.knob { position: absolute; top: 50%; width: 14px; height: 14px; margin: -7px 0 0 -7px; border-radius: 50%; background: #fff; transform: scale(0);
  transition: transform .4s var(--spring-bouncy); box-shadow: 0 1px 6px rgba(0,0,0,.4); }
.bar:active .knob { transform: scale(1.25); }
.buf { transition: width .4s var(--ease-out); }
.bar:hover .knob { transform: scale(1); }
.tip { position: absolute; bottom: 26px; transform: translateX(-50%); display: grid; justify-items: center; gap: 4px; pointer-events: none; }
.thumb { border: 2px solid rgba(255,255,255,.9); border-radius: 8px; background-color: #000; box-shadow: 0 8px 24px rgba(0,0,0,.6); }
.tip-enter-active { transition: opacity .2s var(--ease-out), transform .4s var(--spring-bouncy); }
.tip-leave-active { transition: opacity .12s, transform .12s; }
.tip-enter-from, .tip-leave-to { opacity: 0; transform: translateX(-50%) translateY(8px) scale(.92); }
.tip-time { padding: 2px 10px; background: rgba(20,20,20,.75); border-radius: 999px; font-size: 13px; backdrop-filter: blur(10px); }
.controls { display: flex; align-items: center; gap: 4px; margin-top: 4px; }
.big { width: 52px; height: 52px; }
.time { margin-left: 10px; font-variant-numeric: tabular-nums; font-size: 14px; }
.vol { display: flex; align-items: center; }
.vol input { width: 90px; accent-color: var(--accent); }
.pop { position: relative; }
.menu.up { bottom: 48px; right: 0; top: auto; min-width: 150px; }
.menu .sel { color: var(--accent); }
.on { color: var(--accent); }
.center-msg { position: absolute; inset: 0; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 16px; pointer-events: none; z-index: 1; }
.center-msg.err { pointer-events: auto; background: rgba(0,0,0,.7); text-align: center; padding: 20px; }
.flash { position: absolute; top: 18%; left: 50%; transform: translate(-50%, -50%); padding: 10px 22px; border-radius: 999px; background: rgba(30,30,30,.6);
  font-size: 17px; z-index: 3; pointer-events: none; backdrop-filter: saturate(180%) blur(20px); }
.flash-enter-active { transition: opacity .2s, transform .45s var(--spring-bouncy); }
.flash-leave-active { transition: opacity .3s, transform .3s; }
.flash-enter-from { opacity: 0; transform: translate(-50%, -50%) scale(.8); }
.flash-leave-to { opacity: 0; transform: translate(-50%, -60%); }
/* 中央反馈气泡：弹出后放大淡出 */
.bubble { position: absolute; top: 50%; left: 50%; width: 92px; height: 92px; margin: -46px 0 0 -46px; border-radius: 50%; z-index: 3; pointer-events: none;
  display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 2px; font-size: 12px;
  background: rgba(30,30,30,.5); backdrop-filter: saturate(180%) blur(16px); -webkit-backdrop-filter: saturate(180%) blur(16px); }
.bubble-enter-active { transition: opacity .15s, transform .4s var(--spring-bouncy); }
.bubble-leave-active { transition: opacity .45s var(--ease-out), transform .55s var(--ease-out); }
.bubble-enter-from { opacity: 0; transform: scale(.6); }
.bubble-leave-to { opacity: 0; transform: scale(1.35); }
.swap-enter-active { transition: opacity .15s, transform .35s var(--spring-bouncy); }
.swap-leave-active { transition: opacity .1s, transform .1s; }
.swap-enter-from { opacity: 0; transform: scale(.5); }
.swap-leave-to { opacity: 0; transform: scale(.5); }
.count-bar { height: 3px; border-radius: 2px; background: rgba(255,255,255,.15); overflow: hidden; }
.count-bar div { height: 100%; background: var(--accent); transform-origin: left; animation: count-down linear forwards; }
@keyframes count-down { from { transform: scaleX(1); } to { transform: scaleX(0); } }
.next-card { position: absolute; right: 30px; bottom: 120px; z-index: 3; background: rgba(30,30,30,.72); padding: 18px 20px; border-radius: 16px; display: grid; gap: 10px;
  max-width: 360px; backdrop-filter: saturate(180%) blur(24px); -webkit-backdrop-filter: saturate(180%) blur(24px); box-shadow: 0 20px 50px rgba(0,0,0,.5); }
.next-title { font-size: 17px; }
.resume-chip { position: absolute; left: 22px; bottom: 120px; z-index: 3; display: flex; align-items: center; gap: 14px; padding: 8px 8px 8px 18px;
  border-radius: 999px; background: rgba(30,30,30,.66); box-shadow: 0 12px 36px rgba(0,0,0,.45);
  backdrop-filter: saturate(180%) blur(24px); -webkit-backdrop-filter: saturate(180%) blur(24px); }
.resume-chip .btn { border-radius: 999px; height: 32px; padding: 0 14px; }
.qinfo { display: inline-flex; align-items: center; gap: 6px; padding: 5px 12px; border-radius: 999px; font-size: 13px; color: #ddd; max-width: 40vw;
  white-space: nowrap; overflow: hidden; text-overflow: ellipsis; background: rgba(255,255,255,.1); backdrop-filter: blur(16px); -webkit-backdrop-filter: blur(16px); }
@media (max-width: 600px) { .vol input { display: none; } .bottom { padding: 30px 10px 8px; } }
/* 移动端布局：时间在进度条上方左对齐，控制按钮在底部居中 */
.m-time { padding: 0 2px; font-variant-numeric: tabular-nums; font-size: 13px; color: rgba(255,255,255,.9); }
.mobile .bottom { padding-bottom: max(8px, env(safe-area-inset-bottom)); }
.mobile .controls { justify-content: center; flex-wrap: wrap; gap: 2px; margin-top: 0; }
.mobile .controls .icon-btn:not(.big) { width: 38px; height: 38px; }
.mobile .resume-chip { left: 12px; bottom: 150px; }
.mobile .next-card { left: 12px; right: 12px; bottom: 150px; max-width: none; }
</style>
