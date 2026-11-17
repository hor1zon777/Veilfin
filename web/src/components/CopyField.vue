<script setup>
// 一段可复制的长文本（公钥、命令行），点按钮复制，复制成功后图标短暂变成对勾
import { ref, onBeforeUnmount } from 'vue'
import { toast, toastError } from '../store.js'
import { copyText } from '../util.js'
import Icon from './Icon.vue'

const props = defineProps({ text: { type: String, required: true }, label: { type: String, default: '' } })
const done = ref(false)
let timer = 0

async function copy() {
  try {
    await copyText(props.text)
    done.value = true
    clearTimeout(timer)
    timer = setTimeout(() => (done.value = false), 1600)
    toast((props.label || '内容') + '已复制', 'ok')
  } catch (e) {
    toastError(e)
  }
}
onBeforeUnmount(() => clearTimeout(timer))
</script>

<template>
  <div class="copy-field">
    <code>{{ text }}</code>
    <button type="button" class="icon-btn" :class="{ done }" :title="'复制' + label" @click="copy">
      <Transition name="swap" mode="out-in">
        <Icon :key="done ? 'ok' : 'copy'" :name="done ? 'check' : 'copy'" :size="18" />
      </Transition>
    </button>
  </div>
</template>

<style scoped>
.copy-field { display: flex; align-items: flex-start; gap: 8px; background: var(--bg2); border: 1px solid var(--line); border-radius: var(--radius); padding: 7px 6px 7px 11px; }
code { flex: 1; min-width: 0; font-family: Consolas, ui-monospace, monospace; font-size: 12.5px; line-height: 1.6; word-break: break-all; user-select: all; padding-top: 2px; }
.icon-btn { flex: none; width: 30px; height: 30px; }
.icon-btn.done { color: var(--accent); }
.swap-enter-active, .swap-leave-active { transition: transform .25s var(--spring-bouncy), opacity .15s; }
.swap-enter-from, .swap-leave-to { transform: scale(.4); opacity: 0; }
</style>
