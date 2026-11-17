<script setup>
// 媒体库表单：名称、类型、刮削源、目录（带服务器目录浏览器）、受限。
// 管理后台的“添加 / 编辑媒体库”弹窗和首次设置向导共用。lib 对象由父组件持有，表单直接修改它。
import { ref } from 'vue'
import { rpc, toastError } from '../store.js'
import { kindNames, scraperNames } from '../util.js'
import Modal from './Modal.vue'
import Icon from './Icon.vue'

const props = defineProps({ lib: { type: Object, required: true } })
const emit = defineEmits(['submit'])

const defaultScraper = { home: 'none', movie: 'tmdb', jav: 'metatube', other: 'none' }
const defaultName = { home: '自制视频', movie: '电影', jav: '日本影片', other: '视频' }

function onKind() {
  const l = props.lib
  l.scraper = defaultScraper[l.kind]
  if (l.kind === 'jav') l.restricted = true
  // 名称还是某个类型的默认名（或为空）时，跟着类型一起改
  if (!l.name || Object.values(defaultName).includes(l.name)) l.name = defaultName[l.kind]
}

// ---- 目录浏览 ----
const browser = ref(null) // { path, parent, dirs }
let browseIndex = 0
async function browse(i) {
  browseIndex = i
  try {
    browser.value = await rpc('admin.fs.list', { path: props.lib.paths[i] ?? '' })
  } catch (e) {
    browser.value = await rpc('admin.fs.list', { path: '' }).catch(() => null)
    toastError(e)
  }
}
async function cd(path) {
  try {
    browser.value = await rpc('admin.fs.list', { path })
  } catch (e) {
    toastError(e)
  }
}
function pick() {
  props.lib.paths[browseIndex] = browser.value.path
  browser.value = null
}
</script>

<template>
  <form class="libform" @submit.prevent="emit('submit')">
    <div class="field"><label>名称</label><input v-model="lib.name" class="input" required /></div>
    <div class="field">
      <label>类型</label>
      <select v-model="lib.kind" class="input" @change="onKind">
        <option v-for="(n, k) in kindNames" :key="k" :value="k">{{ n }}</option>
      </select>
      <span class="hint">自制视频以横版缩略图展示，并优先读取同名 .json / .nfo；日本影片会从文件名识别番号。</span>
    </div>
    <div class="field">
      <label>刮削源</label>
      <select v-model="lib.scraper" class="input">
        <option v-for="(n, k) in scraperNames" :key="k" :value="k">{{ n }}</option>
      </select>
      <span class="hint">有 NFO 的视频优先使用 NFO，不会在线刮削。在线刮削需要先在“服务器设置”里填写 TMDB Key 或 MetaTube 地址。</span>
    </div>
    <div class="field">
      <label>目录</label>
      <div v-for="(p, i) in lib.paths" :key="i" class="pathrow">
        <input v-model="lib.paths[i]" class="input" placeholder="服务器上的绝对路径" />
        <button type="button" class="btn" @click="browse(i)">浏览…</button>
        <button v-if="lib.paths.length > 1" type="button" class="icon-btn" @click="lib.paths.splice(i, 1)"><Icon name="close" :size="18" /></button>
      </div>
      <button type="button" class="btn ghost addp" @click="lib.paths.push('')">+ 添加目录</button>
    </div>
    <label class="check"><input v-model="lib.restricted" type="checkbox" /> 受限媒体库</label>
    <p class="hint">受限媒体库只对被授权的用户可见；用户设置 PIN 后，还需要输入 PIN 才能看到。</p>
    <button type="submit" class="hidden" />

    <Modal v-if="browser" title="选择目录" width="560px" @close="browser = null">
      <div class="cur-path">{{ browser.path || '（选择磁盘）' }}</div>
      <div class="dirs">
        <button v-if="browser.path" type="button" class="dir" @click="cd(browser.parent)">⬑ 上一级</button>
        <button v-for="d in browser.dirs" :key="d.path" type="button" class="dir" @click="cd(d.path)"><Icon name="folder" :size="18" class="fi" /> {{ d.name }}</button>
        <div v-if="!browser.dirs?.length" class="muted pad">没有子目录</div>
      </div>
      <template #footer>
        <button type="button" class="btn ghost" @click="browser = null">取消</button>
        <button type="button" class="btn primary" :disabled="!browser.path" @click="pick">选择此目录</button>
      </template>
    </Modal>
  </form>
</template>

<style scoped>
.pathrow { display: flex; gap: 8px; margin-bottom: 6px; align-items: center; }
.addp { justify-self: start; }
.cur-path { font-family: Consolas, monospace; font-size: 13px; padding: 8px 10px; background: var(--bg2); border-radius: 4px; margin-bottom: 8px; word-break: break-all; }
.dirs { max-height: 50vh; overflow-y: auto; display: grid; }
.dir { text-align: left; padding: 8px 10px; border-radius: 8px; display: flex; align-items: center; gap: 8px; animation: fade-in .3s var(--ease-out) backwards; }
.dir:hover { background: rgba(255,255,255,.06); }
.fi { color: #d9b44a; }
.pad { padding: 10px; }
</style>
