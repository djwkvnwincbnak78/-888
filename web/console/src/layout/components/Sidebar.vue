<template>
  <div class="h-full flex flex-col bg-[#09090b]/80 backdrop-blur-xl border-r border-[#1e2230]">
    <div class="h-16 flex items-center gap-3 px-4 shrink-0">
      <img :src="logo" alt="Nexus" class="w-8 h-8 rounded-lg" />
      <div v-show="!collapse" class="min-w-0">
        <div class="font-semibold leading-tight truncate">Nexus</div>
        <div class="text-[11px] text-slate-500 truncate">Control Plane</div>
      </div>
    </div>

    <el-menu
      :default-active="active"
      :collapse="collapse"
      :collapse-transition="false"
      class="border-0 flex-1 overflow-y-auto"
      background-color="transparent"
      @select="onSelect"
    >
      <el-menu-item v-for="item in items" :key="item.path" :index="item.path">
        <el-icon><component :is="item.icon" /></el-icon>
        <template #title>{{ item.title }}</template>
      </el-menu-item>
    </el-menu>

    <div class="p-3 border-t border-[#1e2230]" v-show="!collapse">
      <div class="text-[11px] text-slate-600 break-all">{{ email }}</div>
    </div>
  </div>
</template>

<script setup lang="ts">
const logo = import.meta.env.BASE_URL + 'brand/nexus.svg'
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useAuthStore } from '@/store/auth'

defineProps<{ collapse: boolean }>()
const emit = defineEmits<{ (e: 'navigate'): void }>()

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const email = computed(() => auth.email)

const items = [
  { path: '/dashboard', title: '数据概览', icon: 'DataLine' },
  { path: '/channels', title: '渠道管理', icon: 'Connection' },
  { path: '/history', title: '请求历史', icon: 'Tickets' },
  { path: '/shadow', title: '影子对比', icon: 'Aim' },
  { path: '/tokens', title: '令牌管理', icon: 'Key' },
  { path: '/settings', title: '系统设置', icon: 'Setting' },
]

const active = computed(() => route.path)

function onSelect(path: string) {
  emit('navigate')
  if (path !== route.path) router.push(path)
}
</script>
