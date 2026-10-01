/**
 * [INPUT]: 依赖 DOM 的 tabIndex、disabled、隐藏状态与计算样式
 * [OUTPUT]: 对外提供按 DOM 顺序排列的可见键盘焦点控件
 * [POS]: web/src/utils 的焦点筛选工具，供 Dialog 与 Portal 下拉框共享
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */
export function getFocusableElements(root: ParentNode): HTMLElement[] {
  return Array.from(root.querySelectorAll<HTMLElement>('button, [href], input, select, textarea, [tabindex]'))
    .filter((element) => {
      if (element.tabIndex < 0 || element.matches(':disabled') || element.closest('[hidden], [inert], [aria-hidden="true"]')) return false
      const style = getComputedStyle(element)
      return style.display !== 'none' && style.visibility !== 'hidden'
    })
}
