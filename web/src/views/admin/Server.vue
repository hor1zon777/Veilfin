<script setup>
import { ref, computed, onMounted } from 'vue'
import { state, rpc, toast, toastError, connectionStatus } from '../../store.js'
import CopyField from '../../components/CopyField.vue'

const s = ref(null)
const ffmpeg = ref(true)
const ffmpegInfo = ref('')
const busy = ref(false)
const info = ref(null)
const serverUrl = ref('')

// 本地客户端的启动命令：固定信任当前服务端公钥
const clientCmd = computed(() =>
  info.value ? `veilfin client -server ${serverUrl.value || 'https://你的域名'} -server-key '${info.value.publicKey}'` : '',
)

onMounted(async () => {
  rpc('sys.info')
    .then((i) => (info.value = i))
    .catch(() => {})
  connectionStatus()
    .then((c) => (serverUrl.value = c.mode === 'local' ? c.server : c.serverUrl))
    .catch(() => {})
  try {
    const r = await rpc('admin.settings.get')
    s.value = r.settings
    ffmpeg.value = r.ffmpeg
    ffmpegInfo.value = r.ffmpegInfo
  } catch (e) {
    toastError(e)
  }
})

async function save() {
  busy.value = true
  try {
    s.value = await rpc('admin.settings.set', {
      ...s.value,
      scanIntervalHours: Number(s.value.scanIntervalHours) || 0,
      metadataWorkers: Math.round(Number(s.value.metadataWorkers)) || 4,
      trickplayWorkers: Math.round(Number(s.value.trickplayWorkers)) || 4,
    })
    state.serverName = s.value.serverName
    if (state.me) state.me.keepPageTitle = s.value.keepPageTitle
    toast('设置已保存', 'ok')
  } catch (e) {
    toastError(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="admin-body">
    <p v-if="!ffmpeg" class="warn card-panel">服务端没有找到 ffmpeg / ffprobe：视频可以播放，但不会生成截图、字幕和进度条预览图。请安装 ffmpeg 或启动时用 -ffmpeg 指定目录。</p>
    <form v-if="s" class="settings" @submit.prevent="save">
      <section v-if="info?.publicKey" class="card-panel d1">
        <h2>服务端身份</h2>
        <div class="field">
          <label>服务端公钥</label>
          <CopyField :text="info.publicKey" label="服务端公钥" />
        </div>
        <div class="field">
          <label>公钥指纹（给人工核对用）</label>
          <CopyField :text="info.fingerprint" label="公钥指纹" />
        </div>
        <div class="field">
          <label>本地客户端启动命令</label>
          <CopyField :text="clientCmd" label="启动命令" />
          <span class="hint">在电脑上运行这条命令，客户端会固定信任这个公钥；服务端被替换或遭到中间人攻击时会拒绝连接。</span>
        </div>
      </section>

      <section class="card-panel d2">
        <h2>常规</h2>
        <div class="field"><label>服务器名称</label><input v-model="s.serverName" class="input" /></div>
        <div class="field">
          <label>定时扫描间隔（小时，0 表示关闭）</label>
          <input v-model="s.scanIntervalHours" class="input short" inputmode="numeric" />
        </div>
        <div class="field">
          <label>元数据处理线程数（1–32）</label>
          <input v-model="s.metadataWorkers" class="input short" type="number" min="1" max="32" step="1" inputmode="numeric" />
          <span class="hint">同时刮削、下载封面、截图的条目数，默认 4。刮削服务限流或服务器性能较弱时可以调低；修改后立即生效。</span>
        </div>
        <label class="check"><input v-model="s.watchEnabled" type="checkbox" /> 监听目录变化，自动扫描新增 / 删除的文件</label>
        <label class="check"><input v-model="s.trickplayEnabled" type="checkbox" /> 生成进度条预览图（每个时间点只读取附近的一帧，不会把整个文件读一遍）</label>
        <div v-if="s.trickplayEnabled" class="field sub">
          <label>预览图生成线程数（1–16）</label>
          <input v-model="s.trickplayWorkers" class="input short" type="number" min="1" max="16" step="1" inputmode="numeric" />
          <span class="hint">预览图一次处理一个视频，这里是同时截图的 ffmpeg 数，默认 4。线程越多越快，但更占 CPU；机械硬盘或网络存储上调高收益有限，建议不超过 CPU 核数的一半；修改后立即生效。</span>
        </div>
        <label class="check"><input v-model="s.keepPageTitle" type="checkbox" /> 播放视频时网页标题保持不变（不显示正在播放的视频名称）</label>
        <label class="check"><input v-model="s.autoCheckUpdate" type="checkbox" /> 自动检查更新（每 12 小时访问一次 GitHub，只提示，不会自动安装）</label>
        <p v-if="ffmpegInfo" class="hint ff">正在使用：{{ ffmpegInfo }}</p>
      </section>

      <section class="card-panel d3">
        <h2>TMDB（普通电影）</h2>
        <div class="field">
          <label>API Key 或读访问令牌</label>
          <input v-model="s.tmdbKey" class="input" type="password" autocomplete="off" placeholder="在 themoviedb.org 的账户设置中申请" />
        </div>
        <div class="field"><label>语言</label><input v-model="s.tmdbLang" class="input short" placeholder="zh-CN" /></div>
      </section>

      <section class="card-panel d4">
        <h2>MetaTube（番号刮削）</h2>
        <div class="field">
          <label>服务地址</label>
          <input v-model="s.metatubeUrl" class="input" placeholder="http://127.0.0.1:8080" />
          <span class="hint">需要自行部署 MetaTube 服务端（metatube-community/metatube-sdk-go），本程序通过它的 HTTP API 查询。</span>
        </div>
        <div class="field"><label>访问令牌（可选）</label><input v-model="s.metatubeToken" class="input" type="password" autocomplete="off" /></div>
      </section>

      <section class="card-panel d5">
        <h2>网络</h2>
        <div class="field">
          <label>外网代理（可选）</label>
          <input v-model="s.scraperProxy" class="input" placeholder="http://127.0.0.1:7890 或 socks5://127.0.0.1:1080" />
          <span class="hint">用于在线刮削、下载封面，以及检查和下载更新，不影响视频播放。</span>
        </div>
      </section>

      <div class="actions"><button class="btn primary big" :disabled="busy">保存设置</button></div>
    </form>
  </div>
</template>

<style scoped>
.settings { max-width: 760px; display: grid; gap: 14px; }
h2 { font-size: 16px; margin-bottom: 14px; }
.short { max-width: 200px; }
.check { display: flex; margin-bottom: 8px; }
.sub { margin: 2px 0 12px 24px; }
.warn { color: var(--warn); margin-bottom: 14px; }
.ff { margin-top: 6px; word-break: break-all; }
</style>
