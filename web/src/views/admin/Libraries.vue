<script setup>
import { ref, onMounted } from 'vue'
import { rpc, toast, toastError } from '../../store.js'
import { kindNames, scraperNames, fmtDateTime } from '../../util.js'
import Modal from '../../components/Modal.vue'
import LibraryForm from '../../components/LibraryForm.vue'
import Icon from '../../components/Icon.vue'

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
onMounted(load)

function openNew() {
  edit.value = { id: 0, name: '', kind: 'home', restricted: false, scraper: 'none', paths: [''], sortOrder: libs.value.length }
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
  } catch (e) {
    toastError(e)
  } finally {
    busy.value = false
  }
}

async function scan(l, refresh) {
  try {
    await rpc('admin.libs.scan', { id: l.id, refresh })
    toast(refresh ? '已开始刷新元数据' : '已开始扫描', 'ok')
  } catch (e) {
    toastError(e)
  }
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
      <div v-for="(l, i) in libs" :key="l.id" class="card-panel lib" :style="{ '--i': i }">
        <div class="lhead">
          <h2>{{ l.name }} <Icon v-if="l.restricted" name="lock" :size="16" class="muted" /></h2>
          <span class="muted">{{ kindNames[l.kind] }} · {{ l.count }} 个视频 · {{ scraperNames[l.scraper] }}</span>
        </div>
        <div v-for="p in l.paths" :key="p" class="path">{{ p }}</div>
        <div class="muted small">上次扫描：{{ l.lastScanAt ? fmtDateTime(l.lastScanAt) : '从未' }}</div>
        <div class="row ops">
          <button class="btn" @click="scan(l, false)"><Icon name="refresh" :size="18" /> 扫描</button>
          <button class="btn" @click="scan(l, true)">刷新元数据</button>
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
.lhead { margin-bottom: 8px; }
.lhead h2 { font-size: 18px; display: flex; align-items: center; gap: 6px; }
.path { font-family: Consolas, monospace; font-size: 12.5px; color: #bbb; word-break: break-all; padding: 2px 0; }
.small { font-size: 12.5px; margin-top: 6px; }
.ops { margin-top: 14px; }
</style>
