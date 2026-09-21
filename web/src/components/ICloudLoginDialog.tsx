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
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  async function handleSubmit() {
    if (submitting) return
    setSubmitting(true)
    setError('')
    try {
      await request(`/api/accounts/${accountId}/login`, {
        method: 'POST',
        body: JSON.stringify({
          password,
          ...(otpRequired ? { otp_code: otp } : {}),
        }),
      })
      setPassword('')
      setOtp('')
      setOtpRequired(false)
      onSaved()
    } catch (err) {
      if (err instanceof ApiError && err.code === 'OTP_REQUIRED') {
        setOtpRequired(true)
      } else {
        setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog
      title="Apple ID 账号授权登录"
      open={open}
      onClose={() => {
        setPassword('')
        setOtp('')
        setOtpRequired(false)
        setError('')
        onClose()
      }}
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
          />
          <span className="field-hint" style={{ fontSize: 12, color: 'var(--color-text-secondary, #64748b)', marginTop: 4 }}>
            说明：系统将通过 SRP 安全握手向 Apple 服务器申请会话凭据。
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
            autoComplete="one-time-code"
          />
        </div>
      )}
      <div className="form-actions">
        <button onClick={onClose}>取消</button>
        <button className="primary" onClick={() => void handleSubmit()} disabled={submitting}>
          {submitting ? '登录中…' : otpRequired ? '验证' : '登录'}
        </button>
      </div>
    </Dialog>
  )
}
