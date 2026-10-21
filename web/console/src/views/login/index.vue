<template>
  <div class="min-h-screen flex items-center justify-center p-4">
    <div class="w-full max-w-[420px] rounded-2xl border border-[#1e2230] bg-[#0b0d14]/85 backdrop-blur-xl p-6 md:p-8 space-y-6 shadow-glow">
      <div class="flex items-center gap-3">
        <img :src="logo" alt="Nexus" class="w-11 h-11 rounded-xl" />
        <div>
          <h1 class="text-xl font-semibold tracking-tight">Nexus Control Plane</h1>
          <p class="text-xs text-slate-500">LLM gateway console</p>
        </div>
      </div>

      <el-form ref="formRef" :model="form" :rules="rules" label-position="top" @submit.prevent>
        <el-form-item label="邮箱" prop="email">
          <el-input v-model="form.email" size="large" placeholder="admin@nexus.local" autocomplete="username" />
        </el-form-item>
        <el-form-item label="密码" prop="password">
          <el-input v-model="form.password" type="password" size="large" show-password
                    placeholder="••••••••" autocomplete="current-password" @keyup.enter="submit" />
        </el-form-item>
        <el-button type="primary" size="large" class="w-full" :loading="loading" @click="submit">
          登录
        </el-button>
      </el-form>

      <p class="text-[11px] leading-relaxed text-slate-600">
        凭据仅用于签发 JWT，保存在浏览器 localStorage。
      </p>
    </div>
  </div>
</template>

<script setup lang="ts">
const logo = import.meta.env.BASE_URL + 'brand/nexus.svg'
import { reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { useAuthStore } from '@/store/auth'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

const formRef = ref<FormInstance>()
const loading = ref(false)
const form = reactive({ email: '', password: '' })

const rules: FormRules = {
  email: [{ required: true, message: '请输入邮箱', trigger: 'blur' }],
  password: [{ required: true, message: '请输入密码', trigger: 'blur' }],
}

async function submit() {
  if (!formRef.value) return
  const ok = await formRef.value.validate().catch(() => false)
  if (!ok) return
  loading.value = true
  try {
    await auth.login(form.email.trim(), form.password)
    ElMessage.success('登录成功')
    router.replace(String(route.query.redirect || '/dashboard'))
  } catch (e: any) {
    ElMessage.error(e?.message || '登录失败')
  } finally {
    loading.value = false
  }
}
</script>
