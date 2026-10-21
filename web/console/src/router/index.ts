import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import { TOKEN_KEY } from '@/api/client'

const routes: RouteRecordRaw[] = [
  {
    path: '/login',
    name: 'login',
    component: () => import('@/views/login/index.vue'),
    meta: { public: true, title: '登录' },
  },
  {
    path: '/',
    component: () => import('@/layout/index.vue'),
    redirect: '/dashboard',
    children: [
      { path: 'dashboard', name: 'dashboard', component: () => import('@/views/dashboard/index.vue'), meta: { title: '数据概览', icon: 'DataLine' } },
      { path: 'channels', name: 'channels', component: () => import('@/views/channels/index.vue'), meta: { title: '渠道管理', icon: 'Connection' } },
      { path: 'history', name: 'history', component: () => import('@/views/history/index.vue'), meta: { title: '请求历史', icon: 'Tickets' } },
      { path: 'shadow', name: 'shadow', component: () => import('@/views/shadow/index.vue'), meta: { title: '影子对比', icon: 'Aim' } },
      { path: 'tokens', name: 'tokens', component: () => import('@/views/tokens/index.vue'), meta: { title: '令牌管理', icon: 'Key' } },
      { path: 'settings', name: 'settings', component: () => import('@/views/settings/index.vue'), meta: { title: '系统设置', icon: 'Setting' } },
    ],
  },
  { path: '/:pathMatch(.*)*', redirect: '/dashboard' },
]

const router = createRouter({
  history: createWebHistory('/console/'),
  routes,
  scrollBehavior: () => ({ top: 0 }),
})

router.beforeEach((to) => {
  const authed = Boolean(localStorage.getItem(TOKEN_KEY))
  if (!to.meta.public && !authed) return { path: '/login', query: { redirect: to.fullPath } }
  if (to.path === '/login' && authed) return { path: '/dashboard' }
  document.title = to.meta.title ? `${to.meta.title} · Nexus` : 'Nexus Control Plane'
  return true
})

export default router
