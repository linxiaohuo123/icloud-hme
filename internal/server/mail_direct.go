/**
 * [INPUT]: 依赖 gin, html/template, net/http, strings, time, encoding/json, fmt, strconv, icloud-hme/internal/auth, icloud-hme/internal/mail, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 findAccountForEmail, mailViewHandler, mailRawHandler
 * [POS]: internal/server 的对外直出链接管道，验证码与激活链接独立展示，指定邮件预览使用隔离的 HTML 响应
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/auth"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

// findAccountForEmail 按邮箱别名查找归属的母号 ID
func (s *Server) findAccountForEmail(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return ""
	}
	if s.store != nil {
		if accID, ok := s.store.FindAliasRoute(email); ok && accID != "" {
			return accID
		}
	}
	if s.syncWorker != nil {
		if accID, ok := s.syncWorker.GetAliasAccountOK(email); ok && accID != "" {
			return accID
		}
	}
	if s.store != nil {
		if accID, ok := s.store.FindLeaseAccount(email); ok && accID != "" {
			return accID
		}
	}
	// Fallback: 如果系统仅配置了 1 个账号，直接归属该账号
	accs := s.be.ListAccounts()
	if len(accs) == 1 {
		return accs[0].ID
	}
	return ""
}

// resolveMailAccountID resolves the mailbox account on the server side. A
// normal external token is bound to the account stored with its allocation;
// account_id/account query parameters can never redirect it to another account.
func (s *Server) resolveMailAccountID(c *gin.Context, email string, p auth.Principal) (string, bool) {
	requested := strings.TrimSpace(c.Query("account_id"))
	if requested == "" {
		requested = strings.TrimSpace(c.Query("account"))
	}
	if p.Kind == auth.PrincipalToken && !p.IsAdmin() {
		if s.store == nil {
			failCode(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "未找到该别名或无权访问")
			return "", false
		}
		allocation, err := s.store.GetPrincipalAllocation(c.Request.Context(), email, "token", p.ID)
		if err != nil {
			if errors.Is(err, store.ErrAllocationNotFound) {
				failCode(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "未找到该别名或无权访问")
			} else {
				failCode(c, http.StatusInternalServerError, "PERSISTENCE_ERROR", "读取别名归属失败")
			}
			return "", false
		}
		accountID := strings.TrimSpace(allocation.AccountID)
		if accountID == "" || (requested != "" && requested != accountID) {
			failCode(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "未找到该别名或无权访问")
			return "", false
		}
		return accountID, true
	}
	if requested != "" {
		return requested, true
	}
	return s.findAccountForEmail(email), true
}

// mailViewItem 单封邮件视图数据模型
type mailViewItem struct {
	Index         int    `json:"index"`
	ID            string `json:"id"`
	Subject       string `json:"subject"`
	From          string `json:"from"`
	SenderName    string `json:"sender_name"`
	SenderEmail   string `json:"sender_email"`
	SenderInitial string `json:"sender_initial"`
	Date          string `json:"date"`
	FormattedDate string `json:"formatted_date"`
	RelativeDate  string `json:"relative_date"`
	Preview       string `json:"preview"`
	TextBody      string `json:"text_body"`
	HTMLBody      string `json:"html_body"`
	IsHTML        bool   `json:"is_html"`
	HasOTP        bool   `json:"has_otp"`
	Code          string `json:"code"`
	MagicLink     string `json:"magic_link"`
}

// mailViewData 查信页主视图模型
type mailViewData struct {
	Email       string         `json:"email"`
	AccountID   string         `json:"account_id"`
	AccountName string         `json:"account_name"`
	RefreshTime string         `json:"refresh_time"`
	HasMail     bool           `json:"has_mail"`
	TotalCount  int            `json:"total_count"`
	Items       []mailViewItem `json:"items"`
	LatestItem  mailViewItem   `json:"latest_item"`
	AllOTPs     []mailViewItem `json:"all_otps"`
	ItemsJSON   string         `json:"-"`
}

func formatRelativeTime(dateStr string) (string, string) {
	var t time.Time
	var err error
	if t, err = time.Parse(time.RFC3339, dateStr); err != nil {
		if t, err = time.Parse(time.RFC1123Z, dateStr); err != nil {
			t, err = time.Parse(time.RFC1123, dateStr)
		}
	}
	if err != nil || t.IsZero() {
		return dateStr, dateStr
	}
	localT := t.Local()
	formatted := localT.Format("2006-01-02 15:04:05")
	diff := time.Since(localT)
	var rel string
	if diff < 0 {
		rel = "刚刚"
	} else if diff < time.Minute {
		rel = "刚刚"
	} else if diff < time.Hour {
		rel = fmt.Sprintf("%d分钟前", int(diff.Minutes()))
	} else if diff < 24*time.Hour {
		rel = fmt.Sprintf("%d小时前", int(diff.Hours()))
	} else if diff < 7*24*time.Hour {
		rel = fmt.Sprintf("%d天前", int(diff.Hours()/24))
	} else {
		rel = localT.Format("01-02 15:04")
	}
	return formatted, rel
}

var mailViewTemplate = template.Must(template.New("mailView").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1, maximum-scale=1">
  <title>{{if .HasMail}}收件箱 ({{.TotalCount}}) · {{.Email}}{{else}}收件箱 · {{.Email}}{{end}}</title>
  <link rel="icon" id="pageFavicon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'><path fill='%232563eb' d='M26 15a7 7 0 0 0-13.4-2.8A5 5 0 0 0 4 17a5 5 0 0 0 5 5h17a5 5 0 0 0 0-10z'/><path fill='%2338bdf8' d='M18 10l-4 6h3v5l5-7h-4z'/></svg>">
  <link rel="stylesheet" href="/mail/view-assets.css">

</head>
<body>
  <div class="wrapper">
    <!-- Top Bar -->
    <header class="top-bar">
      <div class="brand-area">
        <div class="brand-logo">
          <svg viewBox="0 0 24 24"><path d="M19.35 10.04C18.67 6.59 15.64 4 12 4 9.11 4 6.6 5.64 5.35 8.04 2.34 8.36 0 10.91 0 14c0 3.31 2.69 6 6 6h13c2.76 0 5-2.24 5-5 0-2.64-2.05-4.78-4.65-4.96zM19 18H6c-2.21 0-4-1.79-4-4 0-2.05 1.53-3.76 3.56-3.97l1.07-.11.5-.95C8.08 7.14 9.94 6 12 6c2.62 0 4.88 1.86 5.39 4.43l.3 1.5 1.53.11c1.56.1 2.78 1.41 2.78 2.96 0 1.65-1.35 3-3 3z"/></svg>
        </div>
        <div class="brand-title">
          <span>iCloud 隐私查信</span>
          {{if .HasMail}}
          <span class="status-badge live"><span class="pulse-dot"></span> 实时就绪</span>
          {{else}}
          <span class="status-badge idle"><span class="pulse-dot"></span> 实时监听中</span>
          {{end}}
        </div>
      </div>
      <div class="top-actions">
        <div class="refresh-pill" id="refreshPill" data-action="toggle-refresh" title="点击暂停/继续自动检测">
          ⏱️ <span id="countdownSec">3s</span> 自动检测
        </div>
        <button class="btn btn-secondary" data-action="refresh" title="按 R 键立即刷新">
          <svg id="refreshSpin" viewBox="0 0 24 24"><path d="M17.65 6.35C16.2 4.9 14.21 4 12 4c-4.42 0-7.99 3.58-7.99 8s3.57 8 7.99 8c3.73 0 6.84-2.55 7.73-6h-2.08c-.82 2.33-3.04 4-5.65 4-3.31 0-6-2.69-6-6s2.69-6 6-6c1.66 0 3.14.69 4.22 1.78L13 11h7V4l-2.35 2.35z"/></svg>
          <span>刷新</span>
        </button>
      </div>
    </header>

    <!-- Alias Banner -->
    <div class="alias-banner">
      <div class="alias-info">
        <span class="alias-label">目标别名:</span>
        <span class="alias-chip" data-action="copy-email" data-email="{{.Email}}" title="点击复制邮箱">
          <span>{{.Email}}</span>
          <svg viewBox="0 0 24 24"><path d="M16 1H4c-1.1 0-2 .9-2 2v14h2V3h12V1zm3 4H8c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h11c1.1 0 2-.9 2-2V7c0-1.1-.9-2-2-2zm0 16H8V7h11v14z"/></svg>
        </span>
        {{if .AccountName}}
        <span class="alias-meta">· 母号: {{.AccountName}}</span>
        {{end}}
      </div>
      <div>
        <button class="btn btn-secondary compact-btn" data-action="copy-url">
          <svg viewBox="0 0 24 24"><path d="M3.9 12c0-1.71 1.39-3.1 3.1-3.1h4V7H7c-2.76 0-5 2.24-5 5s2.24 5 5 5h4v-1.9H7c-1.71 0-3.1-1.39-3.1-3.1zM8 13h8v-2H8v2zm9-6h-4v1.9h4c1.71 0 3.1 1.39 3.1 3.1s-1.39 3.1-3.1 3.1h-4V17h4c2.76 0 5-2.24 5-5s-2.24-5-5-5z"/></svg>
          <span>复制查信链接</span>
        </button>
      </div>
    </div>

    <!-- MASTER OTP SUMMARY BAR (All extracted codes prominently displayed) -->
    <section class="otp-master-bar{{if not .AllOTPs}} is-hidden{{end}}" id="otpMasterBar">
      <div class="otp-latest-row">
        <div class="otp-latest-left">
          <div class="otp-latest-tag">
            <svg viewBox="0 0 24 24"><path d="M19 9l1.25-2.75L23 5l-2.75-1.25L19 1l-1.25 2.75L15 5l2.75 1.25L19 9zm-7.5.5L9 4 6.5 9.5 1 12l5.5 2.5L9 20l2.5-5.5L17 12l-5.5-2.5zM19 15l-1.25 2.75L15 19l2.75 1.25L19 23l1.25-2.75L23 19l-2.75-1.25L19 15z"/></svg>
            <span id="topOtpLabel">{{if and .AllOTPs (index .AllOTPs 0).Code}}提取到验证码{{else}}提取到激活链接{{end}}</span>
          </div>
          <div class="otp-code-highlight{{if or (not .AllOTPs) (not (index .AllOTPs 0).Code)}} is-hidden{{end}}" id="topLatestOtp" data-action="copy-latest-otp" title="点击复制最新验证码">
            {{if .AllOTPs}}{{(index .AllOTPs 0).Code}}{{end}}
          </div>
        </div>
        <div class="otp-actions">
          <button class="btn btn-primary{{if or (not .AllOTPs) (not (index .AllOTPs 0).Code)}} is-hidden{{end}}" id="topLatestCopyBtn" data-action="copy-latest-otp">
            <svg viewBox="0 0 24 24"><path d="M16 1H4c-1.1 0-2 .9-2 2v14h2V3h12V1zm3 4H8c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h11c1.1 0 2-.9 2-2V7c0-1.1-.9-2-2-2zm0 16H8V7h11v14z"/></svg>
            <span>复制验证码</span>
          </button>
          <a class="btn btn-magic{{if or (not .AllOTPs) (not (index .AllOTPs 0).MagicLink)}} is-hidden{{end}}" id="topMagicBtn" href="{{if .AllOTPs}}{{(index .AllOTPs 0).MagicLink}}{{end}}" target="_blank" rel="noopener noreferrer">
            <svg viewBox="0 0 24 24"><path d="M19 19H5V5h7V3H5c-1.11 0-2 .9-2 2v14c0 1.1.89 2 2 2h14c1.1 0 2-.9 2-2v-7h-2v7zM14 3v2h3.59l-9.83 9.83 1.41 1.41L19 6.41V10h2V3h-7z"/></svg>
            <span>打开链接 ↗</span>
          </a>
        </div>
      </div>

      <!-- Historical OTPs Chips Row -->
      <div class="otp-history-section{{if le (len .AllOTPs) 1}} is-hidden{{end}}" id="otpHistorySection">
        <span class="otp-history-label">全部验证信息 ({{len .AllOTPs}} 条):</span>
        {{range .AllOTPs}}
        {{if .Code}}
        <button class="otp-chip-btn" data-action="copy-code" data-code="{{.Code}}" title="点击复制此验证码 (来自: {{.SenderName}} · {{.RelativeDate}})">
          <span class="otp-chip-code">{{.Code}}</span>
          <span>· {{.SenderName}} ({{.RelativeDate}})</span>
        </button>
        {{else}}
        <a class="otp-chip-btn" href="{{.MagicLink}}" target="_blank" rel="noopener noreferrer">激活链接 · {{.SenderName}} ({{.RelativeDate}})</a>
        {{end}}
        {{end}}
      </div>
    </section>

    <!-- TWO-COLUMN MASTER-DETAIL LAYOUT -->
    <div class="main-grid{{if not .HasMail}} is-hidden{{end}}" id="mainGrid">
      <!-- Left Column: Mail List -->
      <aside class="inbox-card">
        <div class="inbox-header">
          <div class="inbox-title">
            <svg class="icon-16" viewBox="0 0 24 24"><path d="M20 4H4c-1.1 0-1.99.9-1.99 2L2 18c0 1.1.9 2 2 2h16c1.1 0 2-.9 2-2V6c0-1.1-.9-2-2-2zm0 4l-8 5-8-5V6l8 5 8-5v2z"/></svg>
            <span>收件列表</span>
            <span class="count-badge" id="inboxCountBadge">共 {{.TotalCount}} 封</span>
          </div>
        </div>
        <div class="mail-list" id="mailListContainer">
          {{range .Items}}
          <div class="mail-item {{if eq .Index 0}}active{{end}}" id="mailItem-{{.Index}}" data-action="select-mail" data-index="{{.Index}}">
            <div class="mail-item-top">
              <div class="sender-box">
                <div class="mini-avatar">{{.SenderInitial}}</div>
                <span class="mini-sender">{{.SenderName}}</span>
              </div>
              <span class="mini-time">{{.RelativeDate}}</span>
            </div>
            <div class="mail-item-subject">{{.Subject}}</div>
            <div class="mail-item-bottom">
              {{if .Code}}
              <div class="item-otp-pill">
                <span>⚡ {{.Code}}</span>
                <span class="quick-copy" data-action="copy-code" data-code="{{.Code}}" title="复制验证码">复制</span>
              </div>
              {{else}}
              <div class="item-preview">{{.Subject}}</div>
              {{end}}
            </div>
          </div>
          {{end}}
        </div>
      </aside>

      <!-- Right Column: Detail Reader -->
      <main class="detail-card">
        <div class="detail-header">
          <h1 class="detail-subject" id="detailSubject">{{.LatestItem.Subject}}</h1>
          <div class="detail-meta-row">
            <div class="sender-details-group">
              <div class="detail-avatar" id="detailAvatar">{{.LatestItem.SenderInitial}}</div>
              <div class="sender-meta">
                <span class="sender-fullname" id="detailSenderName">{{.LatestItem.SenderName}}</span>
                <span class="sender-address" id="detailSenderEmail">{{.LatestItem.SenderEmail}}</span>
              </div>
            </div>
            <div class="detail-date-badge">
              <svg class="icon-14" viewBox="0 0 24 24"><path d="M11.99 2C6.47 2 2 6.48 2 12s4.47 10 9.99 10C17.52 22 22 17.52 22 12S17.52 2 11.99 2zM12 20c-4.42 0-8-3.58-8-8s3.58-8 8-8 8 3.58 8 8-3.58 8-8 8zm.5-13H11v6l5.25 3.15.75-1.23-4.5-2.67z"/></svg>
              <span id="detailDate">{{.LatestItem.FormattedDate}}</span>
            </div>
          </div>
        </div>

        <!-- Detail OTP Strip (Visible if current email has OTP) -->
        <div class="detail-otp-box{{if not .LatestItem.HasOTP}} is-hidden{{end}}" id="detailOtpBox">
          <div class="detail-otp-left{{if not .LatestItem.Code}} is-hidden{{end}}" id="detailOtpCodeBox">
            <span class="detail-otp-label">本信验证码:</span>
            <span class="detail-otp-code" id="detailOtpCode" data-action="copy-detail-otp">{{.LatestItem.Code}}</span>
          </div>
          <div class="detail-otp-actions">
            <button class="btn btn-primary compact-btn{{if not .LatestItem.Code}} is-hidden{{end}}" id="detailOtpCopyBtn" data-action="copy-detail-otp">
              <svg viewBox="0 0 24 24"><path d="M16 1H4c-1.1 0-2 .9-2 2v14h2V3h12V1zm3 4H8c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h11c1.1 0 2-.9 2-2V7c0-1.1-.9-2-2-2zm0 16H8V7h11v14z"/></svg>
              <span>复制</span>
            </button>
            <a class="btn btn-magic compact-btn{{if not .LatestItem.MagicLink}} is-hidden{{end}}" id="detailMagicBtn" href="{{.LatestItem.MagicLink}}" target="_blank" rel="noopener noreferrer">
              <span>打开激活链接 ↗</span>
            </a>
          </div>
        </div>

        <!-- Toolbar & Tabs -->
        <div class="mail-toolbar">
          <div class="tabs">
            <button class="tab-btn active" id="tab-styled" data-action="switch-tab" data-view="styled">💬 优雅排版</button>
            <button class="tab-btn" id="tab-html" data-action="switch-tab" data-view="html">🌐 网页视图</button>
            <button class="tab-btn" id="tab-raw" data-action="switch-tab" data-view="raw">📄 原始文本</button>
          </div>
          <div>
            <button class="btn btn-secondary compact-btn" data-action="copy-full-body">
              <svg viewBox="0 0 24 24"><path d="M16 1H4c-1.1 0-2 .9-2 2v14h2V3h12V1zm3 4H8c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h11c1.1 0 2-.9 2-2V7c0-1.1-.9-2-2-2zm0 16H8V7h11v14z"/></svg>
              <span>复制全文</span>
            </button>
          </div>
        </div>

        <!-- Body Views Container -->
        <div class="body-wrapper">
          <!-- Formatted Styled Text View -->
          <div class="body-view active" id="view-styled">
            <div class="styled-text-content" id="styledContentArea"></div>
          </div>
          <!-- HTML View -->
          <div class="body-view" id="view-html">
            <div class="iframe-container">
              <iframe id="mailFrame" class="body-frame" sandbox="allow-popups" title="邮件正文"></iframe>
            </div>
          </div>
          <!-- Raw Text View -->
          <div class="body-view" id="view-raw">
            <div class="raw-text-content" id="rawContentArea"></div>
          </div>
        </div>
      </main>
    </div>

    <!-- Empty State -->
    <div class="empty-card{{if .HasMail}} is-hidden{{end}}" id="emptyCard">
      <div class="radar-box">
        <div class="radar-ring"></div>
        <svg viewBox="0 0 24 24"><path d="M20 4H4c-1.1 0-1.99.9-1.99 2L2 18c0 1.1.9 2 2 2h16c1.1 0 2-.9 2-2V6c0-1.1-.9-2-2-2zm0 4l-8 5-8-5V6l8 5 8-5v2z"/></svg>
      </div>
      <div class="empty-title">正在等待邮件送达...</div>
      <div class="empty-desc">
        系统已实时建立对 <strong>{{.Email}}</strong> 的查信通道。<br>
        第三方发件方通常需 3~10 秒完成投递，页面正在后台自动无感检测中。
      </div>
      <div class="empty-progress">
        <div class="empty-progress-bar" id="emptyProgressBar"></div>
      </div>
      <button class="btn btn-primary empty-refresh-btn" data-action="manual-refresh">
        <span>立即检查新邮件</span>
      </button>
    </div>

    <footer class="footer">
      iCloud 隐私邮箱轻量查信平台 · 更新于 {{.RefreshTime}}
    </footer>
  </div>

  <!-- Floating Toast Notification -->
  <div class="toast" id="toast"></div>

  <div id="mailViewData" data-items="{{.ItemsJSON}}" data-email="{{.Email}}" data-account-id="{{.AccountID}}"></div>
  <script src="/mail/view-assets.js" defer></script>
</body>
</html>
`))

// mailViewHandler 处理 GET /mail/view。
// 参数:
//
//	email / alias (必须): 别名邮箱
func (s *Server) mailViewHandler(c *gin.Context) {
	email := strings.ToLower(strings.TrimSpace(c.Param("email")))
	if email == "" {
		email = strings.ToLower(strings.TrimSpace(c.Query("email")))
	}
	if email == "" {
		email = strings.ToLower(strings.TrimSpace(c.Query("alias")))
	}
	if email == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "email 参数必填")
		return
	}

	p, exists := getPrincipal(c)
	if !exists || !p.CanVerify() {
		failCode(c, http.StatusForbidden, "SCOPE_DENIED", "当前主体无权查看此别名邮件")
		return
	}
	accountID, allowed := s.resolveMailAccountID(c, email, p)
	if !allowed {
		return
	}

	if s.syncWorker != nil {
		s.syncWorker.Trigger()
	}

	data := mailViewData{
		Email:       email,
		RefreshTime: time.Now().Format("15:04:05"),
		Items:       []mailViewItem{},
		AllOTPs:     []mailViewItem{},
		ItemsJSON:   "[]",
	}

	if accountID != "" {
		data.AccountID = accountID
		if snap, err := s.be.GetAccount(accountID); err == nil && snap.Name != "" {
			data.AccountName = snap.Name
		}
		items, err := s.fetchRecentMessagesForAlias(c.Request.Context(), accountID, email, 20)
		if err != nil && (c.Query("format") == "json" || strings.Contains(c.GetHeader("Accept"), "application/json")) {
			c.Header("Cache-Control", "no-cache, no-store, must-revalidate, max-age=0")
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "同步邮件失败，稍后重试: " + err.Error(),
			})
			return
		}
		if err == nil && len(items) > 0 {
			data.HasMail = true
			data.TotalCount = len(items)
			data.Items = items
			data.LatestItem = items[0]

			allOtps := make([]mailViewItem, 0, len(items))
			for _, it := range items {
				if it.HasOTP && (it.Code != "" || it.MagicLink != "") {
					allOtps = append(allOtps, it)
				}
			}
			data.AllOTPs = allOtps

			b, _ := json.Marshal(items)
			data.ItemsJSON = string(b)
		}
	}

	if c.Query("format") == "json" || strings.Contains(c.GetHeader("Accept"), "application/json") {
		c.Header("Cache-Control", "no-cache, no-store, must-revalidate, max-age=0")
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"data":    data,
		})
		return
	}

	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-cache, no-store, must-revalidate, max-age=0")
	c.Header("Pragma", "no-cache")
	c.Header("Expires", "0")
	_ = mailViewTemplate.Execute(c.Writer, data)
}

// mailRawHandler 处理 GET /mail/raw。
// 参数:
//
//	email / alias (必须): 别名邮箱
//	format (可选): html 或 text (默认 text)
//	message_id (可选): 当前别名最近 20 封中的指定邮件；省略时返回最新邮件
//	frame (可选): 1 表示 HTML 允许被同源查信页嵌入，仍禁止脚本与同源权限
func (s *Server) mailRawHandler(c *gin.Context) {
	email := strings.ToLower(strings.TrimSpace(c.Param("email")))
	if email == "" {
		email = strings.ToLower(strings.TrimSpace(c.Query("email")))
	}
	if email == "" {
		email = strings.ToLower(strings.TrimSpace(c.Query("alias")))
	}
	if email == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "email 参数必填")
		return
	}

	p, exists := getPrincipal(c)
	if !exists || !p.CanVerify() {
		failCode(c, http.StatusForbidden, "SCOPE_DENIED", "当前主体无权查看此别名邮件")
		return
	}
	accountID, allowed := s.resolveMailAccountID(c, email, p)
	if !allowed {
		return
	}

	if s.syncWorker != nil {
		s.syncWorker.Trigger()
	}

	if accountID == "" {
		c.Data(http.StatusNotFound, "text/plain; charset=utf-8", []byte("NO_ACCOUNT_FOR_ALIAS"))
		return
	}

	messageID := strings.TrimSpace(c.Query("message_id"))
	limit := 1
	if messageID != "" {
		limit = 20
	}
	items, err := s.fetchRecentMessagesForAlias(c.Request.Context(), accountID, email, limit)
	if err != nil || len(items) == 0 {
		c.Data(http.StatusNotFound, "text/plain; charset=utf-8", []byte("NO_EMAIL_RECEIVED"))
		return
	}

	latest := items[0]
	if messageID != "" {
		found := false
		for _, item := range items {
			if item.ID == messageID {
				latest, found = item, true
				break
			}
		}
		if !found {
			c.Data(http.StatusNotFound, "text/plain; charset=utf-8", []byte("MESSAGE_NOT_FOUND"))
			return
		}
	}
	format := strings.ToLower(strings.TrimSpace(c.Query("format")))
	if format == "html" {
		// The body is untrusted mailbox content. Keep the preview renderable while
		// preventing scripts, forms, plugins, network requests, and framing.
		ancestors := "'none'"
		if c.Query("frame") == "1" {
			ancestors = "'self'"
		}
		c.Header("Content-Security-Policy", "sandbox allow-popups; default-src 'none'; script-src 'none'; style-src 'unsafe-inline'; img-src data:; connect-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors "+ancestors)
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(latest.HTMLBody))
	} else {
		c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(latest.TextBody))
	}
}

// fetchRecentMessagesForAlias 检索指定别名的最近多封邮件完整详情与验证码提取
func (s *Server) fetchRecentMessagesForAlias(ctx context.Context, accountID, email string, limit int) ([]mailViewItem, error) {
	if limit <= 0 {
		limit = 20
	}
	query := InboxQuery{
		AccountID:       accountID,
		Alias:           email,
		Folder:          "INBOX",
		Limit:           limit,
		Days:            30,
		WithBody:        true,
		FolderSpecified: false,
		DaysSpecified:   false,
		Refresh:         true,
	}

	var result InboxResult
	var err error
	if s.mailReadService != nil {
		result, err = s.mailReadService.ListInbox(ctx, query)
	} else {
		result, err = s.be.ListInboxContext(ctx, query)
	}
	if err != nil || len(result.Messages) == 0 {
		return nil, err
	}

	// 批量检索详情
	bodyMap := make(map[string]*mail.FullMessage)
	if s.mailReadService != nil {
		reqItems := make([]batchMessageItemReq, 0, len(result.Messages))
		for _, m := range result.Messages {
			reqItems = append(reqItems, batchMessageItemReq{
				MessageRef: m.MessageRef,
				Folder:     m.Folder,
				ID:         m.ID,
				UID:        strconv.FormatUint(uint64(m.UID), 10),
			})
		}
		fullMsgs, _, err := s.mailReadService.GetMessagesBatch(ctx, accountID, reqItems)
		if err == nil {
			for _, fm := range fullMsgs {
				if fm != nil {
					if fm.MessageRef != "" {
						bodyMap[fm.MessageRef] = fm
					}
					if fm.ID != "" {
						bodyMap[fm.ID] = fm
					}
				}
			}
		}
	}

	items := make([]mailViewItem, 0, len(result.Messages))
	for idx, m := range result.Messages {
		fullMsg := bodyMap[m.MessageRef]
		if fullMsg == nil {
			fullMsg = bodyMap[m.ID]
		}
		// 单项回退获取详情
		if fullMsg == nil && s.mailReadService != nil {
			targetID := m.MessageRef
			if targetID == "" {
				targetID = m.ID
			}
			if fm, _, _, _, err := s.mailReadService.GetMessageDetail(ctx, accountID, targetID); err == nil && fm != nil {
				fullMsg = fm
			}
		}

		subject := m.Subject
		from := m.From
		date := m.Date
		body := m.Preview
		if fullMsg != nil {
			if fullMsg.Subject != "" {
				subject = fullMsg.Subject
			}
			if fullMsg.From != "" {
				from = fullMsg.From
			}
			if fullMsg.Date != "" {
				date = fullMsg.Date
			}
			if fullMsg.Body != "" {
				body = fullMsg.Body
			}
		}

		senderName := from
		senderEmail := from
		senderInitial := "M"
		if i := strings.Index(from, "<"); i >= 0 {
			namePart := strings.TrimSpace(from[:i])
			emailPart := strings.Trim(strings.TrimSpace(from[i:]), "<>")
			if namePart != "" {
				senderName = strings.Trim(namePart, `"' `)
			} else {
				senderName = emailPart
			}
			senderEmail = emailPart
		}
		cleanName := strings.Trim(senderName, `"' `)
		if len(cleanName) > 0 {
			r := []rune(cleanName)
			senderInitial = strings.ToUpper(string(r[0]))
		}

		formattedDate, relDate := formatRelativeTime(date)

		isHTML := strings.Contains(body, "<html") ||
			strings.Contains(body, "<body") ||
			strings.Contains(body, "<div") ||
			strings.Contains(body, "<table") ||
			strings.Contains(body, "<p>") ||
			strings.Contains(body, "<br") ||
			strings.Contains(body, "<section") ||
			strings.Contains(body, "<style")

		item := mailViewItem{
			Index:         idx,
			ID:            m.ID,
			Subject:       subject,
			From:          from,
			SenderName:    senderName,
			SenderEmail:   senderEmail,
			SenderInitial: senderInitial,
			Date:          date,
			FormattedDate: formattedDate,
			RelativeDate:  relDate,
			Preview:       m.Preview,
			TextBody:      body,
			HTMLBody:      body,
			IsHTML:        isHTML,
		}

		otp := mail.ExtractOTP(subject, body)
		if otp != nil && (otp.Code != "" || otp.MagicLink != "") {
			item.HasOTP = true
			item.Code = otp.Code
			item.MagicLink = otp.MagicLink
		}

		items = append(items, item)
	}

	return items, nil
}
