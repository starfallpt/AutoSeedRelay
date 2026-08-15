import axios from 'axios'

// 统一 axios 实例：baseURL 为 /api/v2，由 dev proxy 转发到后端（默认 :9020）。
const api = axios.create({
  baseURL: '/api/v2',
  timeout: 10000,
})

export default api
