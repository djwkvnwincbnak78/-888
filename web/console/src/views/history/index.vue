<template>
  <div class="space-y-4">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div>
        <h2 class="nx-title">请求历史</h2>
        <p class="text-xs text-slate-500 mt-1">完整 Prompt 存档 · 一键重放</p>
      </div>
      <div class="flex items-center gap-2">
        <el-input v-model="keyword" placeholder="搜索模型 / 状态" clearable class="w-40 sm:w-56">
          <template #prefix><el-icon><Search /></el-icon></template>
        </el-input>
        <el-button :loading="loading" @click="load"><el-icon><Refresh /></el-icon></el-button>
      </div>
    </div>

    <div class="rounded-2xl border border-[#1e2230] bg-[#0f1014]/80 backdrop-blur overflow-hidden">
      <el-table :data="filtered" v-loading="loading" row-key="id" empty-text="暂无请求记录">
        <el-table-column label="时间" :width="isMobile ? 92 : 160">
          <template #default="{ row }">
            <span class="text-xs text-slate-400">{{ isMobile ? shortTime(row.request_time) : new Date(row.request_time).toLocaleString() }}</span>
          </template>
        </el-table-column>
        <el-table-column prop="model_name" label="模型" :min-width="isMobile ? 96 : 150" show-overflow-tooltip />
        <el-table-column v-if="!isMobile" prop="provider" label="Provider" width="110" />
        <el-table-column label="状态" :width="isMobile ? 82 : 100">
          <template #default="{ row }">
            <span v-if="isMobile" class="text-xs" :class="statusClass(row.status)">{{ row.status }}</span>
            <el-tag v-else size="small" effect="dark" :type="row.status === 'success' ? 'success' : row.status === 'cache_hit' ? 'info' : 'danger'">
              {{ row.status }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column v-if="!isMobile" label="Tokens" width="100">
          <template #default="{ row }">{{ row.prompt_tokens + row.completion_tokens }}</template>
        </el-table-column>
        <el-table-column label="延迟" width="90">
          <template #default="{ row }">{{ row.latency_ms }}ms</template>
        </el-table-column>
        <el-table-column label="费用" width="110">
          <template #default="{ row }">${{ Number(row.cost || 0).toFixed(6) }}</template>
        </el-table-column>
        <el-table-column label="操作" :width="isMobile ? 48 : 90" align="right">
          <template #default="{ row }">
            <el-button link type="primary" @click="open(row)">查看</el-button>
          </template>
        </el-table-column>
      </el-table>
    </div>

    <el-drawer v-model="drawer" title="请求详情" :size="isMobile ? '100%' : '560px'"
               :close-on-click-modal="false" destroy-on-close>
      <div v-if="current" class="space-y-4">
        <div class="grid grid-cols-2 gap-2 text-xs">
          <div class="rounded-lg bg-[#09090b] border border-[#1e2230] p-3">
            <div class="text-slate-500">模型</div>
            <div class="text-slate-200 mt-1 break-all">{{ current.model_name }}</div>
          </div>
          <div class="rounded-lg bg-[#09090b] border border-[#1e2230] p-3">
            <div class="text-slate-500">延迟 / 费用</div>
            <div class="text-slate-200 mt-1">{{ current.latency_ms }}ms · ${{ Number(current.cost || 0).toFixed(6) }}</div>
          </div>
        </div>

        <div>
          <div class="text-xs text-slate-500 mb-1">完整 Prompt / 参数</div>
          <pre class="text-[11px] leading-relaxed bg-[#09090b] border border-[#1e2230] rounded-lg p-3 max-h-[46vh] overflow-auto whitespace-pre-wrap break-all">{{ prettyBody }}</pre>
        </div>

        <div class="space-y-2">
          <div class="text-xs text-slate-500">重放到模型</div>
          <el-input v-model="replayModel" placeholder="留空则使用原模型" clearable />
          <el-button type="primary" class="w-full" :loading="replaying" @click="doReplay">
            <el-icon class="mr-1"><VideoPlay /></el-icon>一键重放
          </el-button>
        </div>
      </div>
    </el-drawer>

    <el-dialog v-model="resultDialog" title="重放结果" :width="isMobile ? '92vw' : '640px'">
      <div v-if="result" class="space-y-3">
        <div class="flex flex-wrap gap-2 text-xs">
          <el-tag effect="dark">{{ result.model }}</el-tag>
          <el-tag effect="dark" type="info">{{ result.latency_ms }}ms</el-tag>
          <el-tag effect="dark" type="success">channel {{ result.channel_id }}</el-tag>
        </div>
        <div class="text-xs text-slate-500">回复内容</div>
        <pre class="text-xs leading-relaxed bg-[#09090b] border border-[#1e2230] rounded-lg p-3 max-h-[50vh] overflow-auto whitespace-pre-wrap break-all">{{ replayText }}</pre>
      </div>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { nexusApi, type HistoryRow, type ReplayResult } from '@/api/nexus'
import { useIsMobile } from '@/composables/useIsMobile'

const loading = ref(false)
const replaying = ref(false)
const keyword = ref('')
const rows = ref<HistoryRow[]>([])
const drawer = ref(false)
const current = ref<HistoryRow | null>(null)
const replayModel = ref('')
const resultDialog = ref(false)
const result = ref<ReplayResult | null>(null)
const { isMobile } = useIsMobile()

const filtered = computed(() => {
  const k = keyword.value.trim().toLowerCase()
  if (!k) return rows.value
  return rows.value.filter(r => r.model_name.toLowerCase().includes(k) || r.status.toLowerCase().includes(k))
})

const prettyBody = computed(() => {
  const raw = current.value?.request_body || ''
  if (!raw) return '（该记录没有保存请求体）'
  try { return JSON.stringify(JSON.parse(raw), null, 2) } catch { return raw }
})

const replayText = computed(() => {
  const r = result.value?.response
  if (!r) return ''
  const content = r?.choices?.[0]?.message?.content
  return typeof content === 'string' && content ? content : JSON.stringify(r, null, 2)
})

function statusClass(v: string) {
  if (v === 'success') return 'text-emerald-500'
  if (v === 'cache_hit') return 'text-slate-500'
  return 'text-rose-500'
}

function shortTime(v: string) {
  const d = new Date(v)
  return `${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')} ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
}

async function load() {
  loading.value = true
  try {
    rows.value = await nexusApi.history(100)
  } catch (e: any) {
    ElMessage.error(e?.message || '加载失败')
  } finally {
    loading.value = false
  }
}

function open(row: HistoryRow) {
  current.value = row
  replayModel.value = ''
  // Let the click event finish bubbling before the overlay mounts, otherwise
  // Element's outside-click handler sees the same click and closes it.
  nextTick(() => { drawer.value = true })
}

async function doReplay() {
  if (!current.value || replaying.value) return
  replaying.value = true
  try {
    result.value = await nexusApi.replay({
      request_id: current.value.request_id,
      model: replayModel.value.trim() || undefined,
    })
    resultDialog.value = true
  } catch (e: any) {
    ElMessage.error(e?.message || '重放失败')
  } finally {
    replaying.value = false
  }
}

onMounted(load)
</script>
