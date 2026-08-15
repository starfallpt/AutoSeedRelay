import { defineStore } from 'pinia'
import { ref, computed } from 'vue'

const TOKEN_KEY = 'autoseedrelay_token'

// 登录态暂时以 localStorage 占位，后续切换为后端 cookie session。
export const useAuthStore = defineStore('auth', () => {
  const token = ref<string | null>(localStorage.getItem(TOKEN_KEY))

  const isLoggedIn = computed(() => !!token.value)

  function setToken(value: string) {
    token.value = value
    localStorage.setItem(TOKEN_KEY, value)
  }

  function clear() {
    token.value = null
    localStorage.removeItem(TOKEN_KEY)
  }

  return { token, isLoggedIn, setToken, clear }
})
