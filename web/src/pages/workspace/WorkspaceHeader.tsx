/**
 * [INPUT]: 依赖 api/types 的 AccountSummary/Alias, hooks/useAccounts, react-router-dom 的 useNavigate, components/icons 的各类图标
 * [OUTPUT]: 对外提供 WorkspaceHeader 工作台头部卡片组件，切号器管理焦点与键盘导航，业务弹窗优先处理快捷键，复制状态遵循实际结果
 * [POS]: web/src/pages/workspace 的核心头部视觉组件，呈现极简 Apple/Linear 工业级美学，首屏高屏效与高层次感
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import type { AccountSummary, Alias } from '../../api/types'
import { useAccounts } from '../../hooks/useAccounts'
import {
  IconAccounts,
  IconAliases,
  IconCheck,
  IconChevronDown,
  IconClose,
  IconCloud,
  IconCookie,
  IconCopy,
  IconMail,
  IconPlus,
  IconRefresh,
  IconSearch,
  IconShield,
  IconZap,
} from '../../components/icons'

interface WorkspaceHeaderProps {
  account: AccountSummary | null
  aliases?: Alias[]
  activeAliasCount?: number
  quickCreating: boolean
  aliasLoading: boolean
  onQuickCreate: () => void
  onCreateCustom: () => void
  onSync: () => void
  onOpenLogin?: () => void
  onOpenCookie: () => void
  onOpenMailbox: () => void
  onCopyAccount: (email: string) => Promise<boolean>
}

export default function WorkspaceHeader({
  account,
  aliases = [],
  activeAliasCount = 0,
  quickCreating,
  aliasLoading,
  onQuickCreate,
  onCreateCustom,
  onSync,
  onOpenLogin,
  onOpenCookie,
  onOpenMailbox,
  onCopyAccount,
}: WorkspaceHeaderProps) {
  const navigate = useNavigate()
  const { accounts } = useAccounts()
  const [switcherOpen, setSwitcherOpen] = useState(false)
  const [searchQuery, setSearchQuery] = useState('')
  const [selectedIndex, setSelectedIndex] = useState(0)
  const [accountCopied, setAccountCopied] = useState(false)
  const copyTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const switcherRef = useRef<HTMLDivElement>(null)
  const triggerBtnRef = useRef<HTMLButtonElement>(null)
  const searchInputRef = useRef<HTMLInputElement>(null)
  const optionRefs = useRef<(HTMLButtonElement | null)[]>([])

  useEffect(() => {
    return () => {
      if (copyTimerRef.current) clearTimeout(copyTimerRef.current)
    }
  }, [])

  const emailToCopy = account?.icloud_email || account?.real_email || ''
  const MAX_ALIASES = 750
  const totalCount = aliases.length || account?.alias_total || 0
  const quotaPercent = Math.min(100, Math.round((totalCount / MAX_ALIASES) * 100))
  const inactiveCount = Math.max(0, totalCount - activeAliasCount)
  const isHealthy = account?.status === 'active' && account?.has_cookies

  // 切号候选列表：按异常/临界/健康度智能排序
  const filteredAccounts = useMemo(() => {
    const q = searchQuery.trim().toLowerCase()
    let list = accounts
    if (q) {
      list = accounts.filter(
        (a) =>
          (a.name && a.name.toLowerCase().includes(q)) ||
          (a.real_email && a.real_email.toLowerCase().includes(q)) ||
          (a.icloud_email && a.icloud_email.toLowerCase().includes(q)) ||
          (a.id && a.id.toLowerCase().includes(q)),
      )
    }
    return [...list].sort((a, b) => {
      const aBad = !a.has_cookies || a.status !== 'active'
      const bBad = !b.has_cookies || b.status !== 'active'
      if (aBad !== bBad) return aBad ? -1 : 1

      const aFull = (a.alias_total || 0) >= 700
      const bFull = (b.alias_total || 0) >= 700
      if (aFull !== bFull) return aFull ? -1 : 1

      return (a.alias_total || 0) - (b.alias_total || 0)
    })
  }, [accounts, searchQuery])

  // 打开切号器时初始化选中当前账号，并支持快捷键
  useEffect(() => {
    if (switcherOpen) {
      const currentIdx = filteredAccounts.findIndex((a) => a.id === account?.id)
      setSelectedIndex(currentIdx >= 0 ? currentIdx : 0)
    }
  }, [switcherOpen, account?.id, filteredAccounts])

  useEffect(() => {
    if (switcherOpen) searchInputRef.current?.focus()
  }, [switcherOpen])

  // 键盘高亮自动滚动到可视区域
  useEffect(() => {
    if (switcherOpen && optionRefs.current[selectedIndex]) {
      optionRefs.current[selectedIndex]?.scrollIntoView({ block: 'nearest' })
    }
  }, [selectedIndex, switcherOpen])

  // 全局/外部点击与快捷键监听 (Escape收起, Ctrl+K/Cmd+K唤起)
  useEffect(() => {
    const handleGlobalKeydown = (e: KeyboardEvent) => {
      if (e.defaultPrevented) return
      const modalOpen = Boolean(document.querySelector('[role="dialog"][aria-modal="true"]'))
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        if (modalOpen) return
        setSwitcherOpen((prev) => !prev)
        return
      }
      if (modalOpen) return
      if (switcherOpen && e.key === 'Escape') {
        e.preventDefault()
        setSwitcherOpen(false)
        triggerBtnRef.current?.focus()
      }
    }

    const handleClickOutside = (e: MouseEvent) => {
      if (switcherOpen && switcherRef.current && !switcherRef.current.contains(e.target as Node)) {
        setSwitcherOpen(false)
      }
    }

    document.addEventListener('keydown', handleGlobalKeydown)
    document.addEventListener('mousedown', handleClickOutside)
    return () => {
      document.removeEventListener('keydown', handleGlobalKeydown)
      document.removeEventListener('mousedown', handleClickOutside)
    }
  }, [switcherOpen])

  function handleSelectAccount(targetId: string) {
    setSwitcherOpen(false)
    setSearchQuery('')
    if (targetId !== account?.id) {
      navigate(`/workspace/${encodeURIComponent(targetId)}`)
    }
  }

  function handleSearchKeyDown(e: React.KeyboardEvent<HTMLInputElement>) {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      if (filteredAccounts.length > 0) {
        setSelectedIndex((prev) => (prev + 1) % filteredAccounts.length)
      }
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      if (filteredAccounts.length > 0) {
        setSelectedIndex((prev) => (prev - 1 + filteredAccounts.length) % filteredAccounts.length)
      }
    } else if (e.key === 'Enter') {
      e.preventDefault()
      if (filteredAccounts[selectedIndex]) {
        handleSelectAccount(filteredAccounts[selectedIndex].id)
      }
    } else if (e.key === 'Escape') {
      e.preventDefault()
      setSwitcherOpen(false)
      triggerBtnRef.current?.focus()
    }
  }

  async function handleCopyEmailClick() {
    if (!emailToCopy) return
    if (copyTimerRef.current) clearTimeout(copyTimerRef.current)
    const ok = await onCopyAccount(emailToCopy)
    setAccountCopied(ok)
    if (!ok) return
    copyTimerRef.current = setTimeout(() => setAccountCopied(false), 1600)
  }

  return (
    <div className="workspace-hero-card">
      {/* ── 顶部主行：身份识别、就地切号器与核心快捷动作 ── */}
      <div className="workspace-hero-main">
        <div className="workspace-hero-identity">
          <div className="workspace-hero-info">
            {/* 账号切号器 */}
            <div className="hero-account-switcher" ref={switcherRef}>
              <button
                ref={triggerBtnRef}
                type="button"
                className={`hero-switcher-trigger ${switcherOpen ? 'is-active' : ''}`}
                onClick={() => setSwitcherOpen((v) => !v)}
                title="点击快速切换或搜索其他账号 (快捷键 ⌘K 或 Ctrl+K)"
                aria-expanded={switcherOpen}
              >
                <h1 className="workspace-name">{account?.name || account?.real_email || '加载中…'}</h1>
                <IconChevronDown size={14} className={`hero-switcher-chevron ${switcherOpen ? 'is-open' : ''}`} />
              </button>

              {/* 悬浮切号面板 */}
              {switcherOpen && (
                <div className="hero-switcher-menu" role="menu">
                  <div className="hero-switcher-search">
                    <IconSearch size={13} className="hero-switcher-search-icon" />
                    <input
                      ref={searchInputRef}
                      type="text"
                      className="hero-switcher-input"
                      placeholder="搜索账号或邮箱..."
                      value={searchQuery}
                      onChange={(e) => {
                        setSearchQuery(e.target.value)
                        setSelectedIndex(0)
                      }}
                      onKeyDown={handleSearchKeyDown}
                    />
                    {searchQuery && (
                      <button
                        type="button"
                        className="hero-switcher-clear-btn"
                        onClick={() => {
                          setSearchQuery('')
                          setSelectedIndex(0)
                          searchInputRef.current?.focus()
                        }}
                        title="清空搜索"
                        aria-label="清空搜索"
                      >
                        <IconClose size={12} />
                      </button>
                    )}
                  </div>

                  <div className="hero-switcher-list">
                    {filteredAccounts.length === 0 ? (
                      <div className="hero-switcher-empty">未匹配到相关账号</div>
                    ) : (
                      filteredAccounts.map((acc, index) => {
                        const isCurrent = acc.id === account?.id
                        const isKeyboardActive = index === selectedIndex
                        const isUnhealthy = !acc.has_cookies || acc.status !== 'active'
                        const isNearLimit = (acc.alias_total || 0) >= 700

                        return (
                          <button
                            key={acc.id}
                            ref={(el) => {
                              optionRefs.current[index] = el
                            }}
                            type="button"
                            className={`hero-switcher-option ${isCurrent ? 'is-selected' : ''} ${
                              isKeyboardActive ? 'is-keyboard-active' : ''
                            }`}
                            onMouseEnter={() => setSelectedIndex(index)}
                            onClick={() => handleSelectAccount(acc.id)}
                          >
                            <span
                              className={`hero-switcher-dot ${
                                isUnhealthy
                                  ? 'is-danger'
                                  : isNearLimit
                                    ? 'is-warning'
                                    : 'is-success'
                              }`}
                            />
                            <div className="hero-switcher-option-text">
                              <span className="hero-switcher-option-name">
                                {acc.name || acc.real_email}
                              </span>
                              <span className="hero-switcher-option-email font-mono">
                                {acc.icloud_email || acc.real_email}
                              </span>
                            </div>
                            <div className="hero-switcher-option-meta">
                              <span
                                className="hero-switcher-count font-mono"
                                title={`活跃: ${acc.alias_active ?? 0} / 总计: ${acc.alias_total ?? 0}`}
                              >
                                {acc.alias_total ?? acc.alias_active ?? 0}
                              </span>
                              <span className="hero-switcher-check-slot">
                                {isCurrent && <IconCheck size={14} className="hero-switcher-check text-primary" />}
                              </span>
                            </div>
                          </button>
                        )
                      })
                    )}
                  </div>

                  <div className="hero-switcher-footer">
                    <button
                      type="button"
                      className="hero-switcher-footer-btn"
                      onClick={() => {
                        setSwitcherOpen(false)
                        navigate('/accounts')
                      }}
                    >
                      <IconAccounts size={13} />
                      <span>管理所有账号 ({accounts.length})</span>
                    </button>
                  </div>
                </div>
              )}
            </div>

            {/* 账号真实邮箱（极简纯粹内联无框交互，紧随账号名称聚合身份） */}
            {emailToCopy && (
              <button
                type="button"
                className={`hero-email-btn ${accountCopied ? 'is-copied' : ''}`}
                onClick={() => void handleCopyEmailClick()}
                title={accountCopied ? '已复制到剪贴板！' : `点击复制账号邮箱 (${emailToCopy})`}
                aria-label={`复制账号邮箱 ${emailToCopy}`}
              >
                <span className="hero-email-text">
                  {account?.name && account.name.trim().toLowerCase() !== emailToCopy.toLowerCase()
                    ? emailToCopy
                    : accountCopied
                      ? '已复制'
                      : '复制邮箱'}
                </span>
                <span className="hero-email-copy-icon">
                  {accountCopied ? (
                    <IconCheck size={12} className="text-success" />
                  ) : (
                    <IconCopy size={12} />
                  )}
                </span>
              </button>
            )}

            {/* 状态指示（作为身份后的健康状态补充，通透呼吸灯） */}
            {account && (
              <span
                className={`status-pill ${
                  isHealthy ? 'active' : account.status === 'error' ? 'error' : 'pending'
                }`}
                title={`当前账号状态: ${
                  isHealthy
                    ? '正常运行，凭据有效'
                    : account.status === 'error'
                      ? '凭据失效或异常'
                      : !account.has_cookies
                        ? '未配置 Cookie 凭据'
                        : '待配置'
                }`}
              >
                <span className="status-dot" />
                {isHealthy ? '正常运行' : account.status === 'error' ? '异常' : '待配置'}
              </span>
            )}
          </div>
        </div>

        {/* 右侧核心动作栏：业务出号组与凭据维护组 */}
        <div className="workspace-hero-actions">
          {/* 出号核心业务组 */}
          <div className="hero-actions-group">
            <button
              type="button"
              className="btn btn-primary btn-sm"
              onClick={onQuickCreate}
              disabled={!account || quickCreating}
            >
              <IconZap size={14} />
              <span>{quickCreating ? '生成中…' : '快速出号'}</span>
            </button>
            <button
              type="button"
              className="btn btn-secondary btn-sm"
              onClick={onCreateCustom}
              disabled={!account}
            >
              <IconPlus size={14} />
              <span>自定义别名</span>
            </button>
          </div>

          <span className="hero-actions-divider" aria-hidden="true" />

          {/* 运维与凭据操作组 (次级克制梯次，不喧宾夺主) */}
          <div className="hero-actions-group is-subtle">
            <button
              type="button"
              className="btn btn-secondary btn-sm"
              onClick={onSync}
              disabled={!account || aliasLoading}
              title="与 Apple 官方服务器强制同步最新别名"
            >
              <IconRefresh size={13} className={aliasLoading ? 'animate-spin' : ''} />
              <span>{aliasLoading ? '同步中…' : '同步数据'}</span>
            </button>
            {onOpenLogin && (
              <button
                type="button"
                className="btn btn-secondary btn-sm"
                onClick={onOpenLogin}
                disabled={!account}
                title="输入 Apple 密码与验证码自动登录以刷新会话 Cookie"
              >
                <IconCloud size={13} />
                <span>iCloud 登录</span>
              </button>
            )}
            <button
              type="button"
              className="btn btn-secondary btn-sm"
              onClick={onOpenCookie}
              disabled={!account}
              title={account?.has_cookies ? 'Cookie 正常有效，点击更新' : 'Cookie 失效或未配置，点击配置'}
            >
              <IconCookie size={13} />
              <span>Cookie</span>
            </button>
            <button
              type="button"
              className="btn btn-secondary btn-sm"
              onClick={onOpenMailbox}
              disabled={!account}
              title={account?.mailbox ? '已连接外部 IMAP 收件箱' : account?.has_app_password ? 'iCloud IMAP 已配置' : '尚未配置收件箱，点击配置'}
            >
              <IconMail size={13} />
              <span>IMAP</span>
            </button>
          </div>
        </div>
      </div>

      {/* ── 告警条（仅在异常时顺滑呈现，平时 0 空间占用） ── */}
      {account && !isHealthy && (
        <div className="workspace-hero-alert">
          <div className="hero-alert-left">
            <IconShield size={14} className="text-warning" />
            <span>
              <b>凭据需更新</b>：当前账号尚未配置可用 Cookie 或处于离线状态，出号与自动同步功能已挂起。
            </span>
          </div>
          <div style={{ display: 'flex', gap: '8px', alignItems: 'center' }}>
            {onOpenLogin && (
              <button
                type="button"
                className="btn btn-xs btn-primary"
                onClick={onOpenLogin}
              >
                密码登录上号
              </button>
            )}
            <button
              type="button"
              className="btn btn-xs btn-warning-action"
              onClick={onOpenCookie}
            >
              手动填写 Cookie
            </button>
          </div>
        </div>
      )}

      {/* ── 底部指标与遥测看板条（左右对称，彻底消除冗余） ── */}
      <div className="workspace-hero-substrip">
        {/* 左侧：舒展大气的配额长进度条与统计 */}
        <div className="hero-inline-stat">
          <IconAliases size={13} className="text-purple" />
          <span className="stat-label">别名配额</span>
          <strong className="stat-value font-mono">
            {totalCount} <span className="stat-sub font-mono">/ {MAX_ALIASES}</span>
          </strong>
          <div className="hero-quota-track" title={`已使用配额 ${quotaPercent}%`}>
            <div
              className={`hero-quota-bar ${quotaPercent > 95 ? 'is-danger' : quotaPercent > 80 ? 'is-warning' : ''}`}
              style={{ width: `${totalCount === 0 ? 0 : Math.max(2, quotaPercent)}%` }}
            />
          </div>
          <span className="stat-tag font-mono">{quotaPercent}%</span>
          {inactiveCount > 0 && (
            <span className="stat-sub font-mono">
              ({activeAliasCount} 活跃 · {inactiveCount} 停用)
            </span>
          )}
        </div>

        {/* 右侧：收件箱连通状态交互微链接（不重复报平安，直接提供信息与操作一体化） */}
        <div className="workspace-hero-meta-group">
          {account?.mailbox ? (
            <button
              type="button"
              className="hero-meta-clickable-link font-mono"
              onClick={onOpenMailbox}
              title={`点击修改收件箱配置 (IMAP: ${account.mailbox.imap_host}:${account.mailbox.imap_port})`}
            >
              <IconMail size={12} className="text-primary" />
              <span>外部收件箱: {account.mailbox.email}</span>
            </button>
          ) : account?.has_app_password ? (
            <button
              type="button"
              className="hero-meta-clickable-link"
              onClick={onOpenMailbox}
              title="点击查看或更新 iCloud IMAP 凭据"
            >
              <IconMail size={12} className="text-success" />
              <span>iCloud 官方 IMAP 已就绪</span>
            </button>
          ) : (
            <button
              type="button"
              className="hero-meta-clickable-link text-muted"
              onClick={onOpenMailbox}
              title="点击配置外部收件箱或专用密码"
            >
              <IconMail size={12} />
              <span>未连接外部收件箱 (点击配置)</span>
            </button>
          )}
        </div>
      </div>
    </div>
  )
}
