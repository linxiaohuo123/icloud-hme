/**
 * [INPUT]: 依赖 testing, net/http, net/http/httptest, github.com/gin-gonic/gin, bytes
 * [OUTPUT]: 对外提供 TestIsLoopbackHost, TestValidateListenAddress, TestDNSRebindingMiddleware, TestBodyLimitMiddleware
 * [POS]: internal/server 的网络边界防护与安全准入测试套件
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestIsLoopbackHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"localhost:8080", true},
		{"127.0.0.1", true},
		{"127.0.0.1:8081", true},
		{"::1", true},
		{"[::1]:8080", true},
		{"0.0.0.0", false},
		{"0.0.0.0:8080", false},
		{"192.168.1.10", false},
		{"example.com", false},
		{"evil.com:8080", false},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			if got := isLoopbackHost(tt.host); got != tt.want {
				t.Errorf("isLoopbackHost(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

func TestValidateListenAddress(t *testing.T) {
	// 1. 无凭据下测试
	if err := validateListenAddress("127.0.0.1:8080", false); err != nil {
		t.Errorf("回环地址无凭据应允许: %v", err)
	}
	if err := validateListenAddress("0.0.0.0:8080", false); err == nil {
		t.Errorf("0.0.0.0 无凭据应拒绝")
	}
	if err := validateListenAddress(":8080", false); err == nil {
		t.Errorf(":8080 无凭据应拒绝")
	}

	// 2. 有凭据下测试
	if err := validateListenAddress("0.0.0.0:8080", true); err != nil {
		t.Errorf("0.0.0.0 有凭据应允许: %v", err)
	}
	if err := validateListenAddress(":8080", true); err != nil {
		t.Errorf(":8080 有凭据应允许: %v", err)
	}
}

func TestDNSRebindingMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("无凭据拒绝外部非法Host", func(t *testing.T) {
		r := gin.New()
		r.Use(dnsRebindingMiddleware(false))
		r.GET("/test", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

		req := httptest.NewRequest("GET", "http://evil.com/test", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Errorf("期望 403, 实际得到: %d", w.Code)
		}
	})

	t.Run("无凭据拒绝恶意Origin跨域探测", func(t *testing.T) {
		r := gin.New()
		r.Use(dnsRebindingMiddleware(false))
		r.GET("/test", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

		req := httptest.NewRequest("GET", "http://localhost:8080/test", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Origin", "http://attacker.com")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Errorf("期望 403, 实际得到: %d", w.Code)
		}
	})

	t.Run("无凭据放行合法回环请求", func(t *testing.T) {
		r := gin.New()
		r.Use(dnsRebindingMiddleware(false))
		r.GET("/test", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

		req := httptest.NewRequest("GET", "http://127.0.0.1:8080/test", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Origin", "http://localhost:8080")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("期望 200, 实际得到: %d", w.Code)
		}
	})

	t.Run("有凭据时放行外部访问交由业务鉴权", func(t *testing.T) {
		r := gin.New()
		r.Use(dnsRebindingMiddleware(true))
		r.GET("/test", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

		req := httptest.NewRequest("GET", "http://myserver.com/test", nil)
		req.RemoteAddr = "10.0.0.1:12345"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("期望 200, 实际得到: %d", w.Code)
		}
	})
}

func TestBodyLimitMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(bodyLimitMiddleware())
	r.POST("/upload", func(c *gin.Context) {
		var req map[string]string
		if err := c.ShouldBindJSON(&req); err != nil {
			c.String(http.StatusBadRequest, "bind error: %v", err)
			return
		}
		c.String(http.StatusOK, "ok")
	})

	// 1. 合法小请求
	smallBody := bytes.NewBufferString(`{"key":"value"}`)
	reqSmall := httptest.NewRequest(http.MethodPost, "/upload", smallBody)
	reqSmall.Header.Set("Content-Type", "application/json")
	wSmall := httptest.NewRecorder()
	r.ServeHTTP(wSmall, reqSmall)
	if wSmall.Code != http.StatusOK {
		t.Fatalf("合法小请求期望 200, 得到: %d", wSmall.Code)
	}

	// 2. 超大请求体 (> 1MB) 应当被 MaxBytesReader 拦截报错
	oversized := bytes.Repeat([]byte("a"), int(maxBodyBytes)+1024)
	reqBig := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(oversized))
	reqBig.Header.Set("Content-Type", "application/json")
	wBig := httptest.NewRecorder()
	r.ServeHTTP(wBig, reqBig)
	if wBig.Code != http.StatusBadRequest {
		t.Fatalf("超限请求期望 400 (绑定失败), 得到: %d", wBig.Code)
	}
}
