<template>
  <div class="space-y-4">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div>
        <h2 class="nx-title">令牌管理</h2>
        <p class="text-xs text-slate-500 mt-1">sk-nexus-* 令牌，仅在创建时显示一次</p>
      </div>
      <div class="flex items-center gap-2 w-full sm:w-auto">
        <el-input v-model="keyword" placeholder="搜索名称 / 前缀" clearable class="flex-1 sm:w-64">
          <template #prefix><el-icon><Search /></el-icon></template>
        </el-input>
        <el-button @click="load"><el-icon><Refresh /></el-icon></el-button>
        <el-button type="primary" @click="openCreate">
          <el-icon class="mr-1"><Plus /></el-icon>生成令牌
        </el-button>
      </div>
    </div>

    <div class="rounded-2xl border border-[#1e2230] bg-[#0f1014]/80 backdrop-blur overflow-hidden">
      <el-table :data="paged" v-loading="loading" row-key="id" empty-text="暂无令牌">
        <el-table-column prop="id" label="ID" width="70" />
        <el-table-column prop="name" label="名称" min-width="140" show-overflow-tooltip />
        <el-table-column label="前缀" min-width="180">
          <template #default="{ row }">
            <code class="text-xs text-slate-400">{{ row.prefix }}…</code>
          </template>
        </el-table-column>
        <el-table-column label="额度" width="110">
          <template #default="{ row }">
            {{ Number(row.quota) === 0 ? '不限' : Number(row.quota) }}
          </template>
        </el-table-column>
        <el-table-column label="已用" width="110">
          <template #default="{ row }">{{ Number(row.used).toFixed(4) }}</template>
        </el-table-column>
        <el-table-column label="状态" width="100">
          <template #default="{ row }">
            <el-tag :type="row.status === 'active' ? 'success' : 'info'" size="small" effect="dark">
              {{ row.status }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column v-if="!isMobile" label="过期" min-width="150">
          <template #default="{ row }">
            <span class="text-xs text-slate-400">{{ row.expires_at ? new Date(row.expires_at).toLocaleString() : '永久' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="100" align="right" :fixed="isMobile ? false : 'right'">
          <template #default="{ row }">
            <el-button link type="danger" :disabled="row.status !== 'active'" @click="revoke(row)">吊销</el-button>
          </template>
        </el-table-column>
      </el-table>

      <div class="flex justify-end p-3 border-t border-[#1e2230]">
        <el-pagination v-model:current-page="page" :page-size="pageSize" :total="filtered.length"
                       layout="prev, pager, next" background small />
      </div>
    </div>

    <el-dialog v-model="dialog" title="生成令牌" width="480px" :close-on-click-modal="false">
      <el-form ref="formRef" :model="form" :rules="rules" label-position="top">
        <el-form-item label="名称" prop="name">
          <el-input v-model="form.name" placeholder="production" />
        </el-form-item>
        <el-form-item label="额度（0 = 不限）" prop="quota">
          <el-input v-model="form.quota" placeholder="0" />
        </el-form-item>
        <el-form-item label="有效天数（0 = 永久）" prop="days">
          <el-input-number v-model="form.days" :min="0" :max="3650" class="w-full" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialog = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="submit">生成</el-button>
      </template>
    </el-dialog>

    <el-dialog v-model="resultDialog" title="令牌已生成" width="520px" :close-on-click-modal="false">
      <p class="text-sm text-amber-300 mb-2">该令牌只显示一次，关闭后无法再次查看。</p>
      <div class="rounded-lg border border-[#1e2230] bg-[#09090b] p-3 font-mono text-xs break-all select-all">
        {{ newToken }}
      </div>
      <template #footer>
        <el-button @click="copyToken">复制</el-button>
        <el-button type="primary" @click="resultDialog = false">我已保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox, type FormInstance, type FormRules } from 'element-plus'
import { nexusApi, type TokenRow } from '@/api/nexus'
import { useIsMobile } from '@/composables/useIsMobile'

const { isMobile } = useIsMobile()
const loading = ref(false)
const saving = ref(false)
const keyword = ref('')
const page = ref(1)
const pageSize = 10
const rows = ref<TokenRow[]>([])

const dialog = ref(false)
const resultDialog = ref(false)
const newToken = ref('')
const formRef = ref<FormInstance>()
const form = reactive({ name: 'default', quota: '0', days: 0 })

const rules: FormRules = {
  name: [{ required: true, message: '请输入名称', trigger: 'blur' }],
  quota: [{ required: true, message: '请输入额度（0 表示不限）', trigger: 'blur' }],
}

const filtered = computed(() => {
  const k = keyword.value.trim().toLowerCase()
  if (!k) return rows.value
  return rows.value.filter(t =>
    t.name.toLowerCase().includes(k) || t.prefix.toLowerCase().includes(k))
})

const paged = computed(() => {
  const start = (page.value - 1) * pageSize
  return filtered.value.slice(start, start + pageSize)
})

async function load() {
  loading.value = true
  try {
    rows.value = await nexusApi.listTokens()
  } catch (e: any) {
    ElMessage.error(e?.message || '加载失败')
  } finally {
    loading.value = false
  }
}

function openCreate() {
  form.name = 'default'
  form.quota = '0'
  form.days = 0
  dialog.value = true
}

async function submit() {
  if (!formRef.value || saving.value) return
  const ok = await formRef.value.validate().catch(() => false)
  if (!ok) return
  saving.value = true
  try {
    const res = await nexusApi.createToken(form.name.trim(), form.quota.trim() || '0', form.days)
    newToken.value = res.token
    dialog.value = false
    resultDialog.value = true
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '生成失败')
  } finally {
    saving.value = false
  }
}

async function copyToken() {
  try {
    await navigator.clipboard.writeText(newToken.value)
    ElMessage.success('已复制')
  } catch {
    ElMessage.warning('复制失败，请长按手动选择')
  }
}

async function revoke(row: TokenRow) {
  try {
    await ElMessageBox.confirm(`吊销令牌「${row.name}」（${row.prefix}…）？`, '吊销确认', { type: 'warning' })
  } catch { return }
  try {
    await nexusApi.revokeToken(row.id)
    ElMessage.success('已吊销')
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '吊销失败')
  }
}

onMounted(load)
</script>
