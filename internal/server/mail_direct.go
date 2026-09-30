/**
 * [INPUT]: 依赖 gin, html/template, net/http, strings, time, encoding/json, fmt, strconv, icloud-hme/internal/auth, icloud-hme/internal/mail, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 findAccountForEmail, mailViewHandler, mailRawHandler
 * [POS]: internal/server 的对外直出链接管道，支持 IMAP 正文与 WebMail 预览查信、多条验证码提取与可视化正文直出
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/auth"
	"icloud-hme/internal/mail"
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
	ItemsJSON   template.JS    `json:"-"`
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
  <style>
    :root {
      --bg: #070b14;
      --card-bg: rgba(15, 23, 42, 0.78);
      --card-border: rgba(255, 255, 255, 0.08);
      --card-border-hover: rgba(59, 130, 246, 0.35);
      --inner-bg: rgba(30, 41, 59, 0.45);
      --text: #f8fafc;
      --text-muted: #94a3b8;
      --text-sub: #64748b;
      --brand: #2563eb;
      --brand-hover: #1d4ed8;
      --brand-glow: rgba(37, 99, 235, 0.25);
      --accent-cyan: #38bdf8;
      --accent-cyan-glow: rgba(56, 189, 248, 0.2);
      --success: #10b981;
      --success-glow: rgba(16, 185, 129, 0.2);
      --radius: 14px;
      --radius-sm: 8px;
    }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      background: radial-gradient(circle at 50% -10%, #1e293b 0%, #070b14 65%);
      background-attachment: fixed;
      color: var(--text);
      font-family: -apple-system, BlinkMacSystemFont, "SF Pro Text", "Segoe UI", Roboto, "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", sans-serif;
      min-height: 100vh;
      display: flex;
      flex-direction: column;
      align-items: center;
      padding: 20px 16px 40px;
      line-height: 1.5;
      -webkit-font-smoothing: antialiased;
    }
    .wrapper {
      width: 100%;
      max-width: 1240px;
      display: flex;
      flex-direction: column;
      gap: 16px;
    }

    /* Top App Bar */
    .top-bar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 12px 20px;
      background: rgba(15, 23, 42, 0.7);
      backdrop-filter: blur(16px);
      -webkit-backdrop-filter: blur(16px);
      border: 1px solid var(--card-border);
      border-radius: var(--radius);
      box-shadow: 0 4px 20px -2px rgba(0, 0, 0, 0.4);
      flex-wrap: wrap;
      gap: 12px;
    }
    .brand-area {
      display: flex;
      align-items: center;
      gap: 12px;
    }
    .brand-logo {
      width: 36px;
      height: 36px;
      border-radius: 10px;
      background: linear-gradient(135deg, #2563eb, #38bdf8);
      display: flex;
      align-items: center;
      justify-content: center;
      box-shadow: 0 0 16px var(--brand-glow);
      flex-shrink: 0;
    }
    .brand-logo svg { width: 20px; height: 20px; fill: #fff; }
    .brand-title {
      font-size: 16px;
      font-weight: 700;
      color: #fff;
      display: flex;
      align-items: center;
      gap: 8px;
    }
    .status-badge {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      padding: 3px 10px;
      border-radius: 999px;
      font-size: 11.5px;
      font-weight: 600;
    }
    .status-badge.live {
      background: rgba(16, 185, 129, 0.15);
      color: #34d399;
      border: 1px solid rgba(16, 185, 129, 0.3);
    }
    .status-badge.live .pulse-dot {
      width: 6px;
      height: 6px;
      border-radius: 50%;
      background: #10b981;
      box-shadow: 0 0 8px #10b981;
      animation: pulse 2s infinite;
    }
    .status-badge.idle {
      background: rgba(245, 158, 11, 0.15);
      color: #fbbf24;
      border: 1px solid rgba(245, 158, 11, 0.3);
    }
    .status-badge.idle .pulse-dot {
      width: 6px;
      height: 6px;
      border-radius: 50%;
      background: #f59e0b;
      box-shadow: 0 0 8px #f59e0b;
      animation: pulse 1.5s infinite;
    }
    @keyframes pulse {
      0%, 100% { opacity: 1; transform: scale(1); }
      50% { opacity: 0.35; transform: scale(0.85); }
    }

    .top-actions {
      display: flex;
      align-items: center;
      gap: 10px;
      flex-wrap: wrap;
    }
    .refresh-pill {
      font-size: 12px;
      color: var(--text-muted);
      background: var(--inner-bg);
      border: 1px solid var(--card-border);
      padding: 6px 12px;
      border-radius: 999px;
      display: inline-flex;
      align-items: center;
      gap: 6px;
      user-select: none;
      cursor: pointer;
      transition: all 0.2s ease;
    }
    .refresh-pill:hover {
      border-color: var(--card-border-hover);
      color: var(--text);
    }

    /* Alias Info Strip */
    .alias-banner {
      background: var(--card-bg);
      border: 1px solid var(--card-border);
      border-radius: var(--radius);
      padding: 12px 18px;
      display: flex;
      align-items: center;
      justify-content: space-between;
      flex-wrap: wrap;
      gap: 12px;
      backdrop-filter: blur(12px);
    }
    .alias-info {
      display: flex;
      align-items: center;
      gap: 10px;
      flex-wrap: wrap;
    }
    .alias-label {
      font-size: 13px;
      color: var(--text-muted);
      font-weight: 500;
    }
    .alias-chip {
      background: rgba(37, 99, 235, 0.12);
      border: 1px solid rgba(59, 130, 246, 0.3);
      color: #93c5fd;
      padding: 4px 12px;
      border-radius: 999px;
      font-size: 14px;
      font-weight: 600;
      display: inline-flex;
      align-items: center;
      gap: 6px;
      cursor: pointer;
      transition: all 0.2s ease;
    }
    .alias-chip:hover {
      background: rgba(37, 99, 235, 0.22);
      border-color: #3b82f6;
      color: #fff;
    }
    .alias-chip svg { width: 13px; height: 13px; fill: currentColor; opacity: 0.8; }
    .alias-meta { font-size: 12px; color: var(--text-sub); }

    /* MASTER OTP SUMMARY BAR (All extracted codes) */
    .otp-master-bar {
      background: linear-gradient(135deg, rgba(14, 165, 233, 0.14) 0%, rgba(59, 130, 246, 0.1) 40%, rgba(15, 23, 42, 0.85) 100%);
      border: 1px solid rgba(56, 189, 248, 0.38);
      border-radius: var(--radius);
      padding: 18px 22px;
      box-shadow: 0 12px 32px -8px rgba(56, 189, 248, 0.2);
      display: flex;
      flex-direction: column;
      gap: 12px;
      position: relative;
      overflow: hidden;
    }
    .otp-master-bar::before {
      content: "";
      position: absolute;
      top: 0; left: 0; right: 0; height: 2px;
      background: linear-gradient(90deg, #38bdf8, #818cf8, #34d399);
    }
    .otp-latest-row {
      display: flex;
      align-items: center;
      justify-content: space-between;
      flex-wrap: wrap;
      gap: 14px;
    }
    .otp-latest-left {
      display: flex;
      align-items: center;
      gap: 14px;
      flex-wrap: wrap;
    }
    .otp-latest-tag {
      font-size: 13px;
      font-weight: 700;
      color: #7dd3fc;
      display: flex;
      align-items: center;
      gap: 6px;
      letter-spacing: 0.03em;
    }
    .otp-latest-tag svg { width: 16px; height: 16px; fill: currentColor; }
    .otp-code-highlight {
      font-family: "JetBrains Mono", ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
      font-size: 34px;
      font-weight: 800;
      letter-spacing: 5px;
      background: linear-gradient(135deg, #ffffff 40%, #7dd3fc 100%);
      -webkit-background-clip: text;
      -webkit-text-fill-color: transparent;
      line-height: 1;
      padding: 6px 14px;
      background-color: rgba(0, 0, 0, 0.35);
      border: 1px solid rgba(56, 189, 248, 0.3);
      border-radius: 10px;
      cursor: pointer;
      user-select: all;
      transition: all 0.2s ease;
    }
    .otp-code-highlight:hover {
      border-color: #38bdf8;
      box-shadow: 0 0 16px rgba(56, 189, 248, 0.3);
      transform: scale(1.02);
    }
    .otp-history-section {
      border-top: 1px solid rgba(255, 255, 255, 0.08);
      padding-top: 10px;
      display: flex;
      align-items: center;
      flex-wrap: wrap;
      gap: 8px;
    }
    .otp-history-label {
      font-size: 12px;
      color: var(--text-muted);
      font-weight: 600;
    }
    .otp-chip-btn {
      background: rgba(15, 23, 42, 0.6);
      border: 1px solid rgba(56, 189, 248, 0.25);
      color: #e2e8f0;
      font-size: 12px;
      font-weight: 600;
      padding: 4px 10px;
      border-radius: 6px;
      display: inline-flex;
      align-items: center;
      gap: 6px;
      cursor: pointer;
      transition: all 0.15s ease;
    }
    .otp-chip-btn:hover {
      background: rgba(56, 189, 248, 0.15);
      border-color: #38bdf8;
      color: #fff;
    }
    .otp-chip-code {
      font-family: "JetBrains Mono", monospace;
      color: #38bdf8;
      font-weight: 700;
    }

    /* Buttons */
    .btn {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      gap: 6px;
      padding: 8px 14px;
      border-radius: 8px;
      font-size: 13px;
      font-weight: 600;
      cursor: pointer;
      border: none;
      outline: none;
      text-decoration: none;
      transition: all 0.18s ease;
      white-space: nowrap;
    }
    .btn svg { width: 15px; height: 15px; fill: currentColor; }
    .btn-primary {
      background: linear-gradient(135deg, #2563eb, #1d4ed8);
      color: #fff;
      box-shadow: 0 4px 14px rgba(37, 99, 235, 0.35);
    }
    .btn-primary:hover {
      background: linear-gradient(135deg, #3b82f6, #2563eb);
      box-shadow: 0 6px 20px rgba(37, 99, 235, 0.5);
      transform: translateY(-1px);
    }
    .btn-secondary {
      background: rgba(30, 41, 59, 0.7);
      color: var(--text);
      border: 1px solid var(--card-border);
    }
    .btn-secondary:hover {
      background: rgba(51, 65, 85, 0.85);
      border-color: var(--card-border-hover);
      color: #fff;
    }
    .btn-magic {
      background: linear-gradient(135deg, #4f46e5, #7c3aed);
      color: #fff;
      box-shadow: 0 4px 14px rgba(99, 102, 241, 0.3);
    }
    .btn-magic:hover {
      background: linear-gradient(135deg, #6366f1, #8b5cf6);
      transform: translateY(-1px);
    }

    /* MAIN TWO-COLUMN MASTER-DETAIL LAYOUT */
    .main-grid {
      display: grid;
      grid-template-columns: 360px 1fr;
      gap: 16px;
      align-items: start;
    }

    /* Left Column: Email List */
    .inbox-card {
      background: var(--card-bg);
      border: 1px solid var(--card-border);
      border-radius: var(--radius);
      box-shadow: 0 10px 30px -5px rgba(0, 0, 0, 0.35);
      backdrop-filter: blur(16px);
      overflow: hidden;
      display: flex;
      flex-direction: column;
    }
    .inbox-header {
      padding: 14px 18px;
      border-bottom: 1px solid var(--card-border);
      display: flex;
      align-items: center;
      justify-content: space-between;
    }
    .inbox-title {
      font-size: 14.5px;
      font-weight: 700;
      color: #fff;
      display: flex;
      align-items: center;
      gap: 8px;
    }
    .count-badge {
      font-size: 11.5px;
      background: var(--inner-bg);
      color: #93c5fd;
      padding: 2px 8px;
      border-radius: 999px;
      font-weight: 600;
    }
    .mail-list {
      max-height: 680px;
      overflow-y: auto;
      display: flex;
      flex-direction: column;
    }
    .mail-item {
      padding: 14px 16px;
      border-bottom: 1px solid rgba(255, 255, 255, 0.05);
      cursor: pointer;
      transition: all 0.15s ease;
      display: flex;
      flex-direction: column;
      gap: 6px;
      position: relative;
    }
    .mail-item:last-child { border-bottom: none; }
    .mail-item:hover {
      background: rgba(30, 41, 59, 0.45);
    }
    .mail-item.active {
      background: rgba(37, 99, 235, 0.15);
      border-left: 3px solid #38bdf8;
    }
    .mail-item-top {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 8px;
    }
    .sender-box {
      display: flex;
      align-items: center;
      gap: 8px;
      min-width: 0;
    }
    .mini-avatar {
      width: 24px;
      height: 24px;
      border-radius: 50%;
      background: linear-gradient(135deg, #3b82f6, #8b5cf6);
      color: #fff;
      font-size: 11px;
      font-weight: 700;
      display: flex;
      align-items: center;
      justify-content: center;
      flex-shrink: 0;
    }
    .mini-sender {
      font-size: 13px;
      font-weight: 600;
      color: #f1f5f9;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }
    .mini-time {
      font-size: 11.5px;
      color: var(--text-sub);
      white-space: nowrap;
    }
    .mail-item-subject {
      font-size: 13.5px;
      font-weight: 600;
      color: #e2e8f0;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }
    .mail-item.active .mail-item-subject { color: #fff; }
    .mail-item-bottom {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 8px;
    }
    .item-otp-pill {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      background: rgba(56, 189, 248, 0.15);
      border: 1px solid rgba(56, 189, 248, 0.35);
      color: #38bdf8;
      font-family: "JetBrains Mono", monospace;
      font-weight: 700;
      font-size: 12px;
      padding: 2px 8px;
      border-radius: 6px;
    }
    .item-otp-pill .quick-copy {
      font-size: 10.5px;
      color: #93c5fd;
      cursor: pointer;
      text-decoration: underline;
      margin-left: 2px;
    }
    .item-preview {
      font-size: 12px;
      color: var(--text-sub);
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
      flex: 1;
    }

    /* Right Column: Detail Reader */
    .detail-card {
      background: var(--card-bg);
      border: 1px solid var(--card-border);
      border-radius: var(--radius);
      box-shadow: 0 10px 30px -5px rgba(0, 0, 0, 0.4);
      backdrop-filter: blur(16px);
      overflow: hidden;
      display: flex;
      flex-direction: column;
    }
    .detail-header {
      padding: 20px 24px 16px;
      border-bottom: 1px solid var(--card-border);
      display: flex;
      flex-direction: column;
      gap: 12px;
    }
    .detail-subject {
      font-size: 19px;
      font-weight: 700;
      color: #ffffff;
      line-height: 1.35;
      word-break: break-word;
    }
    .detail-meta-row {
      display: flex;
      align-items: center;
      justify-content: space-between;
      flex-wrap: wrap;
      gap: 10px;
    }
    .sender-details-group {
      display: flex;
      align-items: center;
      gap: 10px;
    }
    .detail-avatar {
      width: 36px;
      height: 36px;
      border-radius: 50%;
      background: linear-gradient(135deg, #3b82f6, #8b5cf6);
      color: #fff;
      font-size: 15px;
      font-weight: 700;
      display: flex;
      align-items: center;
      justify-content: center;
      flex-shrink: 0;
    }
    .sender-meta {
      display: flex;
      flex-direction: column;
      gap: 2px;
    }
    .sender-fullname { font-size: 14px; font-weight: 600; color: #f1f5f9; }
    .sender-address { font-size: 12px; color: var(--text-muted); word-break: break-all; }
    .detail-date-badge {
      font-size: 12px;
      color: var(--text-muted);
      background: var(--inner-bg);
      border: 1px solid var(--card-border);
      padding: 4px 10px;
      border-radius: 6px;
      display: inline-flex;
      align-items: center;
      gap: 6px;
    }

    /* Detail Hero OTP Strip */
    .detail-otp-box {
      margin: 14px 24px 0;
      background: linear-gradient(135deg, rgba(37, 99, 235, 0.18), rgba(16, 185, 129, 0.14));
      border: 1px solid rgba(56, 189, 248, 0.35);
      border-radius: 10px;
      padding: 12px 18px;
      display: flex;
      align-items: center;
      justify-content: space-between;
      flex-wrap: wrap;
      gap: 12px;
    }
    .detail-otp-left {
      display: flex;
      align-items: center;
      gap: 12px;
    }
    .detail-otp-label {
      font-size: 12px;
      color: #7dd3fc;
      font-weight: 700;
      text-transform: uppercase;
    }
    .detail-otp-code {
      font-family: "JetBrains Mono", monospace;
      font-size: 26px;
      font-weight: 800;
      letter-spacing: 4px;
      color: #38bdf8;
      cursor: pointer;
    }

    /* Body Card Toolbar & Tabs */
    .mail-toolbar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 10px 24px;
      background: rgba(15, 23, 42, 0.45);
      border-bottom: 1px solid var(--card-border);
      flex-wrap: wrap;
      gap: 10px;
      margin-top: 14px;
    }
    .tabs {
      display: flex;
      align-items: center;
      gap: 6px;
      background: rgba(2, 6, 23, 0.4);
      padding: 3px;
      border-radius: 8px;
      border: 1px solid var(--card-border);
    }
    .tab-btn {
      background: transparent;
      border: none;
      color: var(--text-muted);
      font-size: 12px;
      font-weight: 600;
      padding: 4px 12px;
      border-radius: 6px;
      cursor: pointer;
      transition: all 0.15s ease;
    }
    .tab-btn:hover { color: #fff; }
    .tab-btn.active {
      background: rgba(59, 130, 246, 0.25);
      color: #60a5fa;
      border: 1px solid rgba(59, 130, 246, 0.3);
    }

    /* Body Views */
    .body-wrapper {
      position: relative;
      min-height: 240px;
      background: rgba(2, 6, 23, 0.25);
    }
    .body-view {
      display: none;
      width: 100%;
    }
    .body-view.active { display: block; }

    /* Formatted / Clean Text View */
    .styled-text-content {
      padding: 24px;
      color: #e2e8f0;
      font-size: 15px;
      line-height: 1.85;
      white-space: pre-wrap;
      word-break: break-word;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "PingFang SC", sans-serif;
    }
    .styled-text-content .link-badge {
      display: inline-flex;
      align-items: center;
      gap: 4px;
      background: rgba(37, 99, 235, 0.15);
      border: 1px solid rgba(59, 130, 246, 0.35);
      color: #93c5fd;
      padding: 2px 8px;
      border-radius: 6px;
      text-decoration: none;
      font-size: 13px;
      font-weight: 500;
      margin: 0 2px;
      transition: all 0.15s ease;
    }
    .styled-text-content .link-badge:hover {
      background: rgba(37, 99, 235, 0.3);
      color: #fff;
      border-color: #3b82f6;
    }
    .styled-text-content .code-highlight {
      background: rgba(56, 189, 248, 0.18);
      border: 1px solid rgba(56, 189, 248, 0.4);
      color: #38bdf8;
      font-family: monospace;
      font-weight: 700;
      padding: 2px 6px;
      border-radius: 4px;
      letter-spacing: 1px;
    }

    /* Iframe HTML View */
    .iframe-container {
      width: 100%;
      background: #ffffff;
      overflow: hidden;
    }
    iframe.body-frame {
      width: 100%;
      min-height: 380px;
      border: 0;
      display: block;
      background: #ffffff;
    }

    /* Raw Text View */
    .raw-text-content {
      padding: 20px 24px;
      color: #94a3b8;
      font-family: "JetBrains Mono", ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
      font-size: 13px;
      line-height: 1.6;
      white-space: pre-wrap;
      word-break: break-all;
      background: #020617;
      overflow-x: auto;
    }

    /* Empty State */
    .empty-card {
      background: var(--card-bg);
      border: 1px dashed rgba(255, 255, 255, 0.12);
      border-radius: var(--radius);
      padding: 60px 24px;
      text-align: center;
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      gap: 16px;
      backdrop-filter: blur(12px);
    }
    .radar-box {
      width: 72px;
      height: 72px;
      border-radius: 50%;
      background: rgba(37, 99, 235, 0.1);
      border: 1px solid rgba(59, 130, 246, 0.3);
      display: flex;
      align-items: center;
      justify-content: center;
      position: relative;
      margin-bottom: 6px;
    }
    .radar-box svg { width: 32px; height: 32px; fill: #38bdf8; }
    .radar-ring {
      position: absolute;
      inset: -8px;
      border-radius: 50%;
      border: 1px solid rgba(59, 130, 246, 0.25);
      animation: radar-expand 2s infinite ease-out;
    }
    @keyframes radar-expand {
      0% { transform: scale(0.85); opacity: 1; }
      100% { transform: scale(1.4); opacity: 0; }
    }
    .empty-title { font-size: 18px; font-weight: 700; color: #fff; }
    .empty-desc { font-size: 14px; color: var(--text-muted); max-width: 440px; line-height: 1.6; }
    .empty-progress {
      width: 200px;
      height: 4px;
      background: rgba(255, 255, 255, 0.08);
      border-radius: 2px;
      overflow: hidden;
      margin-top: 4px;
    }
    .empty-progress-bar {
      height: 100%;
      background: linear-gradient(90deg, #3b82f6, #38bdf8);
      width: 100%;
      transition: width 1s linear;
    }

    /* Floating Toast */
    .toast {
      position: fixed;
      top: 24px;
      left: 50%;
      transform: translateX(-50%) translateY(-100px);
      background: rgba(15, 23, 42, 0.95);
      border: 1px solid rgba(56, 189, 248, 0.4);
      color: #f8fafc;
      padding: 10px 22px;
      border-radius: 999px;
      font-size: 13.5px;
      font-weight: 600;
      display: flex;
      align-items: center;
      gap: 8px;
      box-shadow: 0 10px 30px rgba(0, 0, 0, 0.5), 0 0 20px rgba(56, 189, 248, 0.2);
      z-index: 9999;
      opacity: 0;
      transition: all 0.25s cubic-bezier(0.16, 1, 0.3, 1);
      pointer-events: none;
    }
    .toast.show {
      transform: translateX(-50%) translateY(0);
      opacity: 1;
    }

    .footer {
      text-align: center;
      font-size: 12px;
      color: var(--text-sub);
      margin-top: 8px;
    }

    @media (max-width: 900px) {
      .main-grid {
        grid-template-columns: 1fr;
      }
      .mail-list {
        max-height: 280px;
      }
    }
    @media (max-width: 640px) {
      body { padding: 12px 10px 32px; }
      .otp-code-highlight { font-size: 28px; letter-spacing: 3px; }
      .detail-header { padding: 16px; }
      .styled-text-content { padding: 16px; font-size: 14px; }
    }
  </style>
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
        <div class="refresh-pill" id="refreshPill" onclick="toggleAutoRefresh()" title="点击暂停/继续自动检测">
          ⏱️ <span id="countdownSec">3s</span> 自动检测
        </div>
        <button class="btn btn-secondary" onclick="doRefresh()" title="按 R 键立即刷新">
          <svg id="refreshSpin" viewBox="0 0 24 24"><path d="M17.65 6.35C16.2 4.9 14.21 4 12 4c-4.42 0-7.99 3.58-7.99 8s3.57 8 7.99 8c3.73 0 6.84-2.55 7.73-6h-2.08c-.82 2.33-3.04 4-5.65 4-3.31 0-6-2.69-6-6s2.69-6 6-6c1.66 0 3.14.69 4.22 1.78L13 11h7V4l-2.35 2.35z"/></svg>
          <span>刷新</span>
        </button>
      </div>
    </header>

    <!-- Alias Banner -->
    <div class="alias-banner">
      <div class="alias-info">
        <span class="alias-label">目标别名:</span>
        <span class="alias-chip" onclick="copyText('{{.Email}}', '已复制别名邮箱')" title="点击复制邮箱">
          <span>{{.Email}}</span>
          <svg viewBox="0 0 24 24"><path d="M16 1H4c-1.1 0-2 .9-2 2v14h2V3h12V1zm3 4H8c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h11c1.1 0 2-.9 2-2V7c0-1.1-.9-2-2-2zm0 16H8V7h11v14z"/></svg>
        </span>
        {{if .AccountName}}
        <span class="alias-meta">· 母号: {{.AccountName}}</span>
        {{end}}
      </div>
      <div>
        <button class="btn btn-secondary" style="padding: 5px 12px; font-size: 12px;" onclick="copyCurrentUrl()">
          <svg viewBox="0 0 24 24"><path d="M3.9 12c0-1.71 1.39-3.1 3.1-3.1h4V7H7c-2.76 0-5 2.24-5 5s2.24 5 5 5h4v-1.9H7c-1.71 0-3.1-1.39-3.1-3.1zM8 13h8v-2H8v2zm9-6h-4v1.9h4c1.71 0 3.1 1.39 3.1 3.1s-1.39 3.1-3.1 3.1h-4V17h4c2.76 0 5-2.24 5-5s-2.24-5-5-5z"/></svg>
          <span>复制查信链接</span>
        </button>
      </div>
    </div>

    <!-- MASTER OTP SUMMARY BAR (All extracted codes prominently displayed) -->
    <section class="otp-master-bar" id="otpMasterBar" style="{{if not .AllOTPs}}display: none;{{end}}">
      <div class="otp-latest-row">
        <div class="otp-latest-left">
          <div class="otp-latest-tag">
            <svg viewBox="0 0 24 24"><path d="M19 9l1.25-2.75L23 5l-2.75-1.25L19 1l-1.25 2.75L15 5l2.75 1.25L19 9zm-7.5.5L9 4 6.5 9.5 1 12l5.5 2.5L9 20l2.5-5.5L17 12l-5.5-2.5zM19 15l-1.25 2.75L15 19l2.75 1.25L19 23l1.25-2.75L23 19l-2.75-1.25L19 15z"/></svg>
            <span>提取到验证码</span>
          </div>
          <div class="otp-code-highlight" id="topLatestOtp" onclick="copyCode(this.textContent.trim())" title="点击复制最新验证码">
            {{if .AllOTPs}}{{(index .AllOTPs 0).Code}}{{end}}
          </div>
        </div>
        <div style="display: flex; align-items: center; gap: 8px; flex-wrap: wrap;">
          <button class="btn btn-primary" id="topLatestCopyBtn" onclick="copyCode(document.getElementById('topLatestOtp').textContent.trim())">
            <svg viewBox="0 0 24 24"><path d="M16 1H4c-1.1 0-2 .9-2 2v14h2V3h12V1zm3 4H8c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h11c1.1 0 2-.9 2-2V7c0-1.1-.9-2-2-2zm0 16H8V7h11v14z"/></svg>
            <span>复制验证码</span>
          </button>
          <a class="btn btn-magic" id="topMagicBtn" href="{{if .AllOTPs}}{{(index .AllOTPs 0).MagicLink}}{{end}}" target="_blank" rel="noopener noreferrer" style="{{if or (not .AllOTPs) (not (index .AllOTPs 0).MagicLink)}}display: none;{{end}}">
            <svg viewBox="0 0 24 24"><path d="M19 19H5V5h7V3H5c-1.11 0-2 .9-2 2v14c0 1.1.89 2 2 2h14c1.1 0 2-.9 2-2v-7h-2v7zM14 3v2h3.59l-9.83 9.83 1.41 1.41L19 6.41V10h2V3h-7z"/></svg>
            <span>打开链接 ↗</span>
          </a>
        </div>
      </div>

      <!-- Historical OTPs Chips Row -->
      <div class="otp-history-section" id="otpHistorySection" style="{{if le (len .AllOTPs) 1}}display: none;{{end}}">
        <span class="otp-history-label">全部提取验证码记录 ({{len .AllOTPs}} 条):</span>
        {{range .AllOTPs}}
        <button class="otp-chip-btn" onclick="copyCode('{{.Code}}')" title="点击复制此验证码 (来自: {{.SenderName}} · {{.RelativeDate}})">
          <span class="otp-chip-code">{{.Code}}</span>
          <span>· {{.SenderName}} ({{.RelativeDate}})</span>
        </button>
        {{end}}
      </div>
    </section>

    <!-- TWO-COLUMN MASTER-DETAIL LAYOUT -->
    <div class="main-grid" id="mainGrid" style="{{if not .HasMail}}display: none;{{end}}">
      <!-- Left Column: Mail List -->
      <aside class="inbox-card">
        <div class="inbox-header">
          <div class="inbox-title">
            <svg style="width:16px;height:16px;fill:currentColor" viewBox="0 0 24 24"><path d="M20 4H4c-1.1 0-1.99.9-1.99 2L2 18c0 1.1.9 2 2 2h16c1.1 0 2-.9 2-2V6c0-1.1-.9-2-2-2zm0 4l-8 5-8-5V6l8 5 8-5v2z"/></svg>
            <span>收件列表</span>
            <span class="count-badge" id="inboxCountBadge">共 {{.TotalCount}} 封</span>
          </div>
        </div>
        <div class="mail-list" id="mailListContainer">
          {{range .Items}}
          <div class="mail-item {{if eq .Index 0}}active{{end}}" id="mailItem-{{.Index}}" onclick="selectMail({{.Index}})">
            <div class="mail-item-top">
              <div class="sender-box">
                <div class="mini-avatar">{{.SenderInitial}}</div>
                <span class="mini-sender">{{.SenderName}}</span>
              </div>
              <span class="mini-time">{{.RelativeDate}}</span>
            </div>
            <div class="mail-item-subject">{{.Subject}}</div>
            <div class="mail-item-bottom">
              {{if .HasOTP}}
              <div class="item-otp-pill">
                <span>⚡ {{.Code}}</span>
                <span class="quick-copy" onclick="event.stopPropagation(); copyCode('{{.Code}}')" title="复制验证码">复制</span>
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
              <svg style="width:14px;height:14px;fill:currentColor" viewBox="0 0 24 24"><path d="M11.99 2C6.47 2 2 6.48 2 12s4.47 10 9.99 10C17.52 22 22 17.52 22 12S17.52 2 11.99 2zM12 20c-4.42 0-8-3.58-8-8s3.58-8 8-8 8 3.58 8 8-3.58 8-8 8zm.5-13H11v6l5.25 3.15.75-1.23-4.5-2.67z"/></svg>
              <span id="detailDate">{{.LatestItem.FormattedDate}}</span>
            </div>
          </div>
        </div>

        <!-- Detail OTP Strip (Visible if current email has OTP) -->
        <div class="detail-otp-box" id="detailOtpBox" style="{{if not .LatestItem.HasOTP}}display: none;{{end}}">
          <div class="detail-otp-left">
            <span class="detail-otp-label">本信验证码:</span>
            <span class="detail-otp-code" id="detailOtpCode" onclick="copyCode(this.textContent)">{{.LatestItem.Code}}</span>
          </div>
          <div style="display: flex; gap: 8px;">
            <button class="btn btn-primary" style="padding: 5px 12px; font-size: 12px;" onclick="copyCode(document.getElementById('detailOtpCode').textContent)">
              <svg viewBox="0 0 24 24"><path d="M16 1H4c-1.1 0-2 .9-2 2v14h2V3h12V1zm3 4H8c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h11c1.1 0 2-.9 2-2V7c0-1.1-.9-2-2-2zm0 16H8V7h11v14z"/></svg>
              <span>复制</span>
            </button>
            <a class="btn btn-magic" id="detailMagicBtn" href="{{.LatestItem.MagicLink}}" target="_blank" rel="noopener noreferrer" style="padding: 5px 12px; font-size: 12px; {{if not .LatestItem.MagicLink}}display: none;{{end}}">
              <span>打开激活链接 ↗</span>
            </a>
          </div>
        </div>

        <!-- Toolbar & Tabs -->
        <div class="mail-toolbar">
          <div class="tabs">
            <button class="tab-btn active" id="tab-styled" onclick="switchTab('styled')">💬 优雅排版</button>
            <button class="tab-btn" id="tab-html" onclick="switchTab('html')">🌐 网页视图</button>
            <button class="tab-btn" id="tab-raw" onclick="switchTab('raw')">📄 原始文本</button>
          </div>
          <div>
            <button class="btn btn-secondary" style="padding: 5px 12px; font-size: 12px;" onclick="copyFullBody()">
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
              <iframe id="mailFrame" class="body-frame" sandbox="allow-same-origin allow-popups" onload="resizeIframe(this)"></iframe>
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
    <div class="empty-card" id="emptyCard" style="{{if .HasMail}}display: none;{{end}}">
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
      <button class="btn btn-primary" onclick="checkNewMailsSilently(true)" style="margin-top: 8px;">
        <span>立即检查新邮件</span>
      </button>
    </div>

    <footer class="footer">
      iCloud 隐私邮箱轻量查信平台 · 更新于 {{.RefreshTime}}
    </footer>
  </div>

  <!-- Floating Toast Notification -->
  <div class="toast" id="toast"></div>

  <script>
    let emailList = {{.ItemsJSON}};
    let currentIdx = 0;
    const activeSec = 3;
    const bgSec = 10;
    let pollIntervalSec = activeSec;
    let remaining = pollIntervalSec;
    let totalSec = pollIntervalSec;
    let autoRefreshPaused = false;
    let isChecking = false;

    function showToast(msg) {
      const el = document.getElementById('toast');
      if (!el) return;
      el.textContent = msg;
      el.classList.add('show');
      clearTimeout(window.__toastTimer);
      window.__toastTimer = setTimeout(function() {
        el.classList.remove('show');
      }, 2500);
    }

    function copyText(str, msg) {
      if (!str) return;
      if (!navigator.clipboard) {
        const ta = document.createElement('textarea');
        ta.value = str;
        document.body.appendChild(ta);
        ta.select();
        document.execCommand('copy');
        document.body.removeChild(ta);
        showToast(msg || '已复制');
        return;
      }
      navigator.clipboard.writeText(str).then(function() {
        showToast(msg || '已复制');
      });
    }

    function copyCode(code) {
      if (!code) return;
      copyText(code, '✨ 验证码 ' + code + ' 已复制到剪贴板');
    }

    function copyCurrentUrl() {
      copyText(window.location.href, '查信链接已复制');
    }

    function copyFullBody() {
      const item = emailList[currentIdx];
      if (item) {
        copyText(item.text_body || item.html_body, '邮件全文已复制');
      }
    }

    function resizeIframe(obj) {
      try {
        if (obj.contentWindow && obj.contentWindow.document) {
          const doc = obj.contentWindow.document;
          const h = Math.max(doc.body.scrollHeight, doc.documentElement.scrollHeight);
          if (h > 150) {
            obj.style.height = (h + 30) + 'px';
          }
        }
      } catch(e) {}
    }

    function switchTab(viewId) {
      document.querySelectorAll('.tab-btn').forEach(function(b) { b.classList.remove('active'); });
      document.querySelectorAll('.body-view').forEach(function(v) { v.classList.remove('active'); });
      const btn = document.getElementById('tab-' + viewId);
      const view = document.getElementById('view-' + viewId);
      if (btn) btn.classList.add('active');
      if (view) view.classList.add('active');
    }

    function selectMail(index) {
      if (!emailList || emailList.length === 0) return;
      if (index < 0 || index >= emailList.length) index = 0;
      currentIdx = index;

      document.querySelectorAll('.mail-item').forEach(function(el) { el.classList.remove('active'); });
      const activeEl = document.getElementById('mailItem-' + index);
      if (activeEl) activeEl.classList.add('active');

      const item = emailList[index];
      if (!item) return;

      const subEl = document.getElementById('detailSubject');
      if (subEl) subEl.textContent = item.subject || '（无主题）';
      const avEl = document.getElementById('detailAvatar');
      if (avEl) avEl.textContent = item.sender_initial || 'M';
      const sNameEl = document.getElementById('detailSenderName');
      if (sNameEl) sNameEl.textContent = item.sender_name || item.from;
      const sMailEl = document.getElementById('detailSenderEmail');
      if (sMailEl) sMailEl.textContent = item.sender_email || item.from;
      const dDateEl = document.getElementById('detailDate');
      if (dDateEl) dDateEl.textContent = item.formatted_date || item.date;

      // Update OTP Box
      const otpBox = document.getElementById('detailOtpBox');
      const otpCode = document.getElementById('detailOtpCode');
      const magicBtn = document.getElementById('detailMagicBtn');
      if (item.has_otp && item.code) {
        if (otpBox) otpBox.style.display = 'flex';
        if (otpCode) otpCode.textContent = item.code;
        if (magicBtn) {
          if (item.magic_link) {
            magicBtn.style.display = 'inline-flex';
            magicBtn.href = item.magic_link;
          } else {
            magicBtn.style.display = 'none';
          }
        }
      } else if (otpBox) {
        otpBox.style.display = 'none';
      }

      // Update raw text
      const rawArea = document.getElementById('rawContentArea');
      if (rawArea) rawArea.textContent = item.text_body || item.html_body || '';

      // Update iframe
      const frame = document.getElementById('mailFrame');
      if (frame) {
        if (item.is_html) {
          frame.srcdoc = item.html_body;
        } else {
          frame.srcdoc = '<!DOCTYPE html><html><body style="font-family: sans-serif; white-space: pre-wrap; padding: 20px; line-height: 1.6; color: #1e293b;">' +
            escapeHtml(item.text_body) + '</body></html>';
        }
      }

      // Update styled plain text view
      renderStyledText(item);

      if (item.is_html) {
        switchTab('html');
      } else {
        switchTab('styled');
      }
    }

    function escapeHtml(raw) {
      if (!raw) return '';
      return raw.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
    }

    function renderStyledText(item) {
      const target = document.getElementById('styledContentArea');
      if (!target || !item) return;
      const raw = item.text_body || '';
      let escaped = escapeHtml(raw);

      // Turn URLs into clean link badges
      escaped = escaped.replace(/(https?:\/\/[^\s<)]+)/g, function(url) {
        try {
          const u = new URL(url);
          const host = u.hostname.replace(/^www\./, '');
          return '<a href="' + url + '" target="_blank" rel="noopener noreferrer" class="link-badge" title="' + url + '">🔗 ' + host + '</a>';
        } catch(e) {
          return '<a href="' + url + '" target="_blank" rel="noopener noreferrer" class="link-badge">🔗 打开链接</a>';
        }
      });

      // Highlight OTP Code if present
      if (item.has_otp && item.code) {
        const reg = new RegExp('(\\b' + item.code + '\\b)', 'g');
        escaped = escaped.replace(reg, '<span class="code-highlight">$1</span>');
      }

      target.innerHTML = escaped;
    }

    // SILENT LIVE BACKGROUND POLLING
    async function checkNewMailsSilently(isManual) {
      if (isChecking) {
        if (isManual) showToast('正在同步中，请稍候...');
        return;
      }
      isChecking = true;

      const secEl = document.getElementById('countdownSec');
      if (secEl) secEl.textContent = '检测中...';
      const spin = document.getElementById('refreshSpin');
      if (spin) spin.style.animation = 'spin 0.6s linear infinite';

      try {
        const u = new URL(window.location.href);
        u.searchParams.set('format', 'json');
        u.searchParams.set('_t', Date.now().toString());

        const res = await fetch(u.toString(), {
          headers: { 'Accept': 'application/json' },
          cache: 'no-store'
        });

        if (res.ok) {
          const resp = await res.json();
          if (resp && resp.success && resp.data) {
            applyNewData(resp.data, isManual);
          }
        } else if (res.status === 401 || res.status === 403) {
          autoRefreshPaused = true;
          const pill = document.getElementById('refreshPill');
          if (pill) pill.innerHTML = '⚠️ 权限已失效';
          showToast('身份验证失效，已暂停自动检测');
        }
      } catch(err) {
        console.warn('Silent poll error:', err);
      } finally {
        isChecking = false;
        if (spin) spin.style.animation = '';
        remaining = totalSec;
        if (secEl && !autoRefreshPaused) secEl.textContent = remaining + 's';
      }
    }

    function applyNewData(data, isManual) {
      if (!data) return;

      const oldFirstID = (emailList && emailList.length > 0) ? emailList[0].id : '';
      const newFirstID = (data.items && data.items.length > 0) ? data.items[0].id : '';
      const oldLatestOtp = (emailList && emailList.find(function(x) { return x.has_otp; })) ? emailList.find(function(x) { return x.has_otp; }).code : '';
      const newLatestOtp = (data.all_otps && data.all_otps.length > 0) ? data.all_otps[0].code : '';

      const listCountChanged = Boolean((emailList ? emailList.length : 0) !== (data.items ? data.items.length : 0));
      const hasNewMail = Boolean(newFirstID && newFirstID !== oldFirstID);
      const hasNewOTP = Boolean(newLatestOtp && newLatestOtp !== oldLatestOtp);

      emailList = data.items || [];

      // Update Top Master OTP Bar (Extract latest OTP from AllOTPs)
      const masterOtpBar = document.getElementById('otpMasterBar');
      const latestOtpItem = (data.all_otps && data.all_otps.length > 0) ? data.all_otps[0] : null;

      if (latestOtpItem) {
        if (masterOtpBar) masterOtpBar.style.display = 'flex';
        const topLatestOtp = document.getElementById('topLatestOtp');
        if (topLatestOtp) topLatestOtp.textContent = latestOtpItem.code;

        const topMagicBtn = document.getElementById('topMagicBtn');
        if (topMagicBtn) {
          if (latestOtpItem.magic_link) {
            topMagicBtn.style.display = 'inline-flex';
            topMagicBtn.href = latestOtpItem.magic_link;
          } else {
            topMagicBtn.style.display = 'none';
          }
        }

        if (latestOtpItem && latestOtpItem.code) {
          if (fav) {
            fav.href = "data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'><circle cx='16' cy='16' r='14' fill='%2310b981'/><path fill='%23ffffff' d='M14 20.5l-4.5-4.5 1.4-1.4 3.1 3.1 7.6-7.6 1.4 1.4z'/></svg>";
          }
        }

        const historySec = document.getElementById('otpHistorySection');
        if (historySec) {
          if (data.all_otps.length > 1) {
            historySec.style.display = 'flex';
            let chipsHTML = '<span class="otp-history-label">全部提取验证码记录 (' + data.all_otps.length + ' 条):</span>';
            data.all_otps.forEach(function(it) {
              chipsHTML += '<button class="otp-chip-btn" onclick="copyCode(\'' + it.code + '\')">' +
                '<span class="otp-chip-code">' + it.code + '</span>' +
                '<span>· ' + escapeHtml(it.sender_name) + ' (' + it.relative_date + ')</span>' +
                '</button>';
            });
            historySec.innerHTML = chipsHTML;
          } else {
            historySec.style.display = 'none';
          }
        }
      } else if (masterOtpBar) {
        masterOtpBar.style.display = 'none';
        const fav = document.getElementById('pageFavicon');
        if (fav) {
          fav.href = "data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'><path fill='%232563eb' d='M26 15a7 7 0 0 0-13.4-2.8A5 5 0 0 0 4 17a5 5 0 0 0 5 5h17a5 5 0 0 0 0-10z'/><path fill='%2338bdf8' d='M18 10l-4 6h3v5l5-7h-4z'/></svg>";
        }
      }

      // Update document title (Clean standard mailbox format: 收件箱 (N) · email)
      if (data.has_mail && (data.total_count > 0 || (emailList && emailList.length > 0))) {
        const count = data.total_count || (emailList ? emailList.length : 0);
        document.title = '收件箱 (' + count + ') · ' + (data.email || '');
      } else {
        document.title = '收件箱 · ' + (data.email || '');
      }

      // Update status badge
      const statusBadge = document.querySelector('.status-badge');
      if (statusBadge) {
        if (data.has_mail) {
          statusBadge.className = 'status-badge live';
          statusBadge.innerHTML = '<span class="pulse-dot"></span> 实时就绪';
        } else {
          statusBadge.className = 'status-badge idle';
          statusBadge.innerHTML = '<span class="pulse-dot"></span> 实时监听中';
        }
      }

      // Transition between empty state and mail grid
      const emptyCard = document.getElementById('emptyCard');
      const mainGrid = document.getElementById('mainGrid');
      if (data.has_mail) {
        if (emptyCard) emptyCard.style.display = 'none';
        if (mainGrid) mainGrid.style.display = 'grid';
      } else {
        if (emptyCard) emptyCard.style.display = 'flex';
        if (mainGrid) mainGrid.style.display = 'none';
      }

      // Update Left List Badge
      const countBadge = document.getElementById('inboxCountBadge');
      if (countBadge) countBadge.textContent = '共 ' + (data.total_count || 0) + ' 封';

      // Re-render email list only if new mail arrived or count changed
      if (hasNewMail || hasNewOTP || listCountChanged) {
        const listContainer = document.getElementById('mailListContainer');
        if (listContainer && emailList.length > 0) {
          let listHTML = '';
          emailList.forEach(function(it) {
            const activeCls = (it.index === 0) ? ' active' : '';
            let otpBadge = '<div class="item-preview">' + escapeHtml(it.subject) + '</div>';
            if (it.has_otp) {
              otpBadge = '<div class="item-otp-pill"><span>⚡ ' + it.code + '</span>' +
                '<span class="quick-copy" onclick="event.stopPropagation(); copyCode(\'' + it.code + '\')">复制</span></div>';
            }
            listHTML += '<div class="mail-item' + activeCls + '" id="mailItem-' + it.index + '" onclick="selectMail(' + it.index + ')">' +
              '<div class="mail-item-top">' +
                '<div class="sender-box">' +
                  '<div class="mini-avatar">' + (it.sender_initial || 'M') + '</div>' +
                  '<span class="mini-sender">' + escapeHtml(it.sender_name) + '</span>' +
                '</div>' +
                '<span class="mini-time">' + it.relative_date + '</span>' +
              '</div>' +
              '<div class="mail-item-subject">' + escapeHtml(it.subject) + '</div>' +
              '<div class="mail-item-bottom">' + otpBadge + '</div>' +
            '</div>';
          });
          listContainer.innerHTML = listHTML;
        }

        // New mail arrived! Select top mail and toast
        if (newLatestOtp) {
          showToast('🎉 收到新邮件！最新验证码: ' + newLatestOtp);
        } else {
          showToast('🎉 收到新邮件！收件列表已更新');
        }
        selectMail(0);
      } else {
        // No new mail arrived: keep user reading state intact, give feedback if manual check
        if (isManual) {
          showToast('已完成检查，暂无新邮件');
        }
      }
    }

    function toggleAutoRefresh() {
      autoRefreshPaused = !autoRefreshPaused;
      const pill = document.getElementById('refreshPill');
      if (pill) {
        pill.innerHTML = autoRefreshPaused ? '⏸️ 自动检测已暂停' : '⏱️ <span id="countdownSec">' + Math.max(0, remaining) + 's</span> 自动检测';
      }
      showToast(autoRefreshPaused ? '自动检测已暂停' : '自动检测已开启');
    }

    function doRefresh() {
      checkNewMailsSilently(true);
    }

    setInterval(function() {
      if (autoRefreshPaused || isChecking) return;
      remaining--;
      if (remaining <= 0) {
        remaining = totalSec;
        checkNewMailsSilently(false);
      }
      const secEl = document.getElementById('countdownSec');
      if (secEl) secEl.textContent = remaining + 's';
      const prog = document.getElementById('emptyProgressBar');
      if (prog) {
        const pct = Math.max(0, (remaining / totalSec) * 100);
        prog.style.width = pct + '%';
      }
    }, 1000);

    // Initial render
    if (emailList && emailList.length > 0) {
      selectMail(0);
    }

    // Clean URL address bar: remove any temporary _t or format query params
    try {
      if (window.history && window.history.replaceState) {
        const u = new URL(window.location.href);
        if (u.searchParams.has('_t') || u.searchParams.has('format')) {
          u.searchParams.delete('_t');
          u.searchParams.delete('format');
          window.history.replaceState(null, '', u.pathname + (u.search ? u.search : ''));
        }
      }
    } catch(e) {}

    // Auto check when tab becomes active again and adapt polling frequency
    document.addEventListener('visibilitychange', function() {
      if (!document.hidden) {
        pollIntervalSec = activeSec;
        totalSec = activeSec;
        remaining = 0;
        checkNewMailsSilently(false);
      } else {
        pollIntervalSec = bgSec;
        totalSec = bgSec;
      }
    });

    // Keyboard shortcuts
    document.addEventListener('keydown', function(e) {
      if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA') return;
      if (e.ctrlKey || e.metaKey || e.altKey) return;
      if (e.key === 'c' || e.key === 'C') {
        const item = emailList[currentIdx];
        if (item && item.has_otp) copyCode(item.code);
      } else if (e.key === 'r' || e.key === 'R') {
        doRefresh();
      } else if (e.key === 'ArrowDown') {
        if (currentIdx < emailList.length - 1) selectMail(currentIdx + 1);
      } else if (e.key === 'ArrowUp') {
        if (currentIdx > 0) selectMail(currentIdx - 1);
      }
    });
  </script>
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
	if p.Kind == auth.PrincipalToken && !p.IsAdmin() && s.store != nil {
		if !s.store.IsEmailOwnedByToken(c.Request.Context(), email, p.ID) {
			failCode(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "未找到该别名或无权访问")
			return
		}
	}

	if s.syncWorker != nil {
		s.syncWorker.Trigger()
	}

	data := mailViewData{
		Email:       email,
		RefreshTime: time.Now().Format("15:04:05"),
		Items:       []mailViewItem{},
		AllOTPs:     []mailViewItem{},
		ItemsJSON:   template.JS("[]"),
	}

	accountID := strings.TrimSpace(c.Query("account_id"))
	if accountID == "" {
		accountID = strings.TrimSpace(c.Query("account"))
	}
	if accountID == "" {
		accountID = s.findAccountForEmail(email)
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
				if it.HasOTP && it.Code != "" {
					allOtps = append(allOtps, it)
				}
			}
			data.AllOTPs = allOtps

			b, _ := json.Marshal(items)
			data.ItemsJSON = template.JS(b)
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
	if p.Kind == auth.PrincipalToken && !p.IsAdmin() && s.store != nil {
		if !s.store.IsEmailOwnedByToken(c.Request.Context(), email, p.ID) {
			failCode(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "未找到该别名或无权访问")
			return
		}
	}

	if s.syncWorker != nil {
		s.syncWorker.Trigger()
	}

	accountID := strings.TrimSpace(c.Query("account_id"))
	if accountID == "" {
		accountID = strings.TrimSpace(c.Query("account"))
	}
	if accountID == "" {
		accountID = s.findAccountForEmail(email)
	}
	if accountID == "" {
		c.Data(http.StatusNotFound, "text/plain; charset=utf-8", []byte("NO_ACCOUNT_FOR_ALIAS"))
		return
	}

	items, err := s.fetchRecentMessagesForAlias(c.Request.Context(), accountID, email, 1)
	if err != nil || len(items) == 0 {
		c.Data(http.StatusNotFound, "text/plain; charset=utf-8", []byte("NO_EMAIL_RECEIVED"))
		return
	}

	latest := items[0]
	format := strings.ToLower(strings.TrimSpace(c.Query("format")))
	if format == "html" {
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
