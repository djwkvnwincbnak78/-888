import { defineStore } from 'pinia'
import { nexusApi, type LoginResult } from '@/api/nexus'
import { TOKEN_KEY } from '@/api/client'

interface State {
  token: string
  email: string
  role: string
  tenant: number
}

export const useAuthStore = defineStore('auth', {
  state: (): State => ({ token: '', email: '', role: '', tenant: 0 }),
  getters: {
    isAuthed: (s) => Boolean(s.token),
  },
  actions: {
    restore() {
      this.token = localStorage.getItem(TOKEN_KEY) || ''
      this.email = localStorage.getItem('nexus_email') || ''
      this.role = localStorage.getItem('nexus_role') || ''
      this.tenant = Number(localStorage.getItem('nexus_tenant') || 0)
    },
    async login(email: string, password: string): Promise<LoginResult> {
      const res = await nexusApi.login(email, password)
      this.token = res.token
      this.email = res.email
      this.role = res.role
      this.tenant = res.tenant
      localStorage.setItem(TOKEN_KEY, res.token)
      localStorage.setItem('nexus_email', res.email)
      localStorage.setItem('nexus_role', res.role)
      localStorage.setItem('nexus_tenant', String(res.tenant))
      return res
    },
    logout() {
      this.token = ''
      this.email = ''
      this.role = ''
      this.tenant = 0
      localStorage.removeItem(TOKEN_KEY)
      localStorage.removeItem('nexus_email')
      localStorage.removeItem('nexus_role')
      localStorage.removeItem('nexus_tenant')
    },
  },
})
