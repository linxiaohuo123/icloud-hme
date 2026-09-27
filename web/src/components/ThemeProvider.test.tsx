import { render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ThemeProvider, useTheme } from './ThemeProvider'

function ThemeProbe() {
  const { theme } = useTheme()
  return <span>{theme}</span>
}

describe('ThemeProvider', () => {
  afterEach(() => vi.restoreAllMocks())

  it('存储读取被禁用时仍能渲染', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new DOMException('Storage blocked', 'SecurityError')
    })

    render(<ThemeProvider><ThemeProbe /></ThemeProvider>)
    expect(screen.getByText('light')).toBeInTheDocument()
  })
})
