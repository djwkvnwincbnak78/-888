<template>
  <div class="space-y-4">
    <div class="flex items-center justify-between gap-3">
      <div>
        <h2 class="nx-title">系统设置</h2>
        <p class="text-xs text-slate-500 mt-1">动态配置中心 · 修改即时热更新，无需重启</p>
      </div>
      <el-button :loading="loading" @click="load">刷新</el-button>
    </div>

    <div class="grid grid-cols-1 lg:grid-cols-2 gap-3 md:gap-4">
      <div class="rounded-2xl border border-[#1e2230] bg-[#0f1014]/80 backdrop-blur p-5 space-y-4 lg:col-span-2">
        <div class="flex items-center gap-2">
          <el-icon class="text-violet-400"><MagicStick /></el-icon>
          <span class="text-sm text-slate-200">网关能力开关</span>
          <span class="text-[11px] text-slate-600">POST → 内存热更新</span>
        </div>

        <div v-if="!ready" class="grid grid-cols-1 md:grid-cols-2 gap-3">
          <el-skeleton v-for="n in 4" :key="n" :rows="1" animated class="rounded-xl border border-[#1e2230] p-4" />
        </div>
        <div v-else class="grid grid-cols-1 md:grid-cols-2 gap-3">
          <div v-for="item in switches" :key="item.key"
               class="rounded-xl border border-[#1e2230] bg-[#09090b]/60 p-4 flex items-center justify-between gap-3">
            <div class="min-w-0">
              <div class="text-sm text-slate-200">{{ item.label }}</div>
              <div class="text-[11px] text-slate-500 mt-0.5 leading-relaxed">{{ item.hint }}</div>
            </div>
            <el-switch
              :model-value="settings[item.key] === 'true'"
              :loading="saving === item.key"
              :disabled="loading"
              @change="(v) => toggle(item.key, Boolean(v))"
            />
          </div>
        </div>

        <div class="rounded-xl border border-[#1e2230] bg-[#09090b]/60 p-4 space-y-2">
          <div class="text-sm text-slate-200">影子模型</div>
          <div class="text-[11px] text-slate-500">主请求返回后异步复制到该模型，必须在渠道目录中存在。</div>
          <div class="flex gap-2">
            <el-input v-model="shadowModel" placeholder="例如 gpt-4o-mini" clearable class="flex-1" />
            <el-button type="primary" :loading="saving === 'shadow_model'" @click="saveShadowModel">保存</el-button>
          </div>
        </div>
      </div>

      <div class="rounded-2xl border border-[#1e2230] bg-[#0f1014]/80 backdrop-blur p-5 space-y-4">
        <div class="text-sm text-slate-300">外观</div>
        <div class="flex items-center justify-between">
          <div>
            <div class="text-sm text-slate-200">深色模式</div>
            <div class="text-xs text-slate-500">深空黑 + 电光紫</div>
          </div>
          <el-switch :model-value="dark" @change="setDark" />
        </div>
      </div>

      <div class="rounded-2xl border border-[#1e2230] bg-[#0f1014]/80 backdrop-blur p-5 space-y-4">
        <div class="flex items-center justify-between">
          <div class="text-sm text-slate-300">网关状态</div>
          <el-button size="small" :loading="checking" @click="checkHealth">检测</el-button>
        </div>
        <div class="flex items-center gap-2">
          <span class="w-2 h-2 rounded-full" :class="health.ok ? 'bg-emerald-400' : 'bg-rose-500'"></span>
          <span class="text-sm" :class="health.ok ? 'text-emerald-300' : 'text-rose-300'">
            {{ health.ok ? '在线' : '不可达' }}
          </span>
          <span class="text-xs text-slate-500" v-if="health.checked">{{ health.checked }}</span>
        </div>
        <pre v-if="health.body" class="text-[11px] leading-relaxed bg-[#09090b] border border-[#1e2230] rounded-lg p-3 overflow-x-auto">{{ health.body }}</pre>
      </div>

      <div class="rounded-2xl border border-[#1e2230] bg-[#0f1014]/80 backdrop-blur p-5 space-y-2">
        <div class="text-sm text-slate-300">接口</div>
        <code class="block text-xs bg-[#09090b] border border-[#1e2230] rounded p-2">/admin/api/settings</code>
        <code class="block text-xs bg-[#09090b] border border-[#1e2230] rounded p-2">/admin/api/replay</code>
        <code class="block text-xs bg-[#09090b] border border-[#1e2230] rounded p-2">/admin/api/shadow</code>
      </div>

      <div class="rounded-2xl border border-[#1e2230] bg-[#0f1014]/80 backdrop-blur p-5 space-y-2">
        <div class="text-sm text-slate-300">会话</div>
        <div class="text-sm text-slate-200 break-all">{{ auth.email || '—' }}</div>
        <div class="text-xs text-slate-500">{{ auth.role || '—' }} · tenant {{ auth.tenant || '—' }}</div>
        <el-button class="mt-2" type="danger" plain size="small" @click="logout">退出登录</el-button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { useAuthStore } from '@/store/auth'
import { useTheme } from '@/composables/useTheme'
import { nexusApi } from '@/api/nexus'

const router = useRouter()
const auth = useAuthStore()

const { dark, setTheme } = useTheme()
const checking = ref(false)
const loading = ref(false)
const ready = ref(false)
const saving = ref('')
const settings = reactive<Record<string, string>>({})
const shadowModel = ref('')
const health = reactive<{ ok: boolean; checked: string; body: string }>({ ok: false, checked: '', body: '' })

const switches = [
  { key: 'enable_compress', label: 'Prompt 压缩器', hint: '确定性去冗余，压缩后计费与缓存键一致' },
  { key: 'enable_semantic_cache', label: '语义缓存', hint: '向量相似度命中，按租户隔离' },
  { key: 'enable_shadow_test', label: '影子测试', hint: '主请求返回后异步复制到影子模型' },
  { key: 'replay_enabled', label: '请求重放', hint: '允许控制台重放历史 Prompt' },
]

async function load() {
  loading.value = true
  try {
    const res = await nexusApi.getSettings()
    Object.assign(settings, res.data)
    shadowModel.value = res.data.shadow_model || ''
  } catch (e: any) {
    ElMessage.error(e?.message || '配置加载失败')
  } finally {
    loading.value = false
    ready.value = true
  }
}

async function toggle(key: string, value: boolean) {
  const prev = settings[key]
  settings[key] = String(value)
  saving.value = key
  try {
    await nexusApi.updateSettings({ [key]: value })
    ElMessage.success('配置已实时生效')
  } catch (e: any) {
    settings[key] = prev
    ElMessage.error(e?.message || '保存失败')
  } finally {
    saving.value = ''
  }
}

async function saveShadowModel() {
  saving.value = 'shadow_model'
  try {
    await nexusApi.updateSettings({ shadow_model: shadowModel.value.trim() })
    ElMessage.success('影子模型已更新，实时生效')
  } catch (e: any) {
    ElMessage.error(e?.message || '保存失败')
  } finally {
    saving.value = ''
  }
}

function setDark(v: boolean | string | number) {
  setTheme(Boolean(v))
}

async function checkHealth() {
  checking.value = true
  try {
    const res = await fetch('/health', { headers: { Accept: 'application/json' } })
    health.ok = res.ok
    health.body = (await res.text()).slice(0, 600)
    health.checked = new Date().toLocaleTimeString()
  } catch (e: any) {
    health.ok = false
    health.body = String(e?.message || e)
    health.checked = new Date().toLocaleTimeString()
  } finally {
    checking.value = false
  }
}

function logout() {
  auth.logout()
  ElMessage.success('已退出')
  router.replace('/login')
}

onMounted(() => {
  load()
  checkHealth()
})
</script>
