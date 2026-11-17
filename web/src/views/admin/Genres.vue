<script setup>
import { ref, computed, onMounted } from 'vue'
import { rpc, toast, toastError } from '../../store.js'
import Modal from '../../components/Modal.vue'

const list = ref(null) // [{ name, count }]
const q = ref('')
const selected = ref([])
const busy = ref(false)
const rename = ref(null) // { from: [名称], to, merge: 是否多选合并 }
const del = ref(null) // [名称]

const shown = computed(() => {
  const k = q.value.trim().toLowerCase()
  return k ? list.value.filter((g) => g.name.toLowerCase().includes(k)) : list.value
})
const allShown = computed(() => shown.value.length > 0 && shown.value.every((g) => selected.value.includes(g.name)))
const countOf = (names) => list.value.filter((g) => names.includes(g.name)).reduce((n, g) => n + g.count, 0)

// 新名称已经存在（且不在被改名的项里）时，操作实际上是合并
const mergeTarget = computed(() => {
  const r = rename.value
  if (!r) return null
  const to = r.to.trim()
  return to && !r.from.includes(to) && list.value.find((g) => g.name === to)
})

async function load() {
  try {
    list.value = await rpc('admin.tags.list', { kind: 'genre' })
    const names = new Set(list.value.map((g) => g.name))
    selected.value = selected.value.filter((n) => names.has(n))
  } catch (e) {
    toastError(e)
  }
}
onMounted(load)

function toggleAll() {
  const names = shown.value.map((g) => g.name)
  selected.value = allShown.value
    ? selected.value.filter((n) => !names.includes(n))
    : [...new Set([...selected.value, ...names])]
}

function openRename(g) {
  rename.value = { from: [g.name], to: g.name, merge: false }
}
function openMerge() {
  // 默认合并到条目最多的那个
  const top = list.value.find((g) => selected.value.includes(g.name))
  rename.value = { from: [...selected.value], to: top?.name || '', merge: true }
}

async function doRename() {
  const r = rename.value
  const to = r.to.trim()
  if (!to) return toast('名称不能为空', 'err')
  if (r.from.length === 1 && r.from[0] === to) return (rename.value = null)
  busy.value = true
  try {
    const res = await rpc('admin.tags.rename', { kind: 'genre', from: r.from, to })
    toast(`已更新 ${res.items} 个条目`, 'ok')
    rename.value = null
    selected.value = selected.value.filter((n) => !r.from.includes(n))
    await load()
  } catch (e) {
    toastError(e)
  } finally {
    busy.value = false
  }
}

async function doDelete() {
  busy.value = true
  try {
    const res = await rpc('admin.tags.delete', { kind: 'genre', names: del.value })
    toast(`已从 ${res.items} 个条目中移除`, 'ok')
    selected.value = selected.value.filter((n) => !del.value.includes(n))
    del.value = null
    await load()
  } catch (e) {
    toastError(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="admin-body">
    <div class="row head">
      <input v-model="q" class="input search" placeholder="搜索类型" />
      <span v-if="list" class="muted">共 {{ list.length }} 个类型</span>
      <div class="spacer" />
      <template v-if="selected.length">
        <span class="muted">已选 {{ selected.length }} 个</span>
        <button class="btn" :disabled="selected.length < 2" @click="openMerge">合并</button>
        <button class="btn ghost err" @click="del = [...selected]">删除</button>
      </template>
    </div>
    <p class="hint tip">在这里修改的类型会对相关条目锁定“类型”字段，之后重新刮削不会改回去；在条目的“编辑”窗口中“解除全部锁定”可以恢复自动更新。</p>

    <div v-if="!list" class="loading"><div class="spinner" /></div>
    <div v-else-if="!list.length" class="empty">还没有任何类型，刮削或编辑条目后会出现在这里</div>
    <div v-else class="card-panel">
      <table class="list">
        <thead>
          <tr>
            <th class="sel"><input type="checkbox" :checked="allShown" @change="toggleAll" /></th>
            <th>名称</th>
            <th class="num">条目数</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(g, i) in shown" :key="g.name" class="fade" :style="{ '--i': Math.min(i, 15) }">
            <td class="sel"><input v-model="selected" type="checkbox" :value="g.name" /></td>
            <td>{{ g.name }}</td>
            <td class="num">{{ g.count }}</td>
            <td class="ops">
              <button class="btn ghost" @click="openRename(g)">重命名</button>
              <button class="btn ghost err" @click="del = [g.name]">删除</button>
            </td>
          </tr>
          <tr v-if="!shown.length">
            <td colspan="4" class="muted">没有匹配的类型</td>
          </tr>
        </tbody>
      </table>
    </div>

    <Modal v-if="rename" :title="rename.merge ? `合并 ${rename.from.length} 个类型` : `重命名：${rename.from[0]}`" @close="rename = null">
      <form @submit.prevent="doRename">
        <div v-if="rename.merge" class="field">
          <label>要合并的类型（点击可设为合并后的名称）</label>
          <div class="chips">
            <button v-for="n in rename.from" :key="n" type="button" class="chip pick" :class="{ on: rename.to.trim() === n }" @click="rename.to = n">{{ n }}</button>
          </div>
        </div>
        <div class="field">
          <label>{{ rename.merge ? '合并后的名称' : '新名称' }}</label>
          <input v-model="rename.to" class="input" maxlength="100" required />
          <span v-if="mergeTarget" class="hint">「{{ mergeTarget.name }}」已存在（{{ mergeTarget.count }} 个条目），将合并到它。</span>
          <span class="hint">共影响约 {{ countOf(rename.from) }} 个条目。</span>
        </div>
        <button type="submit" class="hidden" />
      </form>
      <template #footer>
        <button class="btn ghost" @click="rename = null">取消</button>
        <button class="btn primary" :disabled="busy" @click="doRename">{{ rename.merge || mergeTarget ? '合并' : '保存' }}</button>
      </template>
    </Modal>

    <Modal v-if="del" title="删除类型" @close="del = null">
      <p>
        确定删除{{ del.length > 1 ? ` ${del.length} 个类型` : `类型「${del[0]}」` }}？会从约 {{ countOf(del) }} 个条目中移除，条目本身不受影响。
      </p>
      <template #footer>
        <button class="btn ghost" @click="del = null">取消</button>
        <button class="btn danger" :disabled="busy" @click="doDelete">删除</button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.head { margin-bottom: 8px; gap: 12px; flex-wrap: wrap; }
.search { max-width: 260px; }
.tip { display: block; margin-bottom: 14px; }
.sel { width: 36px; }
.num { width: 90px; text-align: right; }
table.list th.num { text-align: right; }
.ops { text-align: right; white-space: nowrap; }
.pick { cursor: pointer; transition: background-color 0.2s, color 0.2s; }
.pick:hover, .pick.on { background: var(--accent); color: #fff; }
</style>
