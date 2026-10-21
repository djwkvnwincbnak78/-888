/**
 * Nexus Control Plane API client.
 * Drop into the template at src/api/nexus.ts
 *
 * Contract: the gateway exposes /admin/api/* behind JWT (Bearer) or X-Admin-Key.
 */
import axios, { AxiosError } from 'axios'

const BASE = import.meta.env.VITE_API_BASE || '/admin/api'
export const TOKEN_KEY = 'nexus_jwt'

export const http = axios.create({
  baseURL: BASE,
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
    const status = err.response?.status
    if (status === 401) {
      localStorage.removeItem(TOKEN_KEY)
      if (!location.pathname.endsWith('/login')) location.assign('/login')
    }
    const message = err.response?.data?.error?.message || err.message
    return Promise.reject(new Error(message))
  },
)

/* ------------------------------- types ---------------------------------- */

export interface LoginResult {
  token: string
  email: string
  role: 'owner' | 'admin' | 'member' | 'viewer'
  tenant: number
}

export interface DailyPoint {
  date: string
  requests: number
  tokens: number
}

export interface ModelSpend {
  model: string
  tokens: number
}

export interface Dashboard {
  requests: number
  tokens: number
  cost: number | string
  series: DailyPoint[]
  models: ModelSpend[]
}

export interface Channel {
  id: number
  name: string
  provider: 'openai' | 'anthropic' | 'ollama'
  base_url: string
  models: string[]
  weight: number
  enabled: boolean
  has_key: boolean
  created_at: string
}

export interface CreateChannelInput {
  name: string
  provider: Channel['provider']
  base_url: string
  api_key: string
  models: string[]
  weight?: number
}

export interface Token {
  id: number
  name: string
  prefix: string
  quota: number | string
  used: number | string
  status: 'active' | 'disabled'
  expires_at?: string
  created_at: string
}

/* ------------------------------- calls ---------------------------------- */

export const nexusApi = {
  /** POST /admin/api/login */
  async login(email: string, password: string): Promise<LoginResult> {
    const { data } = await http.post<LoginResult>('/login', { email, password })
    localStorage.setItem(TOKEN_KEY, data.token)
    return data
  },

  logout(): void {
    localStorage.removeItem(TOKEN_KEY)
  },

  /** GET /admin/api/dashboard */
  async dashboard(): Promise<Dashboard> {
    const { data } = await http.get<Dashboard>('/dashboard')
    return data
  },

  /** GET /admin/api/channels */
  async listChannels(): Promise<Channel[]> {
    const { data } = await http.get<{ data: Channel[] }>('/channels')
    return data.data ?? []
  },

  /** POST /admin/api/channels */
  async createChannel(input: CreateChannelInput): Promise<Channel> {
    const { data } = await http.post<Channel>('/channels', input)
    return data
  },

  /** PATCH /admin/api/channels/{id} */
  async setChannelEnabled(id: number, enabled: boolean): Promise<void> {
    await http.patch(`/channels/${id}`, { enabled })
  },

  /** DELETE /admin/api/channels/{id} */
  async deleteChannel(id: number): Promise<void> {
    await http.delete(`/channels/${id}`)
  },

  /** GET /admin/api/tokens */
  async listTokens(): Promise<Token[]> {
    const { data } = await http.get<{ data: Token[] }>('/tokens')
    return data.data ?? []
  },

  /** POST /admin/api/tokens — plaintext is returned exactly once. */
  async createToken(name: string, quota = '0', expiresDays = 0): Promise<{ token: string; id: number }> {
    const { data } = await http.post('/tokens', { name, quota, expires_days: expiresDays })
    return data
  },

  /** DELETE /admin/api/tokens/{id} */
  async revokeToken(id: number): Promise<void> {
    await http.delete(`/tokens/${id}`)
  },
}

export default nexusApi
