/**
 * [INPUT]: 依赖 react, react-dom/client, ./styles.css, ./App
 * [OUTPUT]: 挂载根节点到 DOM
 * [POS]: web/src 的前端应用入口，StrictMode 渲染根组件
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './styles.css'
import App from './App.tsx'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
