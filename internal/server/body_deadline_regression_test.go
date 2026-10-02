package server

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// 无正文请求的连接读取期限会让 net/http 后台读超时并取消请求 Context；
// 长于读取预算的 GET/DELETE/空 POST 必须正常完成。
func TestBodyLimitMiddlewareDoesNotCancelLongBodylessRequests(t *testing.T) {
	old := bodyReadTimeout
	bodyReadTimeout = 200 * time.Millisecond
	defer func() { bodyReadTimeout = old }()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(bodyLimitMiddleware())
	handler := func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		select {
		case <-time.After(3 * bodyReadTimeout):
			c.String(http.StatusOK, "done")
		case <-c.Request.Context().Done():
			c.String(http.StatusInternalServerError, "canceled: "+c.Request.Context().Err().Error())
		}
	}
	r.Any("/long", handler)
	srv := httptest.NewServer(r)
	defer srv.Close()

	cases := []struct {
		method string
		body   io.Reader
	}{
		{http.MethodGet, nil},
		{http.MethodDelete, nil},
		{http.MethodPost, nil},
		{http.MethodPost, strings.NewReader(`{"a":1}`)},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(tc.method, srv.URL+"/long", tc.body)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", tc.method, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "done" {
			t.Fatalf("%s (body=%v) request was canceled by read deadline: status=%d body=%q", tc.method, tc.body != nil, resp.StatusCode, body)
		}
	}
}

// 慢正文仍受读取预算约束，不能因跳过无正文请求而失去防护。
func TestBodyLimitMiddlewareStillBoundsSlowBody(t *testing.T) {
	old := bodyReadTimeout
	bodyReadTimeout = 200 * time.Millisecond
	defer func() { bodyReadTimeout = old }()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(bodyLimitMiddleware())
	readErr := make(chan error, 1)
	r.POST("/slow", func(c *gin.Context) {
		_, err := io.ReadAll(c.Request.Body)
		readErr <- err
		c.Status(http.StatusBadRequest)
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// 声明 100 字节正文但只发送 1 字节，模拟慢速攻击。
	if _, err := io.WriteString(conn, "POST /slow HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n{"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("slow body read unexpectedly succeeded")
		}
	case <-time.After(10 * bodyReadTimeout):
		t.Fatal("slow body was not interrupted by read budget")
	}
}
