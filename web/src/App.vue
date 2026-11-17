<script setup>
import { computed, ref, watch, nextTick } from 'vue'
import { useRoute } from 'vue-router'
import { state } from './store.js'
import { transitionName, onPageLeft } from './router.js'
import TopBar from './components/TopBar.vue'
import PinDialog from './components/PinDialog.vue'

const route = useRoute()
const showBar = computed(() => state.me && !route.meta.fullscreen && !route.meta.public)
const reload = () => location.reload()
// 管理后台的子页面在布局内部过渡，外层只在进出整个后台时过渡
const pageKey = (r) => r.meta.section || r.path
// 列表类页面保留在内存里：从详情页返回时列表、已加载的分页和滚动位置都还在（回来后各页面自己静默刷新进度）
const CACHED = ['Home', 'Library', 'Search', 'History']
const cached = ref(CACHED)
// 换了账号或受限内容重新上锁时清空缓存，避免按“后退”还能看到之前加载的内容
watch(
  () => [state.me?.id, state.me?.unlocked],
  () => {
    cached.value = []
    nextTick(() => (cached.value = CACHED))
  },
)
</script>

<template>
  <div v-if="state.fatal" class="fatal">
    <h2>无法启动</h2>
    <p>{{ state.fatal }}</p>
    <button class="btn primary" @click="reload">重试</button>
  </div>
  <template v-else>
    <Transition name="fade">
      <TopBar v-if="showBar" />
    </Transition>
    <router-view v-slot="{ Component, route: r }">
      <!-- 登录页背景有无限循环的 animation，必须指定只等 transition 结束，否则 Vue 会按动画时长（18 秒）等待离场 -->
      <Transition :name="transitionName" type="transition" mode="out-in" @after-leave="onPageLeft">
        <KeepAlive :include="cached" :max="6">
          <component :is="Component" :key="pageKey(r)" />
        </KeepAlive>
      </Transition>
    </router-view>
    <PinDialog v-if="state.pin" />
    <TransitionGroup tag="div" name="toast" class="toasts">
      <div v-for="t in state.toasts" :key="t.id" class="toast" :class="t.kind">{{ t.msg }}</div>
    </TransitionGroup>
  </template>
</template>

<style scoped>
.fatal { max-width: 560px; margin: 18vh auto; padding: 28px; background: var(--panel); border-radius: 10px; animation: rise-in 0.6s var(--spring) both; }
.fatal h2 { margin-bottom: 12px; color: var(--err); }
.fatal p { line-height: 1.7; margin-bottom: 20px; }
</style>
