import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '../stores/auth'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    {
      path: '/login',
      name: 'login',
      component: () => import('../views/Login.vue'),
    },
    {
      path: '/',
      name: 'dashboard',
      component: () => import('../views/Dashboard.vue'),
      meta: { requiresAuth: true },
    },
  ],
})

router.beforeEach((to) => {
  const auth = useAuthStore()
  // 未登录访问受保护页 → 登录页
  if (to.meta.requiresAuth && !auth.isLoggedIn) {
    return { path: '/login' }
  }
  // 已登录访问登录页 → 首页
  if (to.path === '/login' && auth.isLoggedIn) {
    return { path: '/' }
  }
})

export default router
