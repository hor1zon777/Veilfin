<script setup>
import { ref, computed, onMounted, onBeforeUnmount } from 'vue'
import { rpc, toast, toastError } from '../../store.js'
import { fmtSize, fmtDateTime } from '../../util.js'
import Modal from '../../components/Modal.vue'
import Icon from '../../components/Icon.vue'

const st = ref(null)
const pre = ref(false)
const checking = ref(false)
const confirm = ref(false)
const busy = ref(false)
const restarting = ref(null) // { status, failed }

let timer = 0
let alive = true

const job = computed(() => st.value?.job)
const running = computed(() => !!job.value?.running)
const latest = computed(() => st.value?.latest)
const canApply = computed(() => latest.value?.newer && latest.value.asset && !st.value.reason && !running.value)

const phaseText = {
  download: '正在下载',
  verify: '正在校验新程序',
  install: '正在替换程序文件',
  wait: '等待备份任务结束',
  restart: '正在重启',
}
const pct = computed(() => (job.value?.total ? Math.min(100, Math.round((job.value.done / job.value.total) * 100)) : 0))

// 只显示发布说明里的“变更”部分（前面是下载表格，与这里无关）
const notes = computed(() => {
  const body = latest.value?.body || ''
  const parts = body.split(/^##\s*变更\s*$/m)
  return (parts.length > 1 ? parts[1] : body).trim()
})

async function load() {
  try {
    const prev = job.value
    st.value = await rpc('admin.update.status')
    if (st.value.prerelease) pre.value = true
    const j = st.value.job
    if (prev?.running && j && !j.running && j.err) toastError(new Error('更新失败：' + j.err))
    if (j?.running && j.phase === 'restart') return waitRestart()
  } catch (e) {
    toastError(e)
  }
  clearTimeout(timer)
  if (alive && running.value) timer = setTimeout(load, 800)
}
onMounted(load)
onBeforeUnmount(() => {
  alive = false
  clearTimeout(timer)
})

async function check() {
  checking.value = true
  try {
    st.value = await rpc('admin.update.check', { prerelease: pre.value })
    if (!st.value.latest) toast('没有找到可用的发布版本', 'info')
    else if (!st.value.latest.newer) toast('已经是最新版本', 'ok')
  } catch (e) {
    toastError(e)
  } finally {
    checking.value = false
  }
}

async function apply() {
  busy.value = true
  try {
    await rpc('admin.update.apply', { tag: latest.value.tag })
    confirm.value = false
    await load()
  } catch (e) {
    toastError(e)
  } finally {
    busy.value = false
  }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

// 等服务端用新程序重启：先等它断开，再等它以新版本回来，然后刷新页面（网页模式下前端也随之更新）
async function waitRestart() {
  if (restarting.value) return
  const old = st.value.current
  restarting.value = { status: `已安装 ${job.value.tag}，服务端正在重启……`, failed: false }
  let wentDown = false
  await sleep(1500)
  for (let i = 0; i < 80 && alive; i++) {
    try {
      const info = await rpc('sys.info')
      if (info.version && info.version !== old) {
        restarting.value.status = `已更新到 ${info.version}，正在刷新页面……`
        await sleep(800)
        location.reload()
        return
      }
      if (wentDown) {
        restarting.value = { status: '服务端已经重启，但版本没有变化，新程序可能没能启动。请查看服务端日志。', failed: true }
        return
      }
    } catch {
      wentDown = true
    }
    await sleep(1500)
  }
  if (alive) restarting.value = { status: '等待服务端重启超时，请查看服务端日志。', failed: true }
}
</script>

<template>
  <div class="admin-body update">
    <section v-if="st" class="card-panel d1">
      <h2>当前版本</h2>
      <div class="cur">
        <span class="ver">{{ st.current }}</span>
        <span class="muted small">{{ st.platform }}{{ st.embeddedFFmpeg ? ' · 内置 ffmpeg' : '' }}</span>
      </div>
      <p v-if="!st.currentKnown" class="hint">当前是开发版本或本地构建，无法与发布版本比较，检查到的最新发布都会被视为可更新。</p>
      <div class="row ops">
        <button class="btn primary" :disabled="checking || running" @click="check">
          <Icon name="refresh" :size="18" :class="{ spinning: checking }" /> {{ checking ? '正在检查……' : '检查更新' }}
        </button>
        <label class="check"><input v-model="pre" type="checkbox" /> 包含预发布版本</label>
        <div class="spacer" />
        <span v-if="st.checkedAt" class="muted small">上次检查：{{ fmtDateTime(st.checkedAt) }}</span>
      </div>
      <p v-if="st.checkErr" class="warn small mt">{{ st.checkErr }}</p>
    </section>

    <Transition name="slide-up">
      <section v-if="latest" :key="latest.tag" class="card-panel d2" :class="{ fresh: latest.newer }">
        <div class="row head">
          <h2>{{ latest.newer ? '发现新版本' : '已是最新版本' }}</h2>
          <span class="tag">{{ latest.tag }}</span>
          <span v-if="latest.prerelease" class="tag pre">预发布</span>
          <div class="spacer" />
          <span class="muted small">{{ fmtDateTime(latest.published) }} 发布</span>
        </div>
        <pre v-if="notes" class="notes">{{ notes }}</pre>
        <p class="muted small">
          <template v-if="latest.asset">本机将使用 <code>{{ latest.asset }}</code>（{{ fmtSize(latest.size) }}）。</template>
          <template v-else>这个版本没有适用于 {{ st.platform }} 的程序。</template>
          <a v-if="latest.url" :href="latest.url" target="_blank" rel="noopener noreferrer">在 GitHub 上查看</a>
        </p>

        <div v-if="job && (job.running || job.err) && job.tag === latest.tag" class="job">
          <template v-if="job.running">
            <div class="row">
              <span class="spin" />
              <span>{{ phaseText[job.phase] || job.phase }}{{ job.phase === 'download' && job.total ? `（${fmtSize(job.done)} / ${fmtSize(job.total)}）` : '' }}</span>
            </div>
            <div v-if="job.phase === 'download'" class="progress"><i :style="{ width: pct + '%' }" /></div>
          </template>
          <p v-else class="warn">上次更新失败：{{ job.err }}</p>
        </div>

        <p v-if="latest.newer && st.reason" class="warn reason">{{ st.reason }}</p>
        <div v-if="latest.newer" class="row ops">
          <div class="spacer" />
          <button class="btn primary" :disabled="!canApply" @click="confirm = true"><Icon name="download" :size="18" /> 下载并更新</button>
        </div>
      </section>
    </Transition>

    <section class="card-panel d3 about">
      <h2>说明</h2>
      <ul class="muted">
        <li>更新由服务端直接从 GitHub Release 下载，用发布附带的 <code>SHA256SUMS.txt</code> 校验，并试运行确认版本号后才会替换程序。</li>
        <li>旧程序会保留在同一目录（<code>{{ st?.oldExecutable || 'veilfin.old' }}</code>），新版本有问题时可以停止服务端后换回。</li>
        <li>访问 GitHub 使用「服务器设置 → 网络」中的代理（未设置时使用系统的 HTTPS_PROXY 环境变量）。</li>
        <li>本地客户端（<code>veilfin client</code>）不会随服务端更新，请自行下载新版本替换。</li>
      </ul>
    </section>

    <Modal v-if="confirm" title="更新服务端" @close="confirm = false">
      <p class="mb">将下载 {{ latest.tag }}（{{ fmtSize(latest.size) }}），校验通过后替换程序并自动重启服务端。</p>
      <p class="muted mb">重启需要几秒钟，期间所有设备会短暂断开，正在播放的视频可能需要重新打开。数据和密钥不受影响。建议先在「备份与恢复」中创建一份备份。</p>
      <template #footer>
        <button class="btn ghost" @click="confirm = false">取消</button>
        <button class="btn primary" :disabled="busy" @click="apply">开始更新</button>
      </template>
    </Modal>

    <Modal v-if="restarting" title="正在更新" @close="restarting.failed && (restarting = null)">
      <div class="restart">
        <span v-if="!restarting.failed" class="spin" />
        <p>{{ restarting.status }}</p>
      </div>
    </Modal>
  </div>
</template>

<style scoped>
.update { max-width: 860px; display: grid; gap: 14px; }
h2 { font-size: 16px; margin-bottom: 12px; }
.cur { display: flex; align-items: baseline; gap: 12px; flex-wrap: wrap; margin-bottom: 14px; }
.ver { font-size: 22px; font-weight: 600; font-family: Consolas, ui-monospace, monospace; }
.small { font-size: 12.5px; }
.mt { margin-top: 10px; }
.ops { gap: 12px; margin-top: 4px; }
.head { margin-bottom: 12px; }
.head h2 { margin: 0; }
.tag { padding: 2px 9px; border-radius: 999px; background: rgba(255, 255, 255, 0.08); font-size: 12.5px; font-family: Consolas, ui-monospace, monospace; }
.tag.pre { background: rgba(240, 198, 116, 0.18); color: var(--warn); font-family: inherit; }
.fresh { border: 1px solid color-mix(in srgb, var(--accent) 45%, transparent); }
.notes { white-space: pre-wrap; word-break: break-word; font: inherit; font-size: 13.5px; line-height: 1.7; max-height: 320px; overflow: auto; padding: 12px 14px; margin-bottom: 12px; border-radius: 8px; background: rgba(0, 0, 0, 0.18); }
.job { display: grid; gap: 8px; margin: 14px 0 4px; font-size: 13.5px; }
.warn { color: var(--warn); line-height: 1.6; }
.reason { margin-top: 12px; }
code { font-family: Consolas, ui-monospace, monospace; font-size: 12.5px; word-break: break-all; }
a { color: var(--accent); margin-left: 6px; }
.about ul { margin-left: 18px; display: grid; gap: 6px; line-height: 1.6; font-size: 13.5px; }
.mb { margin-bottom: 12px; line-height: 1.6; }
.restart { display: flex; align-items: center; gap: 12px; margin-bottom: 12px; line-height: 1.6; }
.spin { flex: none; width: 18px; height: 18px; border: 2px solid rgba(255, 255, 255, 0.15); border-top-color: var(--accent); border-radius: 50%; animation: spin 0.8s linear infinite; }
.spinning { animation: spin 0.8s linear infinite; }
</style>
