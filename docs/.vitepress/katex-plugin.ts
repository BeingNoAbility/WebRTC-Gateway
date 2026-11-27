import katex from 'katex'

// Lightweight build-time Markdown-It KaTeX plugin (zero client JS, zero external CDN).
// Supports:
//   - Single-line & multi-line display blocks: $$...$$
//   - Inline math: $...$ (Pandoc boundary rules: no space right after opening $,
//     no space right before closing $, and closing $ cannot be followed by a digit)

function isValidInlineDelim(src: string, pos: number): { canOpen: boolean; canClose: boolean } {
  const max = src.length
  const prevChar = pos > 0 ? src.charCodeAt(pos - 1) : -1
  const nextChar = pos + 1 < max ? src.charCodeAt(pos + 1) : -1

  let canOpen = true
  let canClose = true

  // Opening `$` cannot be followed by whitespace, closing `$` cannot be preceded by whitespace,
  // and closing `$` cannot be followed by a digit (avoids currency like "$5 and $10").
  if (prevChar === 0x20 || prevChar === 0x09 || (nextChar >= 0x30 && nextChar <= 0x39)) {
    canClose = false
  }
  if (nextChar === 0x20 || nextChar === 0x09) {
    canOpen = false
  }
  return { canOpen, canClose }
}

function renderKatex(tex: string, displayMode: boolean): string {
  const html = katex.renderToString(tex.trim(), {
    displayMode,
    throwOnError: false,
    strict: 'ignore',
    output: 'htmlAndMathml',
  })
  if (displayMode) {
    return `<div class="vp-katex-block" v-pre>${html}</div>\n`
  }
  return `<span class="vp-katex-inline" v-pre>${html}</span>`
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
export function katexPlugin(md: any): void {
  // Block rule: $$ ... $$ (single-line or multi-line)
  md.block.ruler.after(
    'blockquote',
    'math_block',
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    (state: any, startLine: number, endLine: number, silent: boolean): boolean => {
      const startPos = state.bMarks[startLine] + state.tShift[startLine]
      const maxPos = state.eMarks[startLine]
      if (startPos + 2 > maxPos) return false
      if (state.src.slice(startPos, startPos + 2) !== '$$') return false

      let firstLine = state.src.slice(startPos + 2, maxPos)
      let nextLine = startLine
      let content = ''

      // Single-line form: $$ ... $$
      if (firstLine.trim().endsWith('$$') && firstLine.trim().length > 2) {
        const trimmed = firstLine.trim()
        content = trimmed.slice(0, -2)
      } else {
        let found = false
        let lastLine = ''
        while (nextLine + 1 < endLine) {
          nextLine++
          const lineStart = state.bMarks[nextLine] + state.tShift[nextLine]
          const lineMax = state.eMarks[nextLine]
          const lineText = state.src.slice(lineStart, lineMax)
          if (lineText.trim().endsWith('$$')) {
            const closeIdx = lineText.lastIndexOf('$$')
            lastLine = lineText.slice(0, closeIdx)
            found = true
            break
          }
        }
        if (!found) return false
        const middle =
          nextLine > startLine + 1
            ? state.getLines(startLine + 1, nextLine, state.tShift[startLine], true)
            : ''
        content = [firstLine, middle, lastLine].filter((s) => s.length > 0).join('\n')
      }

      if (silent) return true

      state.line = nextLine + 1
      const token = state.push('math_block', 'math', 0)
      token.block = true
      token.content = content
      token.map = [startLine, state.line]
      token.markup = '$$'
      return true
    },
    { alt: ['paragraph', 'reference', 'blockquote', 'list'] }
  )

  // Inline rule: $...$
  md.inline.ruler.after(
    'escape',
    'math_inline',
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    (state: any, silent: boolean): boolean => {
      const src: string = state.src
      const pos: number = state.pos
      if (src.charCodeAt(pos) !== 0x24 /* $ */) return false
      if (pos + 1 < src.length && src.charCodeAt(pos + 1) === 0x24) return false

      const openDelim = isValidInlineDelim(src, pos)
      if (!openDelim.canOpen) {
        if (!silent) state.pending += '$'
        state.pos += 1
        return true
      }

      let match = pos + 1
      while ((match = src.indexOf('$', match)) !== -1) {
        // Count preceding backslashes to handle escaped \$
        let esc = match - 1
        while (esc >= pos + 1 && src.charCodeAt(esc) === 0x5c /* \ */) esc--
        if ((match - 1 - esc) % 2 === 1) {
          match += 1
          continue
        }
        break
      }

      if (match === -1 || match === pos + 1) {
        if (!silent) state.pending += '$'
        state.pos += 1
        return true
      }

      const closeDelim = isValidInlineDelim(src, match)
      if (!closeDelim.canClose) {
        if (!silent) state.pending += '$'
        state.pos += 1
        return true
      }

      if (!silent) {
        const token = state.push('math_inline', 'math', 0)
        token.markup = '$'
        token.content = src.slice(pos + 1, match)
      }
      state.pos = match + 1
      return true
    }
  )

  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  md.renderer.rules.math_block = (tokens: any[], idx: number) =>
    renderKatex(tokens[idx].content, true)

  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  md.renderer.rules.math_inline = (tokens: any[], idx: number) =>
    renderKatex(tokens[idx].content, false)
}

/**
 * Inserts zero-width HTML5 `<wbr>` break opportunities after `_` and `/` inside table-cell
 * inline code spans (`<td><code>...</code></td>`) so long UPPER_SNAKE_CASE env vars and paths
 * wrap cleanly at token boundaries on narrower screens without breaking copy-paste text.
 */
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export function tableCodeWrapPlugin(md: any): void {
  let tdDepth = 0
  const defaultTdOpen =
    md.renderer.rules.td_open ??
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    ((tokens: any[], idx: number, options: any, _env: any, self: any) =>
      self.renderToken(tokens, idx, options))
  const defaultTdClose =
    md.renderer.rules.td_close ??
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    ((tokens: any[], idx: number, options: any, _env: any, self: any) =>
      self.renderToken(tokens, idx, options))
  const defaultCodeInline =
    md.renderer.rules.code_inline ??
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    ((tokens: any[], idx: number, options: any, _env: any, self: any) =>
      self.renderToken(tokens, idx, options))

  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  md.renderer.rules.td_open = (...args: any[]) => {
    tdDepth++
    return defaultTdOpen(...args)
  }
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  md.renderer.rules.td_close = (...args: any[]) => {
    tdDepth = Math.max(0, tdDepth - 1)
    return defaultTdClose(...args)
  }
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  md.renderer.rules.code_inline = (tokens: any[], idx: number, options: any, env: any, self: any) => {
    const rendered: string = defaultCodeInline(tokens, idx, options, env, self)
    if (tdDepth <= 0) return rendered
    // Only inject <wbr> inside the inner text of <code>...</code>, never inside tag attributes.
    const openEnd = rendered.indexOf('>')
    const closeStart = rendered.lastIndexOf('</code>')
    if (openEnd < 0 || closeStart <= openEnd) return rendered
    const inner = rendered.slice(openEnd + 1, closeStart)
    if (inner.length < 10) return rendered
    const wrapped = inner.replace(/([_/])/g, '$1<wbr>')
    return `${rendered.slice(0, openEnd + 1)}${wrapped}${rendered.slice(closeStart)}`
  }
}
