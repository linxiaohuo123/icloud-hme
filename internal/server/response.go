/**
 * [INPUT]: 依赖 net/http, github.com/gin-gonic/gin
 * [OUTPUT]: 对外提供 apiResp, ok, failCode, backendFail
 * [POS]: internal/server 的统一 API 响应格式与稳定错误码映射管道
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

// Package server - 统一响应格式与稳定错误码。
package server

import (
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
	c.AbortWithStatusJSON(status, apiResp{Success: false, Code: code, Message: message})
}

// backendFail 把 Backend 错误映射为统一失败响应。
func backendFail(c *gin.Context, err error) {
	be := asBackendError(err)
	failCode(c, be.Status, be.Code, be.Message)
}
