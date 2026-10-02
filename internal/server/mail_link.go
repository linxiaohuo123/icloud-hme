/**
 * [INPUT]: 依赖 crypto/hmac, crypto/sha256, encoding/base64, net/url, gin, icloud-hme/internal/auth, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 directMailEmail, mailLinkPrincipal, validMailLink, createMailLinkHandler, revokeMailLinksHandler
 * [POS]: internal/server 的单别名只读签名直链：HMAC(email|exp) 绑定单个别名与有效期，仅放行 /mail/code|view|raw；递增 epoch 一键作废全部已发链接
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/auth"
	"icloud-hme/internal/store"
)

const (
	mailLinkEpochSetting = "mail_link_epoch"
	mailLinkMaxDays      = 3650
)

// directMailEmail 统一解析直链目标别名；签名校验与三个直链 handler 必须使用同一解析结果。
func directMailEmail(c *gin.Context) string {
	for _, v := range []string{c.Param("email"), c.Query("email"), c.Query("alias")} {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			return v
		}
	}
	return ""
}

// mailLinkKey 返回当前 epoch 的签名子密钥。进程内缓存，单实例由数据目录锁保证。
func (s *Server) mailLinkKey() ([]byte, error) {
	s.linkKeyMu.Lock()
	defer s.linkKeyMu.Unlock()
	if s.linkKey != nil {
		return s.linkKey, nil
	}
	if s.store == nil || s.store.Cipher() == nil {
		return nil, errors.New("未配置 Master Key，签名直链不可用")
	}
	epoch, err := s.store.GetSetting(mailLinkEpochSetting)
	if err != nil {
		return nil, err
	}
	s.linkKey = s.store.Cipher().DeriveKey("icloud-hme/mail-link/v1/epoch:" + epoch)
	return s.linkKey, nil
}

func mailLinkSig(key []byte, email string, exp int64) string {
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s|%d", email, exp)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// validMailLink 校验 exp/sig 是否为该别名未过期、未作废的签名。
func (s *Server) validMailLink(email, expRaw, sig string) bool {
	exp, err := strconv.ParseInt(expRaw, 10, 64)
	if err != nil || email == "" || sig == "" || time.Now().Unix() > exp {
		return false
	}
	key, err := s.mailLinkKey()
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(mailLinkSig(key, email, exp)))
}

// mailLinkPrincipal 供直链路由的 requireSession 调用：签名有效时返回仅限该别名的 verify 主体。
func (s *Server) mailLinkPrincipal(c *gin.Context) (auth.Principal, bool) {
	email := directMailEmail(c)
	if !s.validMailLink(email, c.Query("exp"), c.Query("sig")) {
		return auth.Principal{}, false
	}
	return auth.Principal{
		Kind: auth.PrincipalLink, ID: email, TokenName: "mail_link",
		Scopes: []string{store.ScopeVerify},
	}, true
}

// createMailLinkHandler 为单个别名签发只读直链 (POST /api/mail-links)。
func (s *Server) createMailLinkHandler(c *gin.Context) {
	var req struct {
		Email string `json:"email"`
		Days  int    `json:"days"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "请求体格式错误")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !strings.Contains(email, "@") {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "email 格式无效")
		return
	}
	if req.Days < 1 || req.Days > mailLinkMaxDays {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", fmt.Sprintf("days 必须在 1-%d 之间", mailLinkMaxDays))
		return
	}
	key, err := s.mailLinkKey()
	if err != nil {
		failCode(c, http.StatusServiceUnavailable, "LINK_UNAVAILABLE", err.Error())
		return
	}
	expiresAt := time.Now().Add(time.Duration(req.Days) * 24 * time.Hour).UTC()
	exp := expiresAt.Unix()
	query := url.Values{
		"email": {email},
		"exp":   {strconv.FormatInt(exp, 10)},
		"sig":   {mailLinkSig(key, email, exp)},
	}.Encode()
	ok(c, gin.H{"email": email, "expires_at": expiresAt.Format(time.RFC3339), "query": query})
}

// revokeMailLinksHandler 递增 epoch，使此前签发的全部直链立即失效 (POST /api/mail-links/revoke)。
// ponytail: 只有全局作废；需要按单个别名作废时再加按别名的 epoch。
func (s *Server) revokeMailLinksHandler(c *gin.Context) {
	if s.store == nil {
		failCode(c, http.StatusServiceUnavailable, "LINK_UNAVAILABLE", "存储不可用")
		return
	}
	s.linkKeyMu.Lock()
	defer s.linkKeyMu.Unlock()
	cur, err := s.store.GetSetting(mailLinkEpochSetting)
	if err != nil {
		backendFail(c, err)
		return
	}
	n, _ := strconv.Atoi(cur)
	if err := s.store.SaveSetting(mailLinkEpochSetting, strconv.Itoa(n+1)); err != nil {
		backendFail(c, err)
		return
	}
	s.linkKey = nil
	ok(c, gin.H{"revoked": true})
}
