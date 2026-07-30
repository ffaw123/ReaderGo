import { createApp } from 'vue'
import { createRouter, createWebHistory } from 'vue-router'
import App from './App.vue'
import AuthView from './views/AuthView.vue'
import WorkspaceView from './views/WorkspaceView.vue'
import './styles.css'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', redirect: '/login' },
    { path: '/login', component: AuthView, props: { mode: 'login' } },
    { path: '/register', component: AuthView, props: { mode: 'register' } },
    { path: '/app', component: WorkspaceView },
    { path: '/:pathMatch(.*)*', redirect: '/login' },
  ],
})

router.beforeEach((to) => {
  if (to.path === '/app') {
    const hasSession = localStorage.getItem('readergo_session')
    const isPreview = sessionStorage.getItem('readergo_preview') === 'true'
    if (!hasSession && !isPreview) return '/login'
  }
  if ((to.path === '/login' || to.path === '/register') && localStorage.getItem('readergo_session')) {
    return '/app'
  }
})

createApp(App).use(router).mount('#app')
