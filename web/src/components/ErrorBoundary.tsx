/**
 * [INPUT]: 依赖 react 的 Component, ReactNode, ErrorInfo
 * [OUTPUT]: 对外提供 ErrorBoundary 错误边界组件，拦截子组件未捕获异常
 * [POS]: web/src/components 的稳定性屏障，彻底终结白屏问题
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { Component, type ErrorInfo, type ReactNode } from 'react'

interface Props {
  children: ReactNode
  fallback?: ReactNode
}

interface State {
  hasError: boolean
  error: Error | null
}

export default class ErrorBoundary extends Component<Props, State> {
  constructor(props: Props) {
    super(props)
    this.state = { hasError: false, error: null }
  }

  static getDerivedStateFromError(error: Error): State {
    return { hasError: true, error }
  }

  componentDidCatch(error: Error, errorInfo: ErrorInfo) {
    console.error('ErrorBoundary 捕获异常:', error, errorInfo)
  }

  render() {
    if (this.state.hasError) {
      if (this.props.fallback) {
        return this.props.fallback
      }
      return (
        <div className="page-container" style={{ padding: '40px 20px' }}>
          <div className="card" style={{ borderColor: 'var(--color-danger)', maxWidth: '600px', margin: '0 auto', padding: '24px' }}>
            <h2 style={{ color: 'var(--color-danger)', margin: '0 0 12px' }}>页面渲染异常</h2>
            <p className="text-secondary" style={{ marginBottom: '16px' }}>
              系统捕获到底层渲染错误，已防止整页白屏：
            </p>
            <pre style={{ background: '#0f172a', color: '#f87171', padding: '12px', borderRadius: '6px', fontSize: '12px', overflowX: 'auto' }}>
              {this.state.error?.message || '未知错误'}
            </pre>
            <div style={{ marginTop: '16px', display: 'flex', gap: '10px' }}>
              <button
                type="button"
                className="btn btn-primary"
                onClick={() => {
                  this.setState({ hasError: false, error: null })
                  window.location.reload()
                }}
              >
                刷新页面
              </button>
              <button
                type="button"
                className="btn btn-secondary"
                onClick={() => {
                  this.setState({ hasError: false, error: null })
                  window.location.href = '/schedule'
                }}
              >
                返回首页
              </button>
            </div>
          </div>
        </div>
      )
    }

    return this.props.children
  }
}
