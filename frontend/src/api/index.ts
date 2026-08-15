import axios, { AxiosError, type InternalAxiosRequestConfig } from 'axios'
import { ElMessage } from 'element-plus'
import router from '../router'
import { useAuthStore } from '../stores/auth'
import type { ApiErrorBody } from './types'

// 扩展 axios 请求配置：允许单次请求跳过认证重定向（用于 bootstrap 探测）。
declare module 'axios' {
  export interface AxiosRequestConfig {
    skipAuthRedirect?: boolean
  }
}

// 统一 axios 实例：baseURL 为 /api/v2，由 dev proxy 转发到后端（默认 :9020）。
const api = axios.create({
  baseURL: '/api/v2',
  timeout: 15000,
  withCredentials: true,
})

// 请求拦截器：从 auth store 读取 CSRF token 并注入 X-CSRF-Token 头。
api.interceptors.request.use((config: InternalAxiosRequestConfig) => {
  const auth = useAuthStore()
  if (auth.csrfToken) {
    config.headers.set('X-CSRF-Token', auth.csrfToken)
  }
  return config
})

// 响应拦截器：捕获 CSRF token；统一处理 401 / 403 / {error}。
api.interceptors.response.use(
  (response) => {
    const token = response.headers['x-csrf-token']
    if (typeof token === 'string' && token) {
      useAuthStore().setCsrfToken(token)
    }
    return response
  },
  (error: AxiosError<ApiErrorBody>) => {
    const status = error.response?.status
    const body = error.response?.data
    const skip = error.config?.skipAuthRedirect
    const auth = useAuthStore()

    if (status === 401) {
      auth.clear()
      if (!skip) {
        ElMessage.error('未登录或会话已过期')
        if (router.currentRoute.value.path !== '/login') {
          router.push({ path: '/login' })
        }
      }
    } else if (status === 403) {
      if (!skip) {
        ElMessage.error(body?.error ?? '系统尚未初始化')
        if (router.currentRoute.value.path !== '/setup') {
          router.push({ path: '/setup' })
        }
      }
    } else if (body?.error) {
      ElMessage.error(body.error)
    } else if (error.code === 'ECONNABORTED') {
      ElMessage.error('请求超时')
    } else {
      ElMessage.error(error.message ?? '请求失败')
    }

    return Promise.reject(error)
  },
)

export default api
