/**
 * Nexus route table — replace the template's src/router/index.ts with this
 * (or merge `nexusRoutes` into it and delete the demo routes).
 *
 * Only four surfaces survive: login, dashboard, channels, tokens.
 */
import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import { TOKEN_KEY } from '@/api/nexus'

export const nexusRoutes: RouteRecordRaw[] = [
  {
    path: '/login',
    name: 'NexusLogin',
    component: () => import('@/views/login/index.vue'),
    meta: { public: true, title: '登录' },
  },
  {
    path: '/',
    component: () => import('@/layout/index.vue'),
    redirect: '/dashboard',
    children: [
      {
        path: 'dashboard',
        name: 'NexusDashboard',
        component: () => import('@/views/dashboard/index.vue'),
        meta: { title: '数据概览', icon: 'Odometer' },
      },
      {
        path: 'channels',
        name: 'NexusChannels',
        component: () => import('@/views/channels/index.vue'),
        meta: { title: '渠道管理', icon: 'Connection' },
      },
      {
        path: 'tokens',
        name: 'NexusTokens',
        component: () => import('@/views/tokens/index.vue'),
        meta: { title: '令牌管理', icon: 'Key' },
      },
    ],
  },
  { path: '/:pathMatch(.*)*', redirect: '/dashboard' },
]

export const router = createRouter({
  history: createWebHistory(),
  routes: nexusRoutes,
  scrollBehavior: () => ({ top: 0 }),
})

router.beforeEach((to) => {
  const authed = Boolean(localStorage.getItem(TOKEN_KEY))
  if (!to.meta.public && !authed) {
    return { path: '/login', query: { redirect: to.fullPath } }
  }
  if (to.path === '/login' && authed) {
    return { path: '/dashboard' }
  }
  document.title = to.meta.title ? `${to.meta.title} · Nexus` : 'Nexus Control Plane'
  return true
})

export default router
