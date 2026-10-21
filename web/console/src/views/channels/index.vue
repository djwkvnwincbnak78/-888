<template>
  <div class="space-y-4">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div>
        <h2 class="nx-title">渠道管理</h2>
        <p class="text-xs text-slate-500 mt-1">上游渠道与密钥（AES-256-GCM 加密存储）</p>
      </div>
      <div class="flex items-center gap-2 w-full sm:w-auto">
        <el-input v-model="keyword" placeholder="搜索名称 / 模型" clearable class="flex-1 sm:w-64">
          <template #prefix><el-icon><Search /></el-icon></template>
        </el-input>
        <el-button @click="load"><el-icon><Refresh /></el-icon></el-button>
        <el-button type="primary" @click="openCreate">
          <el-icon class="mr-1"><Plus /></el-icon>添加渠道
        </el-button>
      </div>
    </div>

    <div class="rounded-2xl border border-[#1e2230] bg-[#0f1014]/80 backdrop-blur overflow-hidden">
      <el-table :data="paged" v-loading="loading" class="w-full" row-key="id" empty-text="暂无渠道">
        <el-table-column prop="id" label="ID" width="70" />
        <el-table-column prop="name" label="名称" min-width="160" show-overflow-tooltip />
        <el-table-column prop="provider" label="Provider" width="120">
          <template #default="{ row }">
            <el-tag size="small" effect="dark" type="info">{{ row.provider }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="模型" min-width="200">
          <template #default="{ row }">
            <span class="text-xs text-slate-400">{{ (row.models || []).join(', ') }}</span>
          </template>
        </el-table-column>
        <el-table-column v-if="!isMobile" prop="weight" label="权重" width="80" />
        <el-table-column label="状态" width="110">
          <template #default="{ row }">
            <el-switch
              :model-value="row.enabled"
              :loading="row._busy"
              @change="(v) => toggle(row, Boolean(v))"
            />
          </template>
        </el-table-column>
        <el-table-column label="操作" width="110" align="right" :fixed="isMobile ? false : 'right'">
          <template #default="{ row }">
            <el-button link type="danger" @click="remove(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>

      <div class="flex justify-end p-3 border-t border-[#1e2230]">
        <el-pagination
          v-model:current-page="page"
          :page-size="pageSize"
          :total="filtered.length"
          layout="prev, pager, next"
          background
          small
        />
      </div>
    </div>

    <el-dialog v-model="dialog" title="添加渠道" width="560px" :close-on-click-modal="false">
      <el-form ref="formRef" :model="form" :rules="rules" label-position="top">
        <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
          <el-form-item label="名称" prop="name">
            <el-input v-model="form.name" placeholder="openai-main" />
          </el-form-item>
          <el-form-item label="Provider" prop="provider">
            <el-select v-model="form.provider" class="w-full">
              <el-option label="OpenAI" value="openai" />
              <el-option label="Anthropic" value="anthropic" />
              <el-option label="Ollama" value="ollama" />
            </el-select>
          </el-form-item>
        </div>
        <el-form-item label="Base URL" prop="base_url">
          <el-input v-model="form.base_url" placeholder="https://api.openai.com/v1" />
        </el-form-item>
        <el-form-item label="API Key" prop="api_key">
          <el-input v-model="form.api_key" type="password" show-password placeholder="sk-..." />
        </el-form-item>
        <el-form-item label="模型（逗号分隔）" prop="modelsText">
          <el-input v-model="form.modelsText" placeholder="gpt-4o, gpt-4o-mini" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialog = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="submit">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox, type FormInstance, type FormRules } from 'element-plus'
import { nexusApi, type Channel, type ChannelInput, type Provider } from '@/api/nexus'
import { useIsMobile } from '@/composables/useIsMobile'

const { isMobile } = useIsMobile()
const loading = ref(false)
const saving = ref(false)
const keyword = ref('')
const page = ref(1)
const pageSize = 10
const rows = ref<(Channel & { _busy?: boolean })[]>([])

const dialog = ref(false)
const formRef = ref<FormInstance>()
const form = reactive({
  name: '',
  provider: 'openai' as Provider,
  base_url: 'https://api.openai.com/v1',
  api_key: '',
  modelsText: '',
})

const rules: FormRules = {
  name: [{ required: true, message: '请输入名称', trigger: 'blur' }],
  provider: [{ required: true, message: '请选择 provider', trigger: 'change' }],
  base_url: [{ required: true, message: '请输入 Base URL', trigger: 'blur' }],
  api_key: [{ required: true, message: '请输入 API Key', trigger: 'blur' }],
  modelsText: [{ required: true, message: '至少一个模型', trigger: 'blur' }],
}

const filtered = computed(() => {
  const k = keyword.value.trim().toLowerCase()
  if (!k) return rows.value
  return rows.value.filter(c =>
    c.name.toLowerCase().includes(k) || (c.models || []).some(m => m.toLowerCase().includes(k)))
})

const paged = computed(() => {
  const start = (page.value - 1) * pageSize
  return filtered.value.slice(start, start + pageSize)
})

async function load() {
  loading.value = true
  try {
    rows.value = await nexusApi.listChannels()
  } catch (e: any) {
    ElMessage.error(e?.message || '加载失败')
  } finally {
    loading.value = false
  }
}

function openCreate() {
  form.name = ''
  form.provider = 'openai'
  form.base_url = 'https://api.openai.com/v1'
  form.api_key = ''
  form.modelsText = ''
  dialog.value = true
}

async function submit() {
  if (!formRef.value || saving.value) return
  const ok = await formRef.value.validate().catch(() => false)
  if (!ok) return
  saving.value = true
  try {
    const input: ChannelInput = {
      name: form.name.trim(),
      provider: form.provider,
      base_url: form.base_url.trim(),
      api_key: form.api_key.trim(),
      models: form.modelsText.split(',').map(s => s.trim()).filter(Boolean),
    }
    await nexusApi.createChannel(input)
    ElMessage.success('渠道已创建，15 秒内生效')
    dialog.value = false
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '创建失败')
  } finally {
    saving.value = false
  }
}

async function toggle(row: Channel & { _busy?: boolean }, value: boolean) {
  row._busy = true
  try {
    await nexusApi.setChannelEnabled(row.id, value)
    row.enabled = value
    ElMessage.success(value ? '已启用' : '已禁用')
  } catch (e: any) {
    ElMessage.error(e?.message || '操作失败')
  } finally {
    row._busy = false
  }
}

async function remove(row: Channel) {
  try {
    await ElMessageBox.confirm(`确认删除渠道「${row.name}」？`, '删除确认', { type: 'warning' })
  } catch { return }
  try {
    await nexusApi.deleteChannel(row.id)
    ElMessage.success('已删除')
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '删除失败')
  }
}

onMounted(load)
</script>
