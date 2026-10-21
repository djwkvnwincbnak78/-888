<template>
  <div class="space-y-4 md:space-y-6">
    <div v-if="loading && !updatedAt" class="rounded-2xl border border-[#1e2230] bg-[#0f1014]/80 p-6">
      <el-skeleton :rows="8" animated />
    </div>
    <template v-else>
    <div class="flex items-center justify-between gap-3">
      <div>
        <h2 class="nx-title">数据概览</h2>
        <p class="text-xs text-slate-500 mt-1">近 7 天网关运行指标</p>
      </div>
      <div class="flex items-center gap-2">
        <span class="hidden sm:inline text-[11px] text-slate-500">{{ updatedAt }}</span>
        <el-button :loading="loading" @click="load">
          <el-icon class="mr-1"><Refresh /></el-icon>刷新
        </el-button>
      </div>
    </div>

    <div class="grid grid-cols-1 sm:grid-cols-2 xl:grid-cols-3 gap-3 md:gap-4">
      <div v-for="card in cards" :key="card.label"
           class="rounded-2xl border border-[#1e2230] bg-[#0f1014]/80 backdrop-blur p-4 md:p-5 min-w-0">
        <div class="flex items-center justify-between">
          <span class="text-xs text-slate-500">{{ card.label }}</span>
          <el-icon :size="18" class="text-violet-400"><component :is="card.icon" /></el-icon>
        </div>
        <div class="mt-2 text-2xl md:text-3xl font-semibold tracking-tight break-all">{{ card.value }}</div>
        <div class="text-[11px] text-slate-600 mt-1">{{ card.hint }}</div>
      </div>
    </div>

    <div class="grid grid-cols-1 xl:grid-cols-2 gap-3 md:gap-4">
      <div class="rounded-2xl border border-[#1e2230] bg-[#0f1014]/80 backdrop-blur p-4 min-w-0">
        <div class="flex items-center justify-between mb-2">
          <span class="text-sm text-slate-300">请求趋势</span>
          <span class="text-[11px] text-slate-500">7 天</span>
        </div>
        <div ref="lineEl" class="w-full h-56 md:h-72"></div>
      </div>
      <div class="rounded-2xl border border-[#1e2230] bg-[#0f1014]/80 backdrop-blur p-4 min-w-0">
        <div class="flex items-center justify-between mb-2">
          <span class="text-sm text-slate-300">模型 Token 占比</span>
          <span class="text-[11px] text-slate-500">Top 8</span>
        </div>
        <div ref="pieEl" class="w-full h-56 md:h-72"></div>
      </div>
    </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import * as echarts from 'echarts'
import { ElMessage } from 'element-plus'
import { nexusApi, type Dashboard } from '@/api/nexus'

const loading = ref(false)
const updatedAt = ref('')
const data = ref<Dashboard>({ requests: 0, tokens: 0, cost: 0, series: [], models: [] })

const lineEl = ref<HTMLDivElement | null>(null)
const pieEl = ref<HTMLDivElement | null>(null)
let lineChart: echarts.ECharts | null = null
let pieChart: echarts.ECharts | null = null
let ro: ResizeObserver | null = null

const cards = computed(() => [
  { label: '近 7 天请求', value: fmt(data.value.requests), hint: '总调用次数', icon: 'DataLine' },
  { label: '消耗 Token', value: fmt(data.value.tokens), hint: 'prompt + completion', icon: 'Coin' },
  { label: '计费金额', value: '$' + Number(data.value.cost || 0).toFixed(6), hint: '按 price_version 结算', icon: 'Wallet' },
])

function fmt(n: number) { return Number(n || 0).toLocaleString() }

async function load() {
  loading.value = true
  try {
    data.value = await nexusApi.dashboard()
    updatedAt.value = new Date().toLocaleTimeString()
    await nextTick()
    render()
  } catch (e: any) {
    ElMessage.error(e?.message || '加载失败')
  } finally {
    loading.value = false
  }
}

function render() {
  if (lineEl.value) {
    lineChart = lineChart || echarts.init(lineEl.value)
    lineChart.setOption({
      backgroundColor: 'transparent',
      grid: { left: 40, right: 16, top: 20, bottom: 30 },
      tooltip: { trigger: 'axis', backgroundColor: '#12141d', borderColor: '#1e2230', textStyle: { color: '#e5e7eb' } },
      xAxis: {
        type: 'category',
        data: (data.value.series || []).map(p => p.date.slice(5)),
        axisLine: { lineStyle: { color: '#334155' } },
        axisLabel: { color: '#94a3b8', fontSize: 10 },
      },
      yAxis: {
        type: 'value',
        splitLine: { lineStyle: { color: '#1e2230' } },
        axisLabel: { color: '#94a3b8', fontSize: 10 },
      },
      series: [{
        type: 'line', smooth: true, symbol: 'circle', symbolSize: 5,
        data: (data.value.series || []).map(p => p.requests),
        lineStyle: { width: 2, color: '#8b5cf6' },
        itemStyle: { color: '#a78bfa' },
        areaStyle: {
          color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
            { offset: 0, color: 'rgba(139,92,246,0.35)' },
            { offset: 1, color: 'rgba(139,92,246,0)' },
          ]),
        },
      }],
    }, true)
  }

  if (pieEl.value) {
    pieChart = pieChart || echarts.init(pieEl.value)
    pieChart.setOption({
      backgroundColor: 'transparent',
      tooltip: { trigger: 'item', backgroundColor: '#12141d', borderColor: '#1e2230', textStyle: { color: '#e5e7eb' } },
      legend: { bottom: 0, textStyle: { color: '#94a3b8', fontSize: 10 } },
      color: ['#8b5cf6', '#22d3ee', '#f472b6', '#f59e0b', '#34d399', '#60a5fa', '#a78bfa', '#fb7185'],
      series: [{
        type: 'pie',
        radius: ['44%', '70%'],
        center: ['50%', '44%'],
        avoidLabelOverlap: true,
        itemStyle: { borderColor: '#0f1014', borderWidth: 2 },
        label: { color: '#cbd5e1', fontSize: 10 },
        data: (data.value.models || []).map(m => ({ name: m.model, value: m.tokens })),
      }],
    }, true)
  }
}

onMounted(() => {
  load()
  ro = new ResizeObserver(() => {
    lineChart?.resize()
    pieChart?.resize()
  })
  if (lineEl.value) ro.observe(lineEl.value)
  if (pieEl.value) ro.observe(pieEl.value)
})

onBeforeUnmount(() => {
  ro?.disconnect()
  lineChart?.dispose()
  pieChart?.dispose()
})
</script>
