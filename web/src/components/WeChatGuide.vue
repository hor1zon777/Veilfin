<script setup>
// 微信内置浏览器引导页：网页模式需要 Service Worker 与 WebCrypto 完成端到端解密，
// 微信内置浏览器无法正常解密播放，引导用户点击右上角菜单跳转到系统浏览器打开当前链接。
import { ref, computed, onBeforeUnmount } from 'vue'
import { toast, toastError } from '../store.js'
import { copyText } from '../util.js'
import Icon from './Icon.vue'
import CopyField from './CopyField.vue'

const ua = navigator.userAgent || ''
const isIOS = /iPhone|iPad|iPod/i.test(ua)
const isAndroid = /Android/i.test(ua)
const isDesktop = /WindowsWechat|MacWechat/i.test(ua) || (!isIOS && !isAndroid)

const url = location.href
const copied = ref(false)
let timer = 0

const steps = computed(() => {
  if (isIOS) {
    return [
      { strong: '点击右上角「···」', rest: '打开菜单' },
      { strong: '选择「在默认浏览器中打开」', rest: '或「在 Safari 中打开」' },
    ]
  }
  if (isAndroid) {
    return [
      { strong: '点击右上角「···」', rest: '打开菜单' },
      { strong: '选择「在浏览器打开」', rest: '使用系统浏览器访问' },
    ]
  }
  return [
    { strong: '点击右上角「···」或工具栏浏览器图标', rest: '' },
    { strong: '选择「用默认浏览器打开」', rest: '或复制下方链接到浏览器访问' },
  ]
})

async function copyLink() {
  try {
    await copyText(url)
    copied.value = true
    clearTimeout(timer)
    timer = setTimeout(() => (copied.value = false), 2000)
    toast('链接已复制，请粘贴到浏览器打开', 'ok')
  } catch (e) {
    toastError(e)
  }
}

onBeforeUnmount(() => clearTimeout(timer))
</script>

<template>
  <div class="auth-page wx-guide">
    <div v-if="!isDesktop" class="wx-arrow" aria-hidden="true">
      <span class="wx-arrow-label">点这里打开浏览器</span>
      <svg class="wx-arrow-svg" viewBox="0 0 64 64" width="52" height="52" fill="none">
        <path d="M12 52 C18 30, 32 20, 50 12" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-dasharray="5 5" />
        <path d="M38 10 L52 11 L46 24" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round" />
      </svg>
    </div>

    <div class="auth-box wx-box">
      <div class="brand"><Icon name="play" :size="26" class="logo" /> Veilfin</div>
      <h2>请在浏览器中打开</h2>
      <p class="muted desc">微信内置浏览器无法完成端到端解密，视频和图片不能正常加载。请跳转到系统浏览器继续访问：</p>

      <ol class="steps">
        <li v-for="(s, i) in steps" :key="i">
          <span class="num">{{ i + 1 }}</span>
          <span><b>{{ s.strong }}</b><template v-if="s.rest">，{{ s.rest }}</template></span>
        </li>
      </ol>

      <div class="field link-box">
        <label>当前链接（跳转后会自动定位到同一页面）</label>
        <CopyField :text="url" label="链接" />
      </div>

      <button type="button" class="btn primary big wide" @click="copyLink">
        <Icon :name="copied ? 'check' : 'copy'" :size="18" />
        {{ copied ? '已复制链接，请去浏览器粘贴打开' : '复制当前链接' }}
      </button>

      <p class="muted hint sec"><Icon name="shield" :size="14" /> 为保障端到端加密，仅在系统浏览器中解密播放</p>
    </div>
  </div>
</template>

<style scoped>
.wx-guide { position: relative; padding-top: max(68px, env(safe-area-inset-top)); }
.wx-arrow { position: fixed; top: max(10px, env(safe-area-inset-top)); right: 16px; display: flex; align-items: flex-end; gap: 4px;
  color: var(--accent); pointer-events: none; z-index: 10; animation: wx-bob 1.8s var(--ease-out) infinite alternate; }
.wx-arrow-label { padding: 5px 12px; border-radius: 999px; background: rgba(82, 181, 75, 0.16); border: 1px solid rgba(82, 181, 75, 0.35);
  font-size: 13px; font-weight: 500; white-space: nowrap; backdrop-filter: blur(12px); -webkit-backdrop-filter: blur(12px); }
.wx-arrow-svg { flex: none; }
@keyframes wx-bob { from { transform: translate3d(0, 0, 0); } to { transform: translate3d(4px, -5px, 0); } }
.wx-box { width: 420px; }
.wx-box h2 { margin-bottom: 10px; }
.desc { line-height: 1.65; margin: 0 0 18px; font-size: 13.5px; }
.steps { list-style: none; margin: 0 0 18px; padding: 14px 16px; display: grid; gap: 12px;
  background: rgba(255, 255, 255, 0.04); border: 1px solid rgba(255, 255, 255, 0.08); border-radius: 12px; }
.steps li { display: flex; align-items: flex-start; gap: 10px; line-height: 1.55; }
.num { flex: none; width: 22px; height: 22px; border-radius: 50%; background: rgba(82, 181, 75, 0.2); color: var(--accent);
  display: inline-flex; align-items: center; justify-content: center; font-size: 12px; font-weight: 600; margin-top: 1px; }
.steps b { color: #fff; font-weight: 600; }
.link-box { margin-bottom: 14px; }
@media (max-width: 480px) {
  .wx-box { padding: 24px 20px; }
}
</style>
