/**
 * [INPUT]: 接收字符串 value
 * [OUTPUT]: 导出 copyText: (value: string) => Promise<boolean>
 * [POS]: web/src/utils 的剪贴板安全写入工具，优先 Clipboard API，自动回退 textarea
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */
/**
 * 复制文本到剪贴板。
 * 某些浏览器扩展会拦截 Clipboard API，因此保留传统 API 作为降级路径。
 */
export async function copyText(value: string): Promise<boolean> {
  if (!value) return false

  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(value)
      return true
    }
  } catch {
    // 继续尝试传统复制，避免权限拒绝直接暴露给用户。
  }

  return copyWithLegacyApi(value)
}

function copyWithLegacyApi(value: string): boolean {
  const textarea = document.createElement('textarea')
  const previousFocus = document.activeElement as HTMLElement | null
  textarea.value = value
  textarea.setAttribute('readonly', '')
  textarea.setAttribute('aria-hidden', 'true')
  textarea.style.position = 'fixed'
  textarea.style.top = '-9999px'
  textarea.style.left = '-9999px'
  textarea.style.opacity = '0'
  document.body.appendChild(textarea)

  try {
    textarea.focus()
    textarea.select()
    textarea.setSelectionRange(0, value.length)
    if (typeof document.execCommand !== 'function') return false
    return document.execCommand('copy')
  } catch {
    return false
  } finally {
    textarea.remove()
    previousFocus?.focus()
  }
}
