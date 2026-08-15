import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '../stores/auth'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    {
      path: '/setup',
      name: 'setup',
      component: () => import('../views/Setup.vue'),
    },
    {
      path: '/login',
      name: 'login',
      component: () => import('../views/Login.vue'),
    },
    {
      path: '/',
      component: () => import('../views/Layout.vue'),
      redirect: '/dashboard',
      children: [
        {
          path: 'dashboard',
          name: 'dashboard',
          component: () => import('../views/Dashboard.vue'),
          meta: { title: '仪表盘' },
        },
        {
          path: 'seeds',
          name: 'seeds',
          component: () => import('../views/Seeds.vue'),
          meta: { title: '种子' },
        },
        {
          path: 'events',
          name: 'events',
          component: () => import('../views/Events.vue'),
          meta: { title: '事件' },
        },
        {
          path: 'sources',
          name: 'sources',
          component: () => import('../views/Sources.vue'),
          meta: { title: '站点源' },
        },
        {
          path: 'targets',
          name: 'targets',
          component: () => import('../views/Targets.vue'),
          meta: { title: '目标站' },
        },
        {
          path: 'qb',
          name: 'qb',
          component: () => import('../views/QB.vue'),
          meta: { title: 'qB 实例' },
        },
        {
          path: 'strategy',
          name: 'strategy',
          component: () => import('../views/Strategy.vue'),
          meta: { title: '策略' },
        },
        {
          path: 'notifiers',
          name: 'notifiers',
          component: () => import('../views/Notifiers.vue'),
          meta: { title: '通知' },
        },
        {
          path: 'backup',
          name: 'backup',
          component: () => import('../views/Backup.vue'),
          meta: { title: '备份' },
        },
      ],
    },
    {
      path: '/:pathMatch(.*)*',
      redirect: '/dashboard',
    },
  ],
})

router.beforeEach(async (to) => {
  const auth = useAuthStore()
  if (!auth.isBootstrapped) {
    await auth.bootstrap()
  }

  // 未初始化 → 仅允许进入 /setup
  if (!auth.initialized) {
    return to.path === '/setup' ? true : { path: '/setup' }
  }
  // 已初始化未登录 → 仅允许进入 /login
  if (!auth.loggedIn) {
    return to.path === '/login' ? true : { path: '/login' }
  }
  // 已登录 → 拦截 /setup 与 /login
  if (to.path === '/setup' || to.path === '/login') {
    return { path: '/dashboard' }
  }
  return true
})

export default router
