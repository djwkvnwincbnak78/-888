import { onBeforeUnmount, onMounted, ref } from 'vue'

const isMobile = ref(typeof window !== 'undefined' && window.matchMedia('(max-width: 768px)').matches)

export function useIsMobile() {
  let mq: MediaQueryList | null = null
  const onChange = (e: MediaQueryListEvent) => { isMobile.value = e.matches }

  onMounted(() => {
    mq = window.matchMedia('(max-width: 768px)')
    isMobile.value = mq.matches
    mq.addEventListener('change', onChange)
  })
  onBeforeUnmount(() => mq?.removeEventListener('change', onChange))

  return { isMobile }
}
