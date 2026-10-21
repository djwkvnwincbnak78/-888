<template>
  <router-view />
</template>

<script setup lang="ts">
import { onMounted } from 'vue'
import { useAuthStore } from '@/store/auth'

onMounted(() => {
  // Restore theme preference and session on cold start.
  const theme = localStorage.getItem('nexus_theme') || 'dark'
  document.documentElement.classList.toggle('dark', theme === 'dark')
  useAuthStore().restore()
})
</script>
