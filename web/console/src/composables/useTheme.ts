import { ref } from 'vue'

const KEY = 'nexus_theme'
const dark = ref(document.documentElement.classList.contains('dark'))

function apply(v: boolean) {
  dark.value = v
  document.documentElement.classList.toggle('dark', v)
  localStorage.setItem(KEY, v ? 'dark' : 'light')
}

export function useTheme() {
  return {
    dark,
    setTheme: apply,
    toggleTheme: () => apply(!dark.value),
  }
}
