/**
 * [INPUT]: 依赖 net/http, github.com/gin-gonic/gin
 * [OUTPUT]: 对外提供 apiResp, ok, failCode, backendFail
 * [POS]: internal/server 的统一 API 响应格式与稳定错误码映射管道
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

// Package server - 统一响应格式与稳定错误码。
package server

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

// apiResp 是统一 API 响应。
type apiResp struct {
	Success bool   `json:"success"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Data    any    `json:"data,omitempty"`
}

// ok 返回统一成功响应。
func ok(c *gin.Context, data any) {
	c.JSON(http.StatusOK, apiResp{Success: true, Data: data})
}

// failCode 返回统一失败响应。
func failCode(c *gin.Context, status int, code, message string) {
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		if c.Writer.Header().Get("Retry-After") == "" {
			c.Header("Retry-After", "2")
		}
	}
	c.AbortWithStatusJSON(status, apiResp{Success: false, Code: code, Message: message})
}

// failBackendError 返回 Backend 统一失败响应，保留 Data 并将 retry_after 映射为 HTTP 响应头 (S07)。
func failBackendError(c *gin.Context, be *BackendError) {
	if be == nil {
		failCode(c, http.StatusInternalServerError, "INTERNAL_ERROR", "未知错误")
		return
	}
	if be.Data != nil {
		if m, ok := be.Data.(map[string]any); ok {
			if val, exists := m["retry_after"]; exists && val != nil {
				c.Header("Retry-After", fmt.Sprint(val))
			}
		}
	}
	if be.Status == http.StatusTooManyRequests || be.Status == http.StatusServiceUnavailable {
		if c.Writer.Header().Get("Retry-After") == "" {
			c.Header("Retry-After", "2")
		}
	}
	c.AbortWithStatusJSON(be.Status, apiResp{Success: false, Code: be.Code, Message: be.Message, Data: be.Data})
}

// backendFail 把 Backend 错误映射为统一失败响应。
func backendFail(c *gin.Context, err error) {
	be := asBackendError(err)
	failBackendError(c, be)
}
