<template>
  <header class="h-16 shrink-0 flex items-center gap-3 px-4 md:px-6 border-b border-[#1e2230] bg-[#09090b]/60 backdrop-blur-xl">
    <button class="p-2 rounded-lg hover:bg-white/5" @click="$emit('toggle')" aria-label="toggle sidebar">
      <el-icon :size="20"><Fold v-if="!collapsed" /><Expand v-else /></el-icon>
    </button>

    <el-breadcrumb separator="/" class="hidden sm:block">
      <el-breadcrumb-item :to="{ path: '/dashboard' }">Nexus</el-breadcrumb-item>
      <el-breadcrumb-item>{{ title }}</el-breadcrumb-item>
    </el-breadcrumb>

    <div class="ml-auto flex items-center gap-2">
      <el-tooltip :content="dark ? '切换亮色' : '切换暗色'">
        <button class="p-2 rounded-lg hover:bg-white/5" @click="toggleTheme">
          <el-icon :size="18"><Moon v-if="dark" /><Sunny v-else /></el-icon>
        </button>
      </el-tooltip>

      <el-dropdown trigger="click" @command="onCommand">
        <button class="flex items-center gap-2 px-2 py-1 rounded-lg hover:bg-white/5">
          <el-avatar :size="28" class="bg-violet-600">N</el-avatar>
          <span class="hidden md:inline text-sm text-slate-300 max-w-[180px] truncate">{{ email }}</span>
          <el-icon class="hidden md:inline"><ArrowDown /></el-icon>
        </button>
        <template #dropdown>
          <el-dropdown-menu>
            <el-dropdown-item command="settings">系统设置</el-dropdown-item>
            <el-dropdown-item divided command="logout">退出登录</el-dropdown-item>
          </el-dropdown-menu>
        </template>
      </el-dropdown>
    </div>
  </header>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useAuthStore } from '@/store/auth'
import { useTheme } from '@/composables/useTheme'

defineProps<{ collapsed: boolean }>()
defineEmits<{ (e: 'toggle'): void }>()

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

const title = computed(() => String(route.meta.title || '控制台'))
const email = computed(() => auth.email)
const { dark, toggleTheme } = useTheme()

function onCommand(cmd: string) {
  if (cmd === 'logout') {
    auth.logout()
    router.replace('/login')
  } else if (cmd === 'settings') {
    router.push('/settings')
  }
}
</script>
