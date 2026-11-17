<script setup>
import { ref, computed, onMounted, onBeforeUnmount } from 'vue'
import { rpc, toast, toastError } from '../../store.js'
import { fmtSize, fmtDateTime, b64ToBytes, saveBlob, readFileBase64 } from '../../util.js'
import Modal from '../../components/Modal.vue'
import Icon from '../../components/Icon.vue'
import CopyField from '../../components/CopyField.vue'

// 与服务端 backupChunk 一致：每块 4 MiB
const CHUNK = 4 << 20

const files = ref([])
const dir = ref('')
const job = ref(null)
const staged = ref(null)
const canRestore = ref(true)
const loaded = ref(false)

const pass1 = ref('')
const pass2 = ref('')
const assets = ref(true)
const busy = ref(false)

const dl = ref(null) // { name, pct }
const up = ref(null) // { name, pct }
const del = ref(null)
const restoreFile = ref(null)
const restorePass = ref('')
const restarting = ref(null) // { keyChanged, publicKey, status, failed }

const running = computed(() => !!job.value?.running)
let timer = 0
let alive = true

async function load() {
  try {
    const r = await rpc('admin.backup.list')
    const prev = job.value
    files.value = r.files
    dir.value = r.dir
    job.value = r.job
    staged.value = r.staged
    canRestore.value = r.canRestore
    loaded.value = true
    // 任务刚结束时提示结果
    if (prev?.running && r.job && !r.job.running && prev.name === r.job.name) {
      if (r.job.err) toastError(new Error((r.job.kind === 'backup' ? '备份失败：' : '恢复失败：') + r.job.err))
      else if (r.job.kind === 'backup') toast('备份完成：' + r.job.name, 'ok')
    }
  } catch (e) {
    toastError(e)
  }
  clearTimeout(timer)
  if (alive && job.value?.running) timer = setTimeout(load, 1000)
}
onMounted(load)
onBeforeUnmount(() => {
  alive = false
  clearTimeout(timer)
})

async function create() {
  if (pass1.value !== pass2.value) {
    toastError(new Error('两次输入的口令不一致'))
    return
  }
  busy.value = true
  try {
    await rpc('admin.backup.create', { pass: pass1.value, assets: assets.value })
    pass1.value = pass2.value = ''
    toast('开始创建备份', 'ok')
    await load()
  } catch (e) {
    toastError(e)
  } finally {
    busy.value = false
  }
}

// 通过加密通道分块下载，拼好后交给浏览器保存
async function download(f) {
  dl.value = { name: f.name, pct: 0 }
  try {
    const parts = []
    let off = 0
    for (;;) {
      const r = await rpc('admin.backup.read', { name: f.name, offset: off })
      const bytes = b64ToBytes(r.data)
      parts.push(bytes)
      off += bytes.length
      dl.value.pct = r.size ? Math.round((off / r.size) * 100) : 100
      if (r.eof) break
      if (!bytes.length) throw new Error('下载中断')
    }
    saveBlob(new Blob(parts, { type: 'application/octet-stream' }), f.name)
  } catch (e) {
    toastError(e)
  } finally {
    dl.value = null
  }
}

async function upload(ev) {
  const file = ev.target.files?.[0]
  ev.target.value = ''
  if (!file) return
  // 先检查文件头，不是备份文件就不用传了
  const head = new Uint8Array(await file.slice(0, 6).arrayBuffer())
  if (new TextDecoder().decode(head) !== 'VFBK1\n') {
    toastError(new Error('这不是 Veilfin 备份文件（.vfbak）'))
    return
  }
  up.value = { name: file.name, pct: 0 }
  try {
    let id = ''
    let off = 0
    for (;;) {
      const end = Math.min(off + CHUNK, file.size)
      const done = end >= file.size
      const data = end > off ? await readFileBase64(file.slice(off, end)) : ''
      const r = await rpc('admin.backup.upload', { id, offset: off, data, done, filename: file.name })
      off = end
      up.value.pct = Math.round((off / file.size) * 100)
      if (done) {
        toast('已上传：' + r.name, 'ok')
        break
      }
      id = r.id
    }
    await load()
  } catch (e) {
    toastError(e)
  } finally {
    up.value = null
  }
}

async function doDelete() {
  try {
    await rpc('admin.backup.delete', { name: del.value.name })
    del.value = null
    load()
  } catch (e) {
    toastError(e)
  }
}

function askRestore(f) {
  restorePass.value = ''
  restoreFile.value = f
}

async function startRestore() {
  busy.value = true
  try {
    await rpc('admin.backup.restore', { name: restoreFile.value.name, pass: restorePass.value })
    restoreFile.value = null
    restorePass.value = ''
    await load()
  } catch (e) {
    toastError(e)
  } finally {
    busy.value = false
  }
}

async function cancelRestore() {
  try {
    await rpc('admin.backup.restore.cancel')
    staged.value = null
  } catch (e) {
    toastError(e)
  }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function applyRestore() {
  const st = staged.value
  busy.value = true
  try {
    await rpc('admin.backup.restore.apply')
  } catch (e) {
    toastError(e)
    busy.value = false
    return
  }
  restarting.value = { keyChanged: st.keyChanged, publicKey: st.publicKey, status: '服务端正在应用备份并重启……', failed: false }
  if (st.keyChanged) {
    // 密钥变了，当前连接不可能再握手成功，只能按提示重新信任
    restarting.value.status = '服务端正在用新的密钥重启。'
    return
  }
  // 等服务端重启完成，然后刷新页面（之前的登录状态已随数据库一起被替换，需要重新登录）
  await sleep(2000)
  for (let i = 0; i < 60; i++) {
    try {
      await rpc('sys.info')
      restarting.value.status = '恢复完成，正在刷新页面……'
      await sleep(600)
      location.reload()
      return
    } catch {
      await sleep(1500)
    }
  }
  restarting.value.status = '等待服务端重启超时，请查看服务端日志。'
  restarting.value.failed = true
}

const jobText = computed(() => {
  const j = job.value
  if (!j?.running) return ''
  if (j.kind === 'backup') return `正在创建备份 ${j.name}` + (j.written ? `（已写入 ${fmtSize(j.written)}）` : '')
  return `正在解密并校验备份 ${j.name}……`
})
</script>

<template>
  <div class="admin-body backup">
    <section class="card-panel d1">
      <h2>创建备份</h2>
      <p class="muted desc">
        备份包含服务端密钥、数据库（用户、媒体库、元数据、观看记录）以及可选的图片缓存，用口令加密（Argon2id + XChaCha20-Poly1305）。视频文件本身不在备份内。
      </p>
      <form class="row" @submit.prevent="create">
        <input v-model="pass1" class="input pw" type="password" placeholder="备份口令（至少 8 位）" autocomplete="new-password" minlength="8" required />
        <input v-model="pass2" class="input pw" type="password" placeholder="再输入一次" autocomplete="new-password" required />
        <label class="check"><input v-model="assets" type="checkbox" /> 包含图片 / 字幕 / 预览图缓存</label>
        <button class="btn primary" :disabled="busy || running">创建备份</button>
      </form>
      <p class="hint">口令丢失后备份无法解密，请妥善保管。不含图片缓存时，自动截图可以重新扫描生成，但手动设置的封面会丢失。</p>
    </section>

    <Transition name="slide-up">
      <section v-if="running" class="card-panel job">
        <span class="spin" />
        <span>{{ jobText }}</span>
      </section>
    </Transition>

    <Transition name="slide-up">
      <section v-if="staged && !restarting" class="card-panel staged">
        <h2>确认恢复</h2>
        <p>
          备份「{{ staged.name }}」已解密并校验通过{{ staged.hasAssets ? '，包含图片缓存' : '，不含图片缓存（恢复后会重新扫描生成截图，手动设置的封面会丢失）' }}。
        </p>
        <p v-if="staged.keyChanged" class="warn">
          备份中的服务端密钥与当前<b>不同</b>，恢复后服务端公钥会变成下面这个，所有客户端都需要重新信任：本地客户端要用新的 <code>-server-key</code> 重新启动；网页模式要在浏览器中清除本站数据后再访问。
        </p>
        <p v-else class="muted">备份中的服务端密钥与当前相同，客户端不需要改动。</p>
        <div class="field">
          <label>恢复后的服务端公钥</label>
          <CopyField :text="staged.publicKey" label="服务端公钥" />
        </div>
        <p class="muted">
          恢复会用备份<b>覆盖</b>当前的数据库和密钥：备份之后新增的用户、媒体库、观看记录和元数据修改都会丢失。服务端会在几秒内自动重启，所有设备会短暂断开（备份之后才登录的设备需要重新登录）。
        </p>
        <div class="row ops">
          <div class="spacer" />
          <button class="btn ghost" :disabled="busy" @click="cancelRestore">取消</button>
          <button class="btn danger" :disabled="busy" @click="applyRestore">确认恢复并重启</button>
        </div>
      </section>
    </Transition>

    <section class="card-panel d2">
      <div class="row head">
        <h2>备份文件</h2>
        <div class="spacer" />
        <label class="btn" :class="{ disabled: !!up }">
          <Icon name="upload" :size="18" /> 上传备份
          <input type="file" accept=".vfbak" class="offscreen" :disabled="!!up" @change="upload" />
        </label>
      </div>
      <Transition name="fade">
        <div v-if="up" class="transfer">
          <span class="muted">正在上传 {{ up.name }}（{{ up.pct }}%）</span>
          <div class="progress"><i :style="{ width: up.pct + '%' }" /></div>
        </div>
      </Transition>
      <p v-if="loaded && !files.length" class="muted empty">还没有备份。</p>
      <TransitionGroup name="list" tag="div" class="files">
        <div v-for="f in files" :key="f.name" class="file">
          <div class="meta">
            <span class="name">{{ f.name }}</span>
            <span class="muted small">{{ fmtSize(f.size) }} · {{ fmtDateTime(f.time) }}</span>
            <div v-if="dl?.name === f.name" class="progress"><i :style="{ width: dl.pct + '%' }" /></div>
          </div>
          <div class="row ops">
            <button class="btn" :disabled="!!dl" @click="download(f)"><Icon name="download" :size="18" /> {{ dl?.name === f.name ? dl.pct + '%' : '下载' }}</button>
            <button class="btn" :disabled="running || !canRestore || !!staged" @click="askRestore(f)"><Icon name="replay" :size="18" /> 恢复</button>
            <button class="btn ghost err" :disabled="running && job?.name === f.name" @click="del = f">删除</button>
          </div>
        </div>
      </TransitionGroup>
      <p class="hint foot">
        备份保存在服务端的 <code>{{ dir }}</code>，建议下载一份到其他地方保存。下载和上传都经过端到端加密通道。
        <template v-if="!canRestore">当前运行方式不支持在线恢复，请停止服务端后使用 <code>veilfin restore</code>。</template>
      </p>
    </section>

    <Modal v-if="restoreFile" title="从备份恢复" @close="restoreFile = null">
      <form @submit.prevent="startRestore">
        <p class="muted mb">输入「{{ restoreFile.name }}」的备份口令。服务端会先解密并校验备份，确认无误后再请你最终确认，这一步不会改动现有数据。</p>
        <input v-model="restorePass" class="input" type="password" placeholder="备份口令" autocomplete="off" required />
      </form>
      <template #footer>
        <button class="btn ghost" @click="restoreFile = null">取消</button>
        <button class="btn primary" :disabled="busy || !restorePass" @click="startRestore">校验备份</button>
      </template>
    </Modal>

    <Modal v-if="del" title="删除备份" @close="del = null">
      <p>确定删除备份「{{ del.name }}」？删除后无法找回。</p>
      <template #footer>
        <button class="btn ghost" @click="del = null">取消</button>
        <button class="btn danger" @click="doDelete">删除</button>
      </template>
    </Modal>

    <Modal v-if="restarting" title="正在恢复" @close="restarting.failed && (restarting = null)">
      <div class="restart">
        <span v-if="!restarting.failed && !restarting.keyChanged" class="spin" />
        <p>{{ restarting.status }}</p>
      </div>
      <template v-if="restarting.keyChanged">
        <p class="warn">服务端公钥已经改变，当前页面无法继续连接。请按下面的方式重新信任新的公钥：</p>
        <ul class="steps muted">
          <li>本地客户端：停止客户端，用新的公钥重新启动（<code>-server-key</code> 参数）。</li>
          <li>网页模式：在浏览器设置中清除本站的数据，然后重新打开页面，并核对公钥指纹。</li>
        </ul>
        <CopyField :text="restarting.publicKey" label="新的服务端公钥" />
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.backup { max-width: 860px; display: grid; gap: 14px; }
h2 { font-size: 16px; margin-bottom: 12px; }
.desc { margin-bottom: 14px; line-height: 1.6; }
.pw { width: 210px; }
.check { margin-right: 4px; }
.hint { margin-top: 10px; line-height: 1.6; }
.job { display: flex; align-items: center; gap: 12px; padding: 14px 20px; }
.staged { border: 1px solid rgba(240, 198, 116, 0.35); }
.staged p { margin-bottom: 10px; line-height: 1.6; }
.staged .field { margin: 12px 0; }
.warn { color: var(--warn); }
code { font-family: Consolas, ui-monospace, monospace; font-size: 12.5px; word-break: break-all; }
.head { margin-bottom: 8px; }
.head h2 { margin: 0; }
.btn.disabled { opacity: 0.5; pointer-events: none; }
.transfer { display: grid; gap: 6px; margin: 4px 0 12px; font-size: 13px; }
.empty { padding: 10px 0; }
.files { display: grid; }
.file { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; padding: 12px 0; border-top: 1px solid var(--line); }
.file:first-child { border-top: none; }
.meta { flex: 1; min-width: 220px; display: grid; gap: 4px; }
.name { font-family: Consolas, ui-monospace, monospace; font-size: 13.5px; word-break: break-all; }
.small { font-size: 12.5px; }
.meta .progress { margin-top: 4px; }
.ops { gap: 8px; }
.foot { border-top: 1px solid var(--line); padding-top: 12px; }
.mb { margin-bottom: 12px; line-height: 1.6; }
.restart { display: flex; align-items: center; gap: 12px; margin-bottom: 12px; }
.steps { margin: 8px 0 12px 18px; display: grid; gap: 6px; line-height: 1.6; }
.spin { flex: none; width: 18px; height: 18px; border: 2px solid rgba(255, 255, 255, 0.15); border-top-color: var(--accent); border-radius: 50%; animation: spin 0.8s linear infinite; }
.list-enter-active { transition: opacity 0.3s var(--ease-out), transform 0.45s var(--spring-bouncy); }
.list-leave-active { transition: opacity 0.2s ease-in, transform 0.2s ease-in; }
.list-enter-from, .list-leave-to { opacity: 0; transform: translate3d(0, 10px, 0) scale(0.98); }
.list-move { transition: transform 0.4s var(--spring); }
</style>
