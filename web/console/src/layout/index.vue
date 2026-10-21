<template>
  <div class="h-screen flex overflow-hidden">
    <aside
      v-if="!isMobile"
      class="shrink-0 transition-[width] duration-200 ease-out"
      :style="{ width: collapsed ? '76px' : '240px' }"
    >
      <Sidebar :collapse="collapsed" @navigate="noop" />
    </aside>

    <el-drawer
      v-model="drawer"
      direction="ltr"
      size="80%"
      :with-header="false"
      class="md:hidden p-0"
    >
      <Sidebar :collapse="false" @navigate="drawer = false" />
    </el-drawer>

    <div class="flex-1 min-w-0 flex flex-col">
      <Navbar :collapsed="collapsed" @toggle="onToggle" />
      <main class="flex-1 overflow-y-auto p-4 md:p-6">
        <router-view v-slot="{ Component }">
          <transition name="fade" mode="out-in">
            <component :is="Component" />
          </transition>
        </router-view>
      </main>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import Sidebar from './components/Sidebar.vue'
import Navbar from './components/Navbar.vue'

const collapsed = ref(false)
const drawer = ref(false)
const isMobile = ref(false)
let mq: MediaQueryList | null = null
const onMqChange = (e: MediaQueryListEvent) => apply(e.matches)

function apply(matches: boolean) {
  isMobile.value = matches
  if (!matches) drawer.value = false
}

function onToggle() {
  if (isMobile.value) {
    drawer.value = true
  } else {
    collapsed.value = !collapsed.value
  }
}

function noop() {}

onMounted(() => {
  mq = window.matchMedia('(max-width: 768px)')
  apply(mq.matches)
  mq.addEventListener('change', onMqChange)
})

onBeforeUnmount(() => {
  mq?.removeEventListener('change', onMqChange)
})
</script>

<style scoped>
.fade-enter-active, .fade-leave-active { transition: opacity .16s ease; }
.fade-enter-from, .fade-leave-to { opacity: 0; }
</style>
