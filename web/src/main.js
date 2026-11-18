import { createApp } from 'vue'
import App from './App.vue'
import router from './router.js'
import { init, state } from './store.js'
import './style.css'
import './motion.css'

// iOS 会忽略 viewport 的 user-scalable=no，双指缩放要靠拦截 WebKit 的手势事件来禁止
for (const t of ['gesturestart', 'gesturechange']) document.addEventListener(t, (e) => e.preventDefault(), { passive: false })

init()
  .catch((e) => {
    state.fatal = e?.message || String(e)
  })
  .finally(() => {
    createApp(App).use(router).mount('#app')
  })
