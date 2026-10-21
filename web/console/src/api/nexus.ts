import http from './client'

export interface LoginResult {
  token: string
  email: string
  role: 'owner' | 'admin' | 'member' | 'viewer'
  tenant: number
}

export interface DailyPoint { date: string; requests: number; tokens: number }
export interface ModelSpend { model: string; tokens: number }
export interface Dashboard {
  requests: number
  tokens: number
  cost: number | string
  series: DailyPoint[]
  models: ModelSpend[]
}

export type Provider = 'openai' | 'anthropic' | 'ollama'

export interface Channel {
  id: number
  name: string
  provider: Provider
  base_url: string
  models: string[]
  weight: number
  enabled: boolean
  has_key: boolean
  created_at: string
}

export interface ChannelInput {
  name: string
  provider: Provider
  base_url: string
  api_key: string
  models: string[]
  weight?: number
}

export interface TokenRow {
  id: number
  name: string
  prefix: string
  quota: number | string
  used: number | string
  status: 'active' | 'disabled'
  expires_at?: string
  created_at: string
}

export interface SettingItem { key: string; value: string; description: string }
export interface HistoryRow {
  id: number
  request_id: string
  model_name: string
  provider: string
  route_reason: string
  prompt_tokens: number
  completion_tokens: number
  cost: number | string
  status: string
  http_status: number
  latency_ms: number
  cached: boolean
  request_time: string
  request_body: string
}
export interface ShadowRow {
  id: number
  trace_id: string
  main_model: string
  shadow_model: string
  main_channel: number
  shadow_channel: number
  main_latency: number
  shadow_latency: number
  main_cost: number | string
  shadow_cost: number | string
  main_tokens: number
  shadow_tokens: number
  status: string
  error_message: string
  created_at: string
}
export interface ReplayResult {
  model: string
  channel_id: number
  route_reason: string
  latency_ms: number
  replayed_from: string
  response: any
}

export const nexusApi = {
  async login(email: string, password: string): Promise<LoginResult> {
    const { data } = await http.post<LoginResult>('/login', { email, password })
    return data
  },
  async dashboard(): Promise<Dashboard> {
    const { data } = await http.get<Dashboard>('/dashboard')
    return data
  },
  async listChannels(): Promise<Channel[]> {
    const { data } = await http.get<{ data: Channel[] }>('/channels')
    return data.data ?? []
  },
  async createChannel(input: ChannelInput): Promise<Channel> {
    const { data } = await http.post<Channel>('/channels', input)
    return data
  },
  async setChannelEnabled(id: number, enabled: boolean): Promise<void> {
    await http.patch(`/channels/${id}`, { enabled })
  },
  async deleteChannel(id: number): Promise<void> {
    await http.delete(`/channels/${id}`)
  },
  async listTokens(): Promise<TokenRow[]> {
    const { data } = await http.get<{ data: TokenRow[] }>('/tokens')
    return data.data ?? []
  },
  async createToken(name: string, quota: string, expiresDays: number): Promise<{ token: string; id: number }> {
    const { data } = await http.post('/tokens', { name, quota, expires_days: expiresDays })
    return data
  },
  async getSettings(): Promise<{ data: Record<string, string>; items: SettingItem[] }> {
    const { data } = await http.get('/settings')
    return data
  },
  async updateSettings(patch: Record<string, string | boolean | number>): Promise<void> {
    await http.put('/settings', patch)
  },
  async history(limit = 50): Promise<HistoryRow[]> {
    const { data } = await http.get<{ data: HistoryRow[] }>('/history', { params: { limit } })
    return data.data ?? []
  },
  async replay(payload: { request_id?: string; model?: string; messages?: unknown[] }): Promise<ReplayResult> {
    const { data } = await http.post<ReplayResult>('/replay', payload)
    return data
  },
  async shadowLogs(limit = 100): Promise<ShadowRow[]> {
    const { data } = await http.get<{ data: ShadowRow[] }>('/shadow', { params: { limit } })
    return data.data ?? []
  },
  async revokeToken(id: number): Promise<void> {
    await http.delete(`/tokens/${id}`)
  },
}

export default nexusApi
