import DefaultTheme from 'vitepress/theme'
import type { Theme } from 'vitepress'
import { inBrowser, onContentUpdated } from 'vitepress'
import { onUnmounted } from 'vue'
import 'katex/dist/katex.min.css'
import './custom.css'
import { initMermaidPanZoom } from './mermaid-zoom'

export default {
  extends: DefaultTheme,
  setup() {
    if (!inBrowser) return
    let cleanup: (() => void) | null = null

    const rebind = () => {
      cleanup?.()
      cleanup = initMermaidPanZoom()
    }

    onContentUpdated(() => {
      rebind()
    })

    onUnmounted(() => {
      cleanup?.()
      cleanup = null
    })
  },
} satisfies Theme
