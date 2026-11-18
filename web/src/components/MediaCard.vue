<script setup>
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import { state } from '../store.js'
import { placeholderStyle, resolutionLabel, fmtDuration } from '../util.js'
import Icon from './Icon.vue'

const props = defineProps({
  item: { type: Object, required: true },
  shape: { type: String, default: 'poster' }, // poster 竖版 | thumb 横版
  subtitle: { type: String, default: '' },
  showProgress: { type: Boolean, default: true },
  index: { type: Number, default: 0 }, // 用于依次出现的延迟
  to: { type: [String, Object], default: '' }, // 点击后去哪里，默认是详情页
})

const router = useRouter()
// 点中间的播放键直接播放（从上次看到的位置继续），点卡片其他位置进详情页。
// 触屏没有真正的悬停：卡片没选中时播放键看不见，这时点到播放键的位置按点卡片处理（进详情页）；
// 点一下卡片选中后（iOS 等会保持悬停状态，播放键显示出来），再点播放键直接播放。
// 是否已选中要在按下时判断：松手后浏览器才会把悬停状态设到这张卡片上
let armed = true
function onPlayDown(e) {
  armed = e.pointerType === 'mouse' || e.pointerType === 'pen' || !!e.currentTarget.closest('.card')?.matches(':hover')
}
function onPlayClick(e) {
  const go = armed
  armed = true // 键盘回车触发的点击没有 pointerdown，按可播放处理
  if (!go) return // 交给外层链接进详情页
  e.preventDefault()
  e.stopPropagation()
  router.push(`/play/${props.item.id}`)
}

const loaded = ref(false)
const img = computed(() => {
  const it = props.item
  return props.shape === 'poster' ? it.poster || it.thumb : it.thumb || it.poster
})
const pct = computed(() => {
  const it = props.item
  if (!it.position || !it.duration) return 0
  return Math.min(100, (it.position / it.duration) * 100)
})
const blur = computed(() => props.item.restricted && state.me?.blurRestricted)
const sub = computed(() => {
  if (props.subtitle) return props.subtitle
  const it = props.item
  const parts = []
  if (it.code && it.code !== it.title) parts.push(it.code)
  if (it.year) parts.push(it.year)
  else if (it.duration) parts.push(fmtDuration(it.duration))
  return parts.join(' · ')
})
// 依次出现：每页最多错开 14 个，后面的同时出现，避免等太久
const delay = computed(() => ({ '--i': Math.min(props.index % 60, 14) }))
</script>

<template>
  <router-link :to="to || `/item/${item.id}`" class="card rise" :class="shape" :style="delay">
    <div class="frame" :style="img ? null : placeholderStyle(item.id)">
      <img v-if="img" :src="img" loading="lazy" decoding="async" alt="" class="lazy-img" :class="{ blur, loaded }" @load="loaded = true" />
      <div v-else class="ph-title">{{ item.title }}</div>
      <span v-if="item.played" class="played" title="已看过"><Icon name="check" :size="16" /></span>
      <span v-if="shape === 'thumb' && resolutionLabel(item.width, item.height)" class="res">{{ resolutionLabel(item.width, item.height) }}</span>
      <div v-if="showProgress && pct" class="progress"><div :style="{ transform: `scaleX(${pct / 100})` }" /></div>
      <div class="hover">
        <button type="button" class="play" title="播放" aria-label="播放" @pointerdown="onPlayDown" @click="onPlayClick"><Icon name="play" :size="30" /></button>
      </div>
    </div>
    <div class="title" :title="item.title">{{ item.title }}</div>
    <div v-if="sub" class="sub">{{ sub }}</div>
  </router-link>
</template>

<style scoped>
.card { display: block; min-width: 0; -webkit-tap-highlight-color: transparent; }
.frame { position: relative; border-radius: 8px; overflow: hidden; background: #1e1e1e; isolation: isolate;
  transition: transform .5s var(--spring), box-shadow .4s var(--ease-out); will-change: transform; }
.poster .frame { aspect-ratio: 2 / 3; }
.thumb .frame { aspect-ratio: 16 / 9; }
.frame img { width: 100%; height: 100%; object-fit: cover; display: block; }
.frame img.blur.loaded { filter: blur(18px) brightness(.7); }
.card:hover .frame img.blur { filter: none; }
.card:hover .frame { transform: translateY(-4px) scale(1.03); box-shadow: 0 0 0 3px var(--accent), 0 18px 36px rgba(0,0,0,.5); }
.card:active .frame { transform: scale(.97); transition-duration: .12s; box-shadow: 0 0 0 3px var(--accent); }
.ph-title { position: absolute; inset: 0; display: flex; align-items: flex-end; padding: 10px; font-size: 13px; color: rgba(255,255,255,.85); word-break: break-all; }
.played { position: absolute; top: 6px; right: 6px; width: 24px; height: 24px; border-radius: 50%; background: var(--accent); display: flex; align-items: center; justify-content: center; color: #fff;
  animation: pop-badge .5s var(--spring-bouncy) both; }
@keyframes pop-badge { from { transform: scale(0); } to { transform: scale(1); } }
.res { position: absolute; top: 6px; left: 6px; font-size: 11px; padding: 1px 6px; background: rgba(0,0,0,.55); border-radius: 4px; backdrop-filter: blur(8px); }
.progress { position: absolute; left: 0; right: 0; bottom: 0; height: 4px; background: rgba(0,0,0,.55); }
.progress div { height: 100%; background: var(--accent); transform-origin: left; animation: grow .9s var(--spring) both; animation-delay: .2s; }
@keyframes grow { from { transform: scaleX(0); } }
.hover { position: absolute; inset: 0; display: flex; align-items: center; justify-content: center; background: rgba(0,0,0,.28); opacity: 0; transition: opacity .25s var(--ease-out); }
.card:hover .hover { opacity: 1; }
.play { width: 52px; height: 52px; border-radius: 50%; background: rgba(82,181,75,.9); display: flex; align-items: center; justify-content: center; color: #fff;
  border: 0; padding: 0; cursor: pointer;
  transform: scale(.6); transition: transform .45s var(--spring-bouncy), background-color .2s, box-shadow .3s; box-shadow: 0 6px 20px rgba(0,0,0,.4); backdrop-filter: blur(6px); }
.card:hover .play { transform: scale(1); }
/* 触屏上卡片没选中时点播放键的位置也进详情页，由 onPlayDown / onPlayClick 判断 */
@media (hover: hover) {
  .card:hover .play:hover { transform: scale(1.12); background: var(--accent); box-shadow: 0 0 0 6px rgba(82,181,75,.25), 0 8px 24px rgba(0,0,0,.5); }
  .card:hover .play:active { transform: scale(.92); transition-duration: .08s; }
}
.title { margin-top: 8px; font-size: 13.5px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; transition: color .2s; }
.card:hover .title { color: #fff; }
.sub { font-size: 12px; color: var(--muted); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
</style>
