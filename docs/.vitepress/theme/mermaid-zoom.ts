/**
 * Zero-dependency interactive Pan / Zoom / Fullscreen controller for VitePress Mermaid diagrams.
 *
 * Design invariants:
 * - Never moves `<div class="mermaid">` out of Vue's DOM hierarchy (safe across HMR and route changes).
 * - Survives `vitepress-plugin-mermaid` dark/light theme re-renders (`v-html` replacements) by
 *   observing each `.mermaid` container and re-applying transforms & floating controls cleanly.
 * - Inline scroll wheel never hijacks normal vertical scrolling (requires Ctrl/⌘ + wheel inline;
 *   direct wheel zoom is enabled in Fullscreen mode).
 */

interface ViewportState {
  scale: number
  tx: number
  ty: number
  fullscreen: boolean
}

const MIN_SCALE = 0.35
const MAX_SCALE = 5
const ZOOM_STEP = 1.25

function clamp(val: number, min: number, max: number): number {
  return Math.min(max, Math.max(min, val))
}

function isZhLocale(): boolean {
  const lang = (document.documentElement.lang || '').toLowerCase()
  return lang.startsWith('zh')
}

function getLabels() {
  if (isZhLocale()) {
    return {
      zoomOut: '缩小',
      zoomIn: '放大',
      reset: '重置视图 (100%)',
      fullscreen: '全屏查看',
      exitFullscreen: '退出全屏 (Esc)',
      hintInline: '拖拽平移 · Ctrl/⌘ + 滚轮缩放 · 双击放大/重置',
      hintFullscreen: '拖拽平移 · 滚轮缩放 · Esc 退出全屏',
    }
  }
  return {
    zoomOut: 'Zoom out',
    zoomIn: 'Zoom in',
    reset: 'Reset view (100%)',
    fullscreen: 'Fullscreen',
    exitFullscreen: 'Exit fullscreen (Esc)',
    hintInline: 'Drag to pan · Ctrl/⌘ + scroll to zoom · Double-click to zoom/reset',
    hintFullscreen: 'Drag to pan · Scroll to zoom · Esc to exit fullscreen',
  }
}

function enhanceMermaidBox(box: HTMLElement): () => void {
  const state: ViewportState = { scale: 1, tx: 0, ty: 0, fullscreen: false }
  const activePointers = new Map<number, { x: number; y: number }>()
  let dragPointerId: number | null = null
  let dragStartX = 0
  let dragStartY = 0
  let dragOrigTx = 0
  let dragOrigTy = 0
  let pinchStartDist = 0
  let pinchStartScale = 1
  let pinchMidX = 0
  let pinchMidY = 0
  let pinchOrigTx = 0
  let pinchOrigTy = 0
  let mutatingOwnChrome = false

  box.classList.add('vp-mermaid-interactive')

  const getSvg = (): SVGSVGElement | null => box.querySelector(':scope > svg')

  let pctBtn: HTMLButtonElement | null = null
  let fsBtn: HTMLButtonElement | null = null
  let hintEl: HTMLDivElement | null = null

  const applyTransform = () => {
    const svg = getSvg()
    if (svg) {
      svg.style.transformOrigin = '0 0'
      svg.style.transform = `translate(${state.tx}px, ${state.ty}px) scale(${state.scale})`
    }
    if (pctBtn) {
      pctBtn.textContent = `${Math.round(state.scale * 100)}%`
    }
    if (fsBtn) {
      const labels = getLabels()
      fsBtn.textContent = state.fullscreen ? '✕' : '⛶'
      fsBtn.title = state.fullscreen ? labels.exitFullscreen : labels.fullscreen
      fsBtn.setAttribute('aria-label', fsBtn.title)
    }
    if (hintEl) {
      const labels = getLabels()
      hintEl.textContent = state.fullscreen ? labels.hintFullscreen : labels.hintInline
    }
  }

  const resetView = () => {
    state.scale = 1
    state.tx = 0
    state.ty = 0
    applyTransform()
  }

  const zoomAt = (nextScale: number, clientX: number, clientY: number) => {
    const clamped = clamp(nextScale, MIN_SCALE, MAX_SCALE)
    if (Math.abs(clamped - state.scale) < 0.001) return
    const rect = box.getBoundingClientRect()
    const cx = clientX - rect.left
    const cy = clientY - rect.top
    const ratio = clamped / state.scale
    state.tx = cx - (cx - state.tx) * ratio
    state.ty = cy - (cy - state.ty) * ratio
    state.scale = clamped
    applyTransform()
  }

  const zoomByCenter = (factor: number) => {
    const rect = box.getBoundingClientRect()
    zoomAt(state.scale * factor, rect.left + rect.width / 2, rect.top + rect.height / 2)
  }

  const setFullscreen = (enable: boolean) => {
    if (state.fullscreen === enable) return
    state.fullscreen = enable
    box.classList.toggle('vp-mermaid-fullscreen', enable)
    document.body.classList.toggle('vp-mermaid-scroll-lock', enable)
    // Reset pan/zoom cleanly when entering/leaving fullscreen so diagram is always centered in view.
    state.scale = 1
    state.tx = 0
    state.ty = 0
    applyTransform()
  }

  const ensureChrome = () => {
    const svg = getSvg()
    if (!svg) return
    mutatingOwnChrome = true
    try {
      svg.style.maxWidth = '100%'
      svg.style.height = 'auto'
      svg.style.display = 'block'

      if (!box.querySelector(':scope > .vp-mermaid-toolbar')) {
        const labels = getLabels()
        const toolbar = document.createElement('div')
        toolbar.className = 'vp-mermaid-toolbar'
        toolbar.setAttribute('role', 'toolbar')

        const makeBtn = (text: string, title: string, onClick: () => void): HTMLButtonElement => {
          const btn = document.createElement('button')
          btn.type = 'button'
          btn.className = 'vp-mermaid-btn'
          btn.textContent = text
          btn.title = title
          btn.setAttribute('aria-label', title)
          btn.addEventListener('pointerdown', (e) => e.stopPropagation())
          btn.addEventListener('click', (e) => {
            e.stopPropagation()
            onClick()
          })
          return btn
        }

        const outBtn = makeBtn('−', labels.zoomOut, () => zoomByCenter(1 / ZOOM_STEP))
        pctBtn = makeBtn('100%', labels.reset, () => resetView())
        pctBtn.classList.add('vp-mermaid-btn-pct')
        const inBtn = makeBtn('+', labels.zoomIn, () => zoomByCenter(ZOOM_STEP))
        fsBtn = makeBtn('⛶', labels.fullscreen, () => setFullscreen(!state.fullscreen))

        toolbar.append(outBtn, pctBtn, inBtn, fsBtn)
        box.appendChild(toolbar)
      } else {
        pctBtn = box.querySelector(':scope > .vp-mermaid-toolbar .vp-mermaid-btn-pct')
        const btns = box.querySelectorAll(':scope > .vp-mermaid-toolbar button')
        fsBtn = (btns[btns.length - 1] as HTMLButtonElement | undefined) ?? null
      }

      if (!box.querySelector(':scope > .vp-mermaid-hint')) {
        hintEl = document.createElement('div')
        hintEl.className = 'vp-mermaid-hint'
        box.appendChild(hintEl)
      } else {
        hintEl = box.querySelector(':scope > .vp-mermaid-hint')
      }

      applyTransform()
    } finally {
      mutatingOwnChrome = false
    }
  }

  const onPointerDown = (e: PointerEvent) => {
    if (e.button !== 0 && e.pointerType === 'mouse') return
    if ((e.target as HTMLElement | null)?.closest('.vp-mermaid-toolbar')) return
    activePointers.set(e.pointerId, { x: e.clientX, y: e.clientY })
    try {
      box.setPointerCapture(e.pointerId)
    } catch {
      // ignore if capture unavailable
    }

    if (activePointers.size === 1) {
      dragPointerId = e.pointerId
      dragStartX = e.clientX
      dragStartY = e.clientY
      dragOrigTx = state.tx
      dragOrigTy = state.ty
      box.classList.add('is-dragging')
    } else if (activePointers.size === 2) {
      dragPointerId = null
      const pts = Array.from(activePointers.values())
      pinchStartDist = Math.hypot(pts[1].x - pts[0].x, pts[1].y - pts[0].y) || 1
      pinchStartScale = state.scale
      const rect = box.getBoundingClientRect()
      pinchMidX = (pts[0].x + pts[1].x) / 2 - rect.left
      pinchMidY = (pts[0].y + pts[1].y) / 2 - rect.top
      pinchOrigTx = state.tx
      pinchOrigTy = state.ty
    }
  }

  const onPointerMove = (e: PointerEvent) => {
    if (!activePointers.has(e.pointerId)) return
    activePointers.set(e.pointerId, { x: e.clientX, y: e.clientY })

    if (activePointers.size === 2) {
      const pts = Array.from(activePointers.values())
      const dist = Math.hypot(pts[1].x - pts[0].x, pts[1].y - pts[0].y)
      if (dist > 0 && pinchStartDist > 0) {
        const nextScale = clamp(pinchStartScale * (dist / pinchStartDist), MIN_SCALE, MAX_SCALE)
        const ratio = nextScale / pinchStartScale
        state.scale = nextScale
        state.tx = pinchMidX - (pinchMidX - pinchOrigTx) * ratio
        state.ty = pinchMidY - (pinchMidY - pinchOrigTy) * ratio
        applyTransform()
      }
      return
    }

    if (dragPointerId === e.pointerId) {
      state.tx = dragOrigTx + (e.clientX - dragStartX)
      state.ty = dragOrigTy + (e.clientY - dragStartY)
      applyTransform()
    }
  }

  const onPointerUpOrCancel = (e: PointerEvent) => {
    activePointers.delete(e.pointerId)
    if (dragPointerId === e.pointerId) {
      dragPointerId = null
    }
    if (activePointers.size === 0) {
      box.classList.remove('is-dragging')
    }
    try {
      if (box.hasPointerCapture(e.pointerId)) {
        box.releasePointerCapture(e.pointerId)
      }
    } catch {
      // ignore
    }
  }

  const onWheel = (e: WheelEvent) => {
    // Inline mode requires Ctrl/⌘ + wheel so vertical page scroll is never trapped.
    // Fullscreen mode allows direct scroll-wheel zoom.
    if (!state.fullscreen && !e.ctrlKey && !e.metaKey) return
    e.preventDefault()
    const factor = e.deltaY < 0 ? 1.14 : 1 / 1.14
    zoomAt(state.scale * factor, e.clientX, e.clientY)
  }

  const onDblClick = (e: MouseEvent) => {
    if ((e.target as HTMLElement | null)?.closest('.vp-mermaid-toolbar')) return
    if (Math.abs(state.scale - 1) > 0.05 || Math.abs(state.tx) > 4 || Math.abs(state.ty) > 4) {
      resetView()
    } else {
      zoomAt(1.6, e.clientX, e.clientY)
    }
  }

  const onKeyDown = (e: KeyboardEvent) => {
    if (e.key === 'Escape' && state.fullscreen) {
      setFullscreen(false)
    }
  }

  box.addEventListener('pointerdown', onPointerDown)
  box.addEventListener('pointermove', onPointerMove)
  box.addEventListener('pointerup', onPointerUpOrCancel)
  box.addEventListener('pointercancel', onPointerUpOrCancel)
  box.addEventListener('wheel', onWheel, { passive: false })
  box.addEventListener('dblclick', onDblClick)
  window.addEventListener('keydown', onKeyDown)

  // Observe Mermaid.vue's async SVG render & light/dark theme re-renders.
  const boxObserver = new MutationObserver(() => {
    if (mutatingOwnChrome) return
    ensureChrome()
  })
  boxObserver.observe(box, { childList: true })

  ensureChrome()

  return () => {
    if (state.fullscreen) {
      setFullscreen(false)
    }
    boxObserver.disconnect()
    box.removeEventListener('pointerdown', onPointerDown)
    box.removeEventListener('pointermove', onPointerMove)
    box.removeEventListener('pointerup', onPointerUpOrCancel)
    box.removeEventListener('pointercancel', onPointerUpOrCancel)
    box.removeEventListener('wheel', onWheel)
    box.removeEventListener('dblclick', onDblClick)
    window.removeEventListener('keydown', onKeyDown)
  }
}

export function initMermaidPanZoom(): () => void {
  const cleanups = new Map<HTMLElement, () => void>()

  const scan = () => {
    const boxes = document.querySelectorAll<HTMLElement>('.vp-doc .mermaid')
    for (const box of boxes) {
      if (!cleanups.has(box)) {
        cleanups.set(box, enhanceMermaidBox(box))
      }
    }
    for (const [el, dispose] of cleanups) {
      if (!el.isConnected) {
        dispose()
        cleanups.delete(el)
      }
    }
  }

  // Watch .vp-doc for async <Suspense><Mermaid /></Suspense> mounts.
  const root = document.querySelector('.vp-doc') ?? document.body
  const docObserver = new MutationObserver(() => scan())
  docObserver.observe(root, { childList: true, subtree: true })

  scan()

  return () => {
    docObserver.disconnect()
    for (const dispose of cleanups.values()) {
      dispose()
    }
    cleanups.clear()
    document.body.classList.remove('vp-mermaid-scroll-lock')
  }
}
