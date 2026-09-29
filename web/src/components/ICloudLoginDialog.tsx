/**
 * [INPUT]: 依赖 components/Dialog, api/client 的 request/ApiError
 * [OUTPUT]: 对外提供 ICloudLoginDialog 苹果账号官方密码与 OTP 两阶段认证弹窗
 * [POS]: web/src/components 的凭据认证层，供账号列表与快捷操作消费
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useState } from 'react'
import Dialog from './Dialog'
import { request, ApiError } from '../api/client'

interface ICloudLoginDialogProps {
  accountId: string
  accountEmail?: string
  accountName?: string
  open: boolean
  onClose: () => void
  onSaved: () => void
}

/** iCloud 密码登录对话框:支持 OTP 两阶段 */
export default function ICloudLoginDialog({
  accountId,
  accountEmail,
  accountName,
  open,
  onClose,
  onSaved,
}: ICloudLoginDialogProps) {
  const [password, setPassword] = useState('')
  const [otp, setOtp] = useState('')
  const [otpRequired, setOtpRequired] = useState(false)
  const [camoufoxTaskId, setCamoufoxTaskId] = useState<string | null>(null)
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [cancelling, setCancelling] = useState(false)

  async function handleSubmit() {
    if (submitting) return
    setSubmitting(true)
    setError('')
    try {
      await request(`/api/accounts/${accountId}/login`, {
        method: 'POST',
        body: JSON.stringify(otpRequired ? { otp_code: otp } : { password }),
      })
      setPassword('')
      setOtp('')
      setOtpRequired(false)
      setCamoufoxTaskId(null)
      onSaved()
    } catch (err) {
      if (err instanceof ApiError && err.code === 'OTP_REQUIRED') {
        const data = err.data
        setCamoufoxTaskId(
          data && typeof data === 'object' && 'task_id' in data && typeof data.task_id === 'string'
            ? data.task_id
            : null,
        )
        setOtpRequired(true)
      } else {
        setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
      }
    } finally {
      setSubmitting(false)
    }
  }

  async function handleClose() {
    if (submitting || cancelling) return
    if (camoufoxTaskId) {
      setCancelling(true)
      try {
        await request(`/api/accounts/${accountId}/login/cancel`, {
          method: 'POST',
          body: { task_id: camoufoxTaskId },
        })
      } catch (err) {
        setError(err instanceof ApiError ? err.message : '取消登录失败，请重试')
        setCancelling(false)
        return
      }
      setCancelling(false)
    }
    setPassword('')
    setOtp('')
    setOtpRequired(false)
    setCamoufoxTaskId(null)
    setError('')
    onClose()
  }

  return (
    <Dialog
      title="Apple ID 账号授权登录"
      open={open}
      onClose={() => void handleClose()}
    >
      {accountEmail && (
        <div className="alert-info" style={{ marginBottom: 12 }}>
          正在为账号 <strong>{accountName ? `${accountName} (${accountEmail})` : accountEmail}</strong> 进行 Apple 官方认证
        </div>
      )}
      {error && (
        <div className="alert-error" role="alert">
          {error}
        </div>
      )}
      {otpRequired && (
        <div className="alert-info">该 Apple ID 已启用双重认证，请输入发往受信任设备的 6 位验证码。</div>
      )}
      {!otpRequired && (
        <div className="form-field">
          <label htmlFor="icloud-login-password">Apple ID 密码</label>
          <input
            id="icloud-login-password"
            type="password"
            autoComplete="current-password"
            placeholder="请输入该 Apple ID 的官方密码"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            onKeyDown={(e) => { if (e.key === 'Enter') void handleSubmit() }}
          />
          <span className="field-hint" style={{ fontSize: 12, color: 'var(--color-text-secondary, #64748b)', marginTop: 4, lineHeight: 1.5, display: 'block' }}>
            系统将向 Apple 申请登录凭据。如登录受阻，可通过【更新 Cookie】使用浏览器 Cookie 激活。
          </span>
        </div>
      )}
      {otpRequired && (
        <div className="form-field">
          <label htmlFor="icloud-login-otp">验证码</label>
          <input
            id="icloud-login-otp"
            type="text"
            inputMode="numeric"
            maxLength={6}
            value={otp}
            onChange={(e) => setOtp(e.target.value.replace(/\D/g, ''))}
            onKeyDown={(e) => { if (e.key === 'Enter') void handleSubmit() }}
            autoComplete="one-time-code"
          />
        </div>
      )}
      {(submitting || cancelling) && (
        <div className="alert-info" style={{ marginTop: 12 }}>
          {cancelling ? '正在取消登录…' : '正在完成 Apple 账号验证，请稍候…'}
        </div>
      )}
      <div className="form-actions">
        <button type="button" onClick={() => void handleClose()} disabled={submitting || cancelling}>取消</button>
        <button type="button" className="primary" onClick={() => void handleSubmit()} disabled={submitting || cancelling}>
          {cancelling ? '取消中…' : submitting ? '登录中…' : otpRequired ? '验证' : '登录'}
        </button>
      </div>
    </Dialog>
  )
}
