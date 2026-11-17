<script setup>
// 媒体库扫描进度：排队 / 扫描文件时显示流动的进度条，处理条目和生成预览图时显示实际进度，完成后打勾
import { computed } from 'vue'
import Icon from './Icon.vue'

const props = defineProps({ p: { type: Object, required: true } })

const active = computed(() => props.p.phase !== 'done' && props.p.phase !== 'failed')
// 进度条和右侧的计数一致：处理条目时按条目算，之后按预览图算
const pct = computed(() => {
  const p = props.p
  if (p.phase === 'done') return 100
  if (p.phase !== 'processing') return 0
  if (p.done < p.total) return Math.round((p.done / p.total) * 100)
  return p.trickTotal ? Math.round((p.trickDone / p.trickTotal) * 100) : 100
})
const indeterminate = computed(() => props.p.phase === 'queued' || props.p.phase === 'scanning')

const title = computed(() => {
  const p = props.p
  switch (p.phase) {
    case 'queued':
      return '等待扫描……'
    case 'scanning':
      return '正在扫描文件'
    case 'processing':
      return p.done < p.total ? (p.refresh ? '正在刷新元数据' : '正在读取元数据和截图') : '正在生成进度条预览图'
    case 'done':
      return '扫描完成'
    default:
      return '扫描失败'
  }
})
const count = computed(() => {
  const p = props.p
  if (p.phase === 'scanning') return `已发现 ${p.found} 个视频`
  if (p.phase === 'processing') return p.done < p.total ? `${p.done} / ${p.total}` : `${p.trickDone} / ${p.trickTotal}`
  if (p.phase === 'done') return `共 ${p.found} 个视频`
  return ''
})
const detail = computed(() => {
  const p = props.p
  if (p.phase === 'failed') return p.err
  const parts = []
  if (p.added) parts.push(`新增 ${p.added}`)
  if (p.removed) parts.push(`移除 ${p.removed}`)
  if (p.phase === 'done' && p.total > p.added) parts.push(`${p.refresh ? '重新处理' : '处理'}了 ${p.total} 个`)
  if (p.failed) parts.push(`${p.failed} 个处理失败`)
  if (p.phase === 'done' && !parts.length) parts.push('没有变化')
  return parts.join(' · ')
})
</script>

<template>
  <div class="scan" :class="p.phase">
    <div class="line">
      <span v-if="active" class="dot" />
      <span v-else-if="p.phase === 'done'" class="badge ok"><Icon name="check" :size="14" /></span>
      <span v-else class="badge bad"><Icon name="close" :size="14" /></span>
      <span :key="title" class="title in">{{ title }}</span>
      <span class="spacer" />
      <span class="count">{{ count }}</span>
    </div>
    <!-- 流动光带和实际进度是两个元素，切换时进度条不会从光带的宽度往回缩 -->
    <div class="bar">
      <i v-if="indeterminate" key="flow" class="flow" />
      <i v-else key="fill" class="fill" :style="{ width: pct + '%' }" />
    </div>
    <div v-if="detail" :key="detail" class="detail in">{{ detail }}</div>
  </div>
</template>

<style scoped>
.scan { margin-top: 12px; padding: 12px 14px; border-radius: 10px; background: rgba(82, 181, 75, 0.08); border: 1px solid rgba(82, 181, 75, 0.22);
  transition: background-color 0.4s, border-color 0.4s; }
.scan.failed { background: rgba(220, 80, 80, 0.08); border-color: rgba(220, 80, 80, 0.3); }
.line { display: flex; align-items: center; gap: 8px; font-size: 13.5px; }
.title { font-weight: 600; }
.count { color: var(--muted); font-variant-numeric: tabular-nums; font-size: 12.5px; }
.dot { width: 10px; height: 10px; margin: 0 2px; border-radius: 50%; background: var(--accent); animation: pulse 1.2s ease-in-out infinite; }
@keyframes pulse {
  0%, 100% { transform: scale(0.75); box-shadow: 0 0 0 0 rgba(82, 181, 75, 0.55); }
  50% { transform: scale(1); box-shadow: 0 0 0 6px rgba(82, 181, 75, 0); }
}
.badge { width: 18px; height: 18px; border-radius: 50%; display: inline-flex; align-items: center; justify-content: center; color: #fff;
  animation: pop 0.5s var(--spring-bouncy) both; }
.badge.ok { background: var(--accent); }
.badge.bad { background: #c44; }
@keyframes pop { from { transform: scale(0) rotate(-45deg); } to { transform: scale(1) rotate(0); } }
.bar { position: relative; height: 6px; margin-top: 10px; border-radius: 3px; background: rgba(255, 255, 255, 0.08); overflow: hidden; }
.bar i { position: absolute; left: 0; top: 0; bottom: 0; border-radius: 3px; background: linear-gradient(90deg, var(--accent), #7fd474); }
.bar .fill { transition: width 0.6s var(--spring); }
.failed .bar i { background: #c44; }
/* 不知道总数时：一段光带来回流动 */
.bar .flow { width: 35%; animation: flow 1.3s ease-in-out infinite; }
@keyframes flow { from { transform: translateX(-100%); } to { transform: translateX(290%); } }
.detail { margin-top: 8px; font-size: 12.5px; color: var(--muted); word-break: break-all; }
.failed .detail { color: #e88; }
/* 文字变化时新内容直接淡入（不等旧内容退场，阶段切换很快时也不会出现空白） */
.in { animation: text-in 0.35s var(--spring) both; }
@keyframes text-in { from { opacity: 0; transform: translateY(4px); } }
</style>
