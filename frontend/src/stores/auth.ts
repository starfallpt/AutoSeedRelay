import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import api from '../api'

// 从 cookie 中兜底读取 CSRF token（后端可能通过 XSRF-TOKEN / csrf_token cookie 下发）。
function readCsrfFromCookie(): string | null {
  for (const item of document.cookie.split(';')) {
    const [name, ...rest] = item.trim().split('=')
    if (name === 'XSRF-TOKEN' || name === 'csrf_token' || name === 'csrftoken') {
      return decodeURIComponent(rest.join('='))
    }
  }
  return null
}

// 认证 / 初始化状态（session cookie 模式，不做 localStorage token）。
export const useAuthStore = defineStore('auth', () => {
  // null 表示尚未探测
  const initialized = ref<boolean | null>(null)
  const loggedIn = ref(false)
  const csrfToken = ref<string | null>(null)

  const isBootstrapped = computed(() => initialized.value !== null)

  function setCsrfToken(token: string) {
    csrfToken.value = token
  }

  function clear() {
    loggedIn.value = false
  }

  async function bootstrap() {
    if (!csrfToken.value) {
      csrfToken.value = readCsrfFromCookie()
    }
    try {
      const { data } = await api.get<{ initialized: boolean }>('/setup/status')
      initialized.value = !!data.initialized
    } catch {
      // 探测失败视为未初始化（后续请求会给出具体错误提示）
      initialized.value = false
    }

    if (initialized.value) {
      try {
        await api.get('/auth/me', { skipAuthRedirect: true })
        loggedIn.value = true
      } catch {
        loggedIn.value = false
      }
    } else {
      loggedIn.value = false
    }
  }

  async function login(password: string): Promise<boolean> {
    const { data } = await api.post<{ ok: boolean }>('/auth/login', { password })
    const ok = !!data.ok
    loggedIn.value = ok
    return ok
  }

  async function logout() {
    try {
      await api.post('/auth/logout')
    } catch {
      // 忽略登出接口错误
    } finally {
      clear()
    }
  }

  async function setup(password: string): Promise<boolean> {
    await api.post('/setup/complete', { password })
    initialized.value = true
    loggedIn.value = true
    return true
  }

  return {
    initialized,
    loggedIn,
    csrfToken,
    isBootstrapped,
    setCsrfToken,
    clear,
    bootstrap,
    login,
    logout,
    setup,
  }
})
