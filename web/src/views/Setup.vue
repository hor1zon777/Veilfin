<script setup>
// 首次设置向导：欢迎 → 创建管理员 → 服务器设置 → 添加媒体库 → 完成。
// 初始化不需要代码，但只能在服务端启动后的时间窗口（默认 30 分钟）内完成。
import { ref, computed, onMounted, onBeforeUnmount } from 'vue'
import { useRouter } from 'vue-router'
import { state, rpc, toast, toastError } from '../store.js'
import { kindNames, scraperNames } from '../util.js'
import Icon from '../components/Icon.vue'
import LibraryForm from '../components/LibraryForm.vue'

const router = useRouter()
const STEPS = ['欢迎', '管理员', '服务器', '媒体库', '完成']
// 已经初始化过（管理员重新打开向导，或创建管理员后刷新了页面）就从“服务器”这一步开始
const step = ref(state.needsSetup ? 0 : 2)
const dir = ref('fwd')
const busy = ref(false)
const err = ref('')

function go(n) {
  dir.value = n > step.value ? 'fwd' : 'back'
  err.value = ''
  step.value = n
  if (n === 2) loadSettings()
  if (n === 3) loadLibs()
}

// ---- 初始化时间窗口倒计时 ----
const remain = ref(state.setupRemaining)
const expired = computed(() => state.needsSetup && remain.value <= 0)
const remainText = computed(() => {
  const s = Math.max(0, remain.value)
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`
})
let timer = null
onMounted(async () => {
  if (state.needsSetup) {
    try {
      remain.value = (await rpc('sys.info')).setupRemaining || 0
    } catch {}
    timer = setInterval(() => remain.value > 0 && remain.value--, 1000)
  } else {
    go(2)
  }
})
onBeforeUnmount(() => clearInterval(timer))

// ---- 第 2 步：管理员 ----
const username = ref('admin')
const password = ref('')
const password2 = ref('')
async function createAdmin() {
  err.value = ''
  if (password.value !== password2.value) {
    err.value = '两次输入的密码不一致'
    return
  }
  busy.value = true
  try {
    const r = await rpc('setup.create', { username: username.value, password: password.value })
    state.me = r.me || r
    state.needsSetup = false
    clearInterval(timer)
    password.value = password2.value = ''
    go(2)
  } catch (e) {
    err.value = e.message
  } finally {
    busy.value = false
  }
}

// ---- 第 3 步：服务器设置 ----
const settings = ref(null)
const ffmpeg = ref(true)
const showScrape = ref(false)
async function loadSettings() {
  if (settings.value) return
  try {
    const r = await rpc('admin.settings.get')
    settings.value = r.settings
    ffmpeg.value = r.ffmpeg
  } catch (e) {
    toastError(e)
  }
}
async function saveSettings() {
  busy.value = true
  try {
    settings.value = await rpc('admin.settings.set', settings.value)
    state.serverName = settings.value.serverName
    go(3)
  } catch (e) {
    err.value = e.message
  } finally {
    busy.value = false
  }
}

// ---- 第 4 步：媒体库 ----
const libs = ref([])
const adding = ref(false)
const newLib = ref(null)
function resetLib() {
  newLib.value = { id: 0, name: '自制视频', kind: 'home', restricted: false, scraper: 'none', paths: [''], sortOrder: libs.value.length }
}
async function loadLibs() {
  try {
    libs.value = await rpc('admin.libs.list')
  } catch (e) {
    toastError(e)
  }
  resetLib()
  adding.value = !libs.value.length
}
async function addLib() {
  const l = { ...newLib.value, paths: newLib.value.paths.filter((p) => p.trim()) }
  if (!l.paths.length) {
    err.value = '请至少选择一个目录'
    return
  }
  busy.value = true
  err.value = ''
  try {
    await rpc('admin.libs.create', l)
    toast(`已添加「${l.name}」，正在后台扫描`, 'ok')
    libs.value = await rpc('admin.libs.list')
    resetLib()
    adding.value = false
  } catch (e) {
    err.value = e.message
  } finally {
    busy.value = false
  }
}

function finish() {
  router.replace('/')
}
</script>

<template>
  <div class="auth-page">
    <div class="wizard">
      <div class="brand"><Icon name="play" :size="30" class="logo" /> Veilfin <span class="muted sub">设置向导</span></div>

      <!-- 步骤条：进度线按弹簧曲线伸长，当前步骤的圆点放大 -->
      <ol class="steps">
        <span class="line"><span class="fill" :style="{ transform: `scaleX(${step / (STEPS.length - 1)})` }" /></span>
        <li v-for="(s, i) in STEPS" :key="s" :class="{ done: i < step, cur: i === step }">
          <span class="dot"><Icon v-if="i < step" name="check" :size="14" /><template v-else>{{ i + 1 }}</template></span>
          <span class="lbl">{{ s }}</span>
        </li>
      </ol>

      <Transition :name="'step-' + dir" mode="out-in">
        <!-- 欢迎 -->
        <section v-if="step === 0" key="0" class="pane">
          <h2>欢迎使用 Veilfin（帷影）</h2>
          <p>这是一个带端到端加密的个人视频服务：请求、视频、图片和字幕在传输时全部加密，CDN 和中间人只能看到不透明的密文。</p>
          <p class="muted">接下来几步会创建管理员账号、填写基本设置并添加媒体库，之后都可以在管理后台修改。</p>
          <div class="window" :class="{ bad: expired }">
            <template v-if="expired">
              <b>初始化时间窗口已过期。</b>为防止别人抢先完成初始化，服务端只在启动后的 30 分钟内允许初始化。请重启服务端，然后刷新本页。
            </template>
            <template v-else>
              为防止别人抢先完成初始化，请在 <b class="countdown">{{ remainText }}</b> 内完成管理员创建（超时后重启服务端即可重新计时）。
            </template>
          </div>
          <div class="nav">
            <span />
            <button id="start" class="btn primary big" :disabled="expired" @click="go(1)">开始设置 <Icon name="chevronRight" :size="20" /></button>
          </div>
        </section>

        <!-- 管理员 -->
        <form v-else-if="step === 1" key="1" class="pane" @submit.prevent="createAdmin">
          <h2>创建管理员账号</h2>
          <p class="muted">管理员可以管理用户、媒体库和服务器设置。请使用足够长的密码。</p>
          <div class="field">
            <label for="u">用户名</label>
            <input id="u" v-model="username" class="input" autocomplete="username" required />
          </div>
          <div class="field">
            <label for="p">密码（至少 8 位）</label>
            <input id="p" v-model="password" class="input" type="password" autocomplete="new-password" minlength="8" required />
          </div>
          <div class="field">
            <label for="p2">确认密码</label>
            <input id="p2" v-model="password2" class="input" type="password" autocomplete="new-password" required />
          </div>
          <p v-if="!expired" class="hint">剩余初始化时间 <span class="countdown">{{ remainText }}</span></p>
          <p v-else class="err">初始化时间窗口已过期，请重启服务端后刷新本页。</p>
          <div class="err">{{ err }}</div>
          <div class="nav">
            <button type="button" class="btn ghost" @click="go(0)">上一步</button>
            <button class="btn primary big" type="submit" :disabled="busy || expired">{{ busy ? '创建中…' : '创建并继续' }}</button>
          </div>
        </form>

        <!-- 服务器设置 -->
        <form v-else-if="step === 2" key="2" class="pane" @submit.prevent="saveSettings">
          <h2>服务器设置</h2>
          <p v-if="!ffmpeg" class="warn">服务端没有找到 ffmpeg / ffprobe：视频可以播放，但不会生成截图、字幕和进度条预览图。建议安装 ffmpeg，或启动时用 -ffmpeg 指定目录。</p>
          <template v-if="settings">
            <div class="field">
              <label for="sn">服务器名称</label>
              <input id="sn" v-model="settings.serverName" class="input" placeholder="Veilfin" />
              <span class="hint">显示在页面标题和登录页上。</span>
            </div>
            <button type="button" class="fold" :class="{ open: showScrape }" @click="showScrape = !showScrape">
              <Icon name="chevronRight" :size="18" /> 在线刮削（可选）
            </button>
            <div class="fold-wrap" :class="{ open: showScrape }">
              <div class="fold-clip">
                <p class="hint">自制视频通常不需要。普通电影用 TMDB，日本影片用 MetaTube 按番号刮削。</p>
                <div class="field"><label>TMDB API Key 或读访问令牌</label><input v-model="settings.tmdbKey" class="input" type="password" autocomplete="off" /></div>
                <div class="field"><label>MetaTube 服务地址</label><input v-model="settings.metatubeUrl" class="input" placeholder="http://127.0.0.1:8080" /></div>
                <div class="field"><label>MetaTube 访问令牌（可选）</label><input v-model="settings.metatubeToken" class="input" type="password" autocomplete="off" /></div>
                <div class="field"><label>刮削代理（可选）</label><input v-model="settings.scraperProxy" class="input" placeholder="http://127.0.0.1:7890 或 socks5://127.0.0.1:1080" /></div>
              </div>
            </div>
          </template>
          <div v-else class="loading"><div class="spinner" /></div>
          <div class="err">{{ err }}</div>
          <div class="nav">
            <button type="button" class="btn ghost" @click="go(3)">跳过</button>
            <button id="srv-next" class="btn primary big" type="submit" :disabled="busy || !settings">保存并继续</button>
          </div>
        </form>

        <!-- 媒体库 -->
        <section v-else-if="step === 3" key="3" class="pane">
          <h2>添加媒体库</h2>
          <p class="muted">媒体库是服务器上存放视频的目录（支持 mp4 / m4v / mov / webm）。扫描在后台进行，不需要等待。</p>
          <TransitionGroup v-if="libs.length" tag="div" name="lib" class="libs">
            <div v-for="l in libs" :key="l.id" class="lib">
              <Icon name="library" :size="20" class="li" />
              <div class="lmain">
                <div>{{ l.name }} <Icon v-if="l.restricted" name="lock" :size="14" class="muted" /></div>
                <div class="muted small">{{ kindNames[l.kind] }} · {{ scraperNames[l.scraper] }} · {{ l.paths.join('、') }}</div>
              </div>
            </div>
          </TransitionGroup>
          <div v-if="adding && newLib" class="addbox">
            <LibraryForm :lib="newLib" @submit="addLib" />
            <div class="row end">
              <button v-if="libs.length" type="button" class="btn ghost" @click="adding = false">取消</button>
              <button id="lib-add" class="btn primary" :disabled="busy" @click="addLib">{{ busy ? '添加中…' : '添加此媒体库' }}</button>
            </div>
          </div>
          <button v-else class="btn" @click="adding = true">+ 再添加一个媒体库</button>
          <div class="err">{{ err }}</div>
          <div class="nav">
            <button type="button" class="btn ghost" @click="go(2)">上一步</button>
            <button id="libs-next" class="btn primary big" @click="go(4)">{{ libs.length ? '下一步' : '跳过，稍后添加' }}</button>
          </div>
        </section>

        <!-- 完成 -->
        <section v-else key="4" class="pane done">
          <div class="big-check"><Icon name="check" :size="44" /></div>
          <h2>设置完成</h2>
          <p class="muted">管理员「{{ state.me?.username }}」已登录，{{ libs.length ? `${libs.length} 个媒体库正在后台扫描。` : '还没有添加媒体库，可以稍后在管理后台添加。' }}</p>
          <ul class="tips">
            <li>到「设置」页核对服务端公钥指纹，和服务端启动日志里打印的一致，才说明连接没有被中间人替换。</li>
            <li>在「管理后台」可以添加用户、调整媒体库和服务器设置；日本影片类的媒体库默认受限，可以为账号设置 PIN。</li>
            <li>对隐私要求更高时，推荐在自己电脑上运行本地客户端（<code>veilfin client</code>），前端代码就不经过 CDN。</li>
          </ul>
          <div class="nav">
            <button type="button" class="btn ghost" @click="go(3)">上一步</button>
            <button id="finish" class="btn primary big" @click="finish">开始使用 <Icon name="chevronRight" :size="20" /></button>
          </div>
        </section>
      </Transition>
    </div>
  </div>
</template>

<style scoped>
.wizard { width: 640px; max-width: 100%; background: rgba(32,32,32,.82); padding: 28px 32px 26px; border-radius: 22px; box-shadow: 0 30px 80px rgba(0,0,0,.5);
  border: 0.5px solid rgba(255,255,255,.08); backdrop-filter: saturate(180%) blur(30px); -webkit-backdrop-filter: saturate(180%) blur(30px);
  animation: sheet-in .7s var(--spring-bouncy) backwards; }
.brand { display: flex; align-items: center; gap: 8px; font-size: 22px; font-weight: 600; margin-bottom: 22px; }
.brand .sub { font-size: 14px; font-weight: normal; margin-left: 4px; }
.logo { color: var(--accent); animation: logo-pop .8s var(--spring-bouncy) .15s backwards; }

.steps { position: relative; display: flex; justify-content: space-between; list-style: none; margin: 0 0 26px; padding: 0; }
.steps .line { position: absolute; left: 14px; right: 14px; top: 13px; height: 2px; background: rgba(255,255,255,.12); border-radius: 1px; }
.steps .fill { position: absolute; inset: 0; background: var(--accent); transform-origin: left; transition: transform .7s var(--spring); }
.steps li { position: relative; display: flex; flex-direction: column; align-items: center; gap: 6px; width: 28px; }
.dot { width: 28px; height: 28px; border-radius: 50%; display: flex; align-items: center; justify-content: center; font-size: 13px; color: var(--muted);
  background: #2c2c2c; border: 2px solid rgba(255,255,255,.14);
  transition: background-color .35s var(--ease-out), border-color .35s, color .35s, transform .5s var(--spring-bouncy), box-shadow .35s; }
.done .dot { background: var(--accent); border-color: var(--accent); color: #fff; }
.cur .dot { border-color: var(--accent); color: #fff; transform: scale(1.18); box-shadow: 0 0 0 5px rgba(82,181,75,.18); }
.lbl { font-size: 12px; color: var(--muted); white-space: nowrap; transition: color .3s; }
.cur .lbl, .done .lbl { color: #ddd; }

.pane h2 { font-size: 20px; margin-bottom: 10px; }
.pane > p { margin: 0 0 12px; line-height: 1.7; }
.window { margin: 18px 0 4px; padding: 12px 14px; border-radius: 12px; background: rgba(82,181,75,.1); border: 0.5px solid rgba(82,181,75,.3); line-height: 1.7; font-size: 14px; }
.window.bad { background: rgba(200,70,70,.12); border-color: rgba(200,70,70,.4); }
.countdown { font-variant-numeric: tabular-nums; color: var(--accent); }
.nav { display: flex; justify-content: space-between; align-items: center; margin-top: 22px; }
.err { color: var(--err); min-height: 1em; }
.warn { color: var(--warn); }
.small { font-size: 12.5px; }

/* 可折叠区块：grid 行高从 0fr 过渡到 1fr */
.fold { display: inline-flex; align-items: center; gap: 4px; color: var(--accent); margin: 4px 0 8px; }
.fold .icon { transition: transform .45s var(--spring-bouncy); }
.fold.open .icon { transform: rotate(90deg); }
.fold-wrap { display: grid; grid-template-rows: 0fr; opacity: 0; transition: grid-template-rows .55s var(--spring), opacity .3s var(--ease-out); }
.fold-wrap.open { grid-template-rows: 1fr; opacity: 1; }
.fold-clip { min-height: 0; overflow: hidden; }

.libs { display: grid; gap: 8px; margin-bottom: 12px; }
.lib { display: flex; gap: 12px; align-items: center; padding: 10px 14px; border-radius: 12px; background: rgba(255,255,255,.05); }
.li { color: var(--accent); flex-shrink: 0; }
.lmain { min-width: 0; }
.lmain .small { word-break: break-all; }
.lib-enter-active { transition: opacity .3s, transform .5s var(--spring-bouncy); }
.lib-enter-from { opacity: 0; transform: scale(.94) translateY(6px); }
.addbox { padding: 16px 16px 12px; border-radius: 14px; background: rgba(255,255,255,.035); border: 0.5px solid rgba(255,255,255,.08); margin-bottom: 8px; }
.end { justify-content: flex-end; }

.done { text-align: center; }
.big-check { width: 76px; height: 76px; margin: 4px auto 14px; border-radius: 50%; background: var(--accent); color: #fff; display: flex; align-items: center; justify-content: center;
  box-shadow: 0 12px 36px rgba(82,181,75,.35); animation: logo-pop .7s var(--spring-bouncy) .1s backwards; }
.tips { text-align: left; margin: 16px 0 0; padding-left: 20px; display: grid; gap: 8px; color: #ccc; font-size: 13.5px; line-height: 1.7; }
.done .nav { text-align: left; }

/* 步骤切换：前进时从右侧推入，后退时从左侧推入 */
.step-fwd-enter-active, .step-back-enter-active { transition: opacity .3s var(--ease-out), transform .55s var(--spring); }
.step-fwd-leave-active, .step-back-leave-active { transition: opacity .15s, transform .2s var(--ease-out); }
.step-fwd-enter-from { opacity: 0; transform: translate3d(32px, 0, 0); }
.step-fwd-leave-to { opacity: 0; transform: translate3d(-20px, 0, 0); }
.step-back-enter-from { opacity: 0; transform: translate3d(-32px, 0, 0); }
.step-back-leave-to { opacity: 0; transform: translate3d(20px, 0, 0); }
@media (max-width: 560px) {
  .wizard { padding: 22px 18px; }
  .lbl { display: none; }
}
</style>
