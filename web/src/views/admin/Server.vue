<script setup>
import { ref, computed, onMounted } from 'vue'
import { state, rpc, toast, toastError, connectionStatus } from '../../store.js'
import CopyField from '../../components/CopyField.vue'

const s = ref(null)
const ffmpeg = ref(true)
const ffmpegInfo = ref('')
const encoders = ref([]) // 实际可用的 H.264 编码器（服务端试编码检测）
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
    encoders.value = r.encoders || []
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
      transcodeMax: Math.round(Number(s.value.transcodeMax)) || 2,
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
        <div class="field">
          <label>进度条预览图生成线程数（1–16）</label>
          <input v-model="s.trickplayWorkers" class="input short" type="number" min="1" max="16" step="1" inputmode="numeric" />
          <span class="hint">是否生成进度条预览图在每个媒体库的设置里单独开关。预览图一次处理一个视频，这里是同时截图的 ffmpeg 数，默认 4。线程越多越快，但更占 CPU；机械硬盘或网络存储上调高收益有限，建议不超过 CPU 核数的一半；修改后立即生效。</span>
        </div>
        <label class="check"><input v-model="s.keepPageTitle" type="checkbox" /> 播放视频时网页标题保持不变（不显示正在播放的视频名称）</label>
        <label class="check"><input v-model="s.autoCheckUpdate" type="checkbox" /> 自动检查更新（每 12 小时访问一次 GitHub，只提示，不会自动安装）</label>
        <p v-if="ffmpegInfo" class="hint ff">正在使用：{{ ffmpegInfo }}</p>
      </section>

      <section class="card-panel d3">
        <h2>播放时转码</h2>
        <p v-if="ffmpeg && !encoders.length" class="warn">
          当前使用的 ffmpeg 没有可用的 H.264 编码器，播放时不能切换画质。
          需要转码请安装带 libx264 的 ffmpeg（或有显卡硬件编码的版本），或启动时用 -ffmpeg 指定；内置 ffmpeg 的服务端版本自带 OpenH264，也可以改用它。然后重启服务端。
        </p>
        <template v-if="encoders.length">
          <label class="check"><input v-model="s.transcodeEnabled" type="checkbox" /> 允许播放时选择较低画质（服务端实时转码）</label>
          <p class="hint sub">网络慢、或浏览器不支持原始编码（例如 HEVC）时可以转成 1080P / 720P / 480P / 360P 的 H.264 播放。转码很占 CPU，原画播放不受影响。</p>
          <div class="field">
            <label>编码器</label>
            <select v-model="s.transcodeEncoder" class="input short">
              <option value="">自动（{{ encoders[0].label }}）</option>
              <option v-for="e in encoders" :key="e.id" :value="e.id">{{ e.label }}</option>
            </select>
            <span class="hint">这里只列出能用的编码器。显卡硬件编码几乎不占 CPU，转码更快；OpenH264（内置 ffmpeg 自带）画质不如 libx264，只在没有其他编码器时自动选用。</span>
          </div>
          <div class="field">
            <label>同时转码的播放数上限（1–16）</label>
            <input v-model="s.transcodeMax" class="input short" type="number" min="1" max="16" step="1" inputmode="numeric" />
            <span class="hint">默认 2。达到上限后，新的播放只能看原画；软件编码时建议按 CPU 性能设置（一路 1080P 大约要占满 2～4 个核）。</span>
          </div>
        </template>
      </section>

      <section class="card-panel d4">
        <h2>TMDB（普通电影）</h2>
        <div class="field">
          <label>API Key 或读访问令牌</label>
          <input v-model="s.tmdbKey" class="input" type="password" autocomplete="off" placeholder="在 themoviedb.org 的账户设置中申请" />
        </div>
        <div class="field"><label>语言</label><input v-model="s.tmdbLang" class="input short" placeholder="zh-CN" /></div>
      </section>

      <section class="card-panel d5">
        <h2>MetaTube（番号刮削）</h2>
        <div class="field">
          <label>服务地址</label>
          <input v-model="s.metatubeUrl" class="input" placeholder="http://127.0.0.1:8080" />
          <span class="hint">需要自行部署 MetaTube 服务端（metatube-community/metatube-sdk-go），本程序通过它的 HTTP API 查询。</span>
        </div>
        <div class="field"><label>访问令牌（可选）</label><input v-model="s.metatubeToken" class="input" type="password" autocomplete="off" /></div>
      </section>

      <section class="card-panel d6">
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
