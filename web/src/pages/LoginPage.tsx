/**
 * [INPUT]: 依赖 react-router-dom 的 useNavigate, auth/AuthProvider 的 useAuth, api/client 的 ApiError, components/icons 的 IconLock/IconShield/IconEye/IconEyeOff
 * [OUTPUT]: 对外提供 LoginPage 管理员登录页面组件
 * [POS]: web/src/pages 的公开受限路由入口，负责单密码鉴权表单提交
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import { useAuth } from '../auth/AuthProvider'
import { ApiError } from '../api/client'
import { IconEye, IconEyeOff, IconLock, IconShield } from '../components/icons'

export default function LoginPage() {
  const { login } = useAuth()
  const navigate = useNavigate()
  const [password, setPassword] = useState('')
  const [showPassword, setShowPassword] = useState(false)
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  async function handleSubmit(e: FormEvent) {
    e.preventDefault()
    if (submitting || !password) return
    setSubmitting(true)
    setError('')
    try {
      await login(password)
      navigate('/accounts', { replace: true })
    } catch (err) {
      setError(
        err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态',
      )
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="login-page">
      <div className="login-brand">
        <span className="logo" aria-hidden="true">
          <IconShield size={30} />
        </span>
        <h1>iCloud HME 管理台</h1>
        <p>管理你的 iCloud 隐藏邮箱别名与邮件</p>
      </div>
      <form onSubmit={handleSubmit} className="card login-form">
        {error && (
          <div className="alert-error" role="alert">
            {error}
          </div>
        )}
        <div className="form-field">
          <label htmlFor="admin-password">管理员密码</label>
          <div style={{ position: 'relative', width: '100%' }}>
            <input
              id="admin-password"
              type={showPassword ? 'text' : 'password'}
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
              placeholder="请输入管理员密码"
              style={{ paddingRight: 40 }}
            />
            <button
              type="button"
              className="login-password-toggle"
              onClick={() => setShowPassword((prev) => !prev)}
              aria-label={showPassword ? '隐藏密码' : '显示密码'}
              tabIndex={-1}
            >
              {showPassword ? <IconEyeOff size={18} /> : <IconEye size={18} />}
            </button>
          </div>
        </div>
        <div className="form-actions">
          <button type="submit" className="primary" disabled={submitting}>
            {submitting ? '登录中…' : '登录'}
          </button>
        </div>
        <div className="login-security-tag" aria-hidden="true">
          <IconLock size={13} />
          <span>本地内存加密会话 · 杜绝泄露</span>
        </div>
      </form>
    </div>
  )
}
