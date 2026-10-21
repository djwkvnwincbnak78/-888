import axios, { AxiosError } from 'axios'
import { ElMessage } from 'element-plus'

export const TOKEN_KEY = 'nexus_jwt'

const http = axios.create({
  baseURL: import.meta.env.VITE_API_BASE || '/admin/api',
  timeout: 20000,
  headers: { 'Content-Type': 'application/json' },
})

http.interceptors.request.use((config) => {
  const token = localStorage.getItem(TOKEN_KEY)
  if (token) config.headers.Authorization = `Bearer ${token}`
  return config
})

http.interceptors.response.use(
  (res) => res,
  (err: AxiosError<{ error?: { message?: string } }>) => {
    if (err.response?.status === 401) {
      localStorage.removeItem(TOKEN_KEY)
      if (!location.pathname.endsWith('/login')) {
        location.assign('/console/login')
      }
    }
    if (err.response && err.response.status >= 500) {
      ElMessage.error('服务器开小差了')
    }
    const msg = err.response?.data?.error?.message || err.message
    return Promise.reject(new Error(msg))
  },
)

export default http
