<template>
  <div class="space-y-4">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div>
        <h2 class="nx-title">影子对比</h2>
        <p class="text-xs text-slate-500 mt-1">主链路与影子模型延迟 / 成本对照</p>
      </div>
      <el-button :loading="loading" @click="load"><el-icon><Refresh /></el-icon></el-button>
    </div>

    <div class="grid grid-cols-2 lg:grid-cols-4 gap-3">
      <div class="rounded-2xl border border-[#1e2230] bg-[#0f1014]/80 backdrop-blur p-4">
        <div class="text-xs text-slate-500">样本数</div>
        <div class="text-2xl font-semibold mt-1">{{ rows.length }}</div>
      </div>
      <div class="rounded-2xl border border-[#1e2230] bg-[#0f1014]/80 backdrop-blur p-4">
        <div class="text-xs text-slate-500">主链路平均延迟</div>
        <div class="text-2xl font-semibold mt-1">{{ avgMain }}<span class="text-sm text-slate-500">ms</span></div>
      </div>
      <div class="rounded-2xl border border-[#1e2230] bg-[#0f1014]/80 backdrop-blur p-4">
        <div class="text-xs text-slate-500">影子平均延迟</div>
        <div class="text-2xl font-semibold mt-1">{{ avgShadow }}<span class="text-sm text-slate-500">ms</span></div>
      </div>
      <div class="rounded-2xl border border-[#1e2230] bg-[#0f1014]/80 backdrop-blur p-4">
        <div class="text-xs text-slate-500">成本差（影 − 主）</div>
        <div class="text-2xl font-semibold mt-1" :class="costDelta >= 0 ? 'text-rose-300' : 'text-emerald-300'">
          ${{ costDelta.toFixed(6) }}
        </div>
      </div>
    </div>

    <div class="rounded-2xl border border-[#1e2230] bg-[#0f1014]/80 backdrop-blur overflow-hidden">
      <el-table :data="rows" v-loading="loading" row-key="id" empty-text="暂无影子样本（在系统设置中开启影子测试并指定影子模型）">
        <el-table-column label="时间" width="160">
          <template #default="{ row }">
            <span class="text-xs text-slate-400">{{ new Date(row.created_at).toLocaleString() }}</span>
          </template>
        </el-table-column>
        <el-table-column label="主 → 影子" min-width="210">
          <template #default="{ row }">
            <span class="text-xs">{{ row.main_model }}</span>
            <span class="text-slate-600 mx-1">→</span>
            <span class="text-xs text-violet-300">{{ row.shadow_model }}</span>
          </template>
        </el-table-column>
        <el-table-column label="延迟对比" min-width="190">
          <template #default="{ row }">
            <div class="flex items-center gap-2 text-[11px]">
              <span class="text-slate-300 w-14">{{ row.main_latency }}ms</span>
              <span class="text-slate-600">/</span>
              <span :class="row.shadow_latency > row.main_latency ? 'text-rose-300' : 'text-emerald-300'">
                {{ row.shadow_latency }}ms
              </span>
            </div>
            <div class="h-1.5 rounded bg-[#1e2230] mt-1 overflow-hidden">
              <div class="h-full bg-violet-500"
                   :style="{ width: barWidth(row.main_latency, row.shadow_latency) + '%' }"></div>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="成本对比" width="160">
          <template #default="{ row }">
            <div class="text-[11px] text-slate-300">${{ Number(row.main_cost).toFixed(6) }}</div>
            <div class="text-[11px]" :class="Number(row.shadow_cost) > Number(row.main_cost) ? 'text-rose-300' : 'text-emerald-300'">
              ${{ Number(row.shadow_cost).toFixed(6) }}
            </div>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="90">
          <template #default="{ row }">
            <el-tag size="small" effect="dark" :type="row.status === 'ok' ? 'success' : 'danger'">{{ row.status }}</el-tag>
          </template>
        </el-table-column>
      </el-table>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { nexusApi, type ShadowRow } from '@/api/nexus'

const loading = ref(false)
const rows = ref<ShadowRow[]>([])

const avgMain = computed(() => avg(rows.value.map(r => r.main_latency)))
const avgShadow = computed(() => avg(rows.value.map(r => r.shadow_latency)))
const costDelta = computed(() => rows.value.reduce(
  (acc, r) => acc + Number(r.shadow_cost || 0) - Number(r.main_cost || 0), 0))

function avg(xs: number[]) {
  if (!xs.length) return 0
  return Math.round(xs.reduce((a, b) => a + b, 0) / xs.length)
}

function barWidth(main: number, shadow: number) {
  const max = Math.max(main, shadow, 1)
  return Math.round((main / max) * 100)
}

async function load() {
  loading.value = true
  try {
    rows.value = await nexusApi.shadowLogs(200)
  } catch (e: any) {
    ElMessage.error(e?.message || '加载失败')
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>
