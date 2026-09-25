/**
 * [INPUT]: 依赖 context, flag, os, path/filepath, time, icloud-hme/internal/account, icloud-hme/internal/server, icloud-hme/internal/store
 * [OUTPUT]: icloud-hme 二进制可执行文件入口 (支持 -backup 与 -restore 一致性容灾 CLI)
 * [POS]: 项目全局 CLI 引导与环境初始化层
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

// Command icloud-hme 启动 iCloud Hide My Email 多账号管理平台。
//
// 两个核心 HTTP 接口:
//
//	POST /api/create  — 创建隐私邮箱别名
//	GET  /api/inbox   — 读取邮件
//
// 用法:
//
//	./icloud-hme                    # 默认 :8081
//	./icloud-hme -addr :9000        # 指定端口
//	./icloud-hme -data ./data       # 指定数据目录
//	./icloud-hme -data ./data -backup ./backup.db   # 离线/在线一致性备份 (不启动服务)
//	./icloud-hme -data ./data -restore ./backup.db  # 离线一致性恢复并校验 (不启动服务)
//	./icloud-hme -debug             # 调试模式
//	./icloud-hme -log-level debug   # 日志级别 (debug/info/warn/error)
//
// 安全配置(必填):
//
//	ICLOUD_HME_ADMIN_PASSWORD      管理员密码,至少 8 字符(进程启动后从环境清除)
//	ICLOUD_HME_SESSION_TTL         会话有效期,默认 12h,范围 15m-168h
//	ICLOUD_HME_SECURE_COOKIE       TLS 反向代理部署时设为 true
//	ICLOUD_HME_COOKIE_MONITOR_INTERVAL  Cookie 健康监控周期,默认 30m,范围 5m-24h
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/security"
	"icloud-hme/internal/server"
	"icloud-hme/internal/store"
)

// 构建期注入的版本信息(见 build.sh 的 -ldflags -X)。
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8081", "HTTP 监听地址 (默认 127.0.0.1:8081 本机回环)")
	dataDir := flag.String("data", "./data", "数据目录 (accounts.json 存放位置)")
	debug := flag.Bool("debug", false, "调试模式 (启用 Gin 调试日志)")
	passwordFlag := flag.String("password", "", "管理员密码 (至少 8 字符,也可通过 ICLOUD_HME_ADMIN_PASSWORD 设置)")
	apiKeyFlag := flag.String("api-key", "", "自动化 API Key (也可通过 ICLOUD_HME_API_KEY 设置)")
	apiTokenFlag := flag.String("api-token", "", "兼容参考项目的 API Token 参数 (也可通过 ICLOUD_PRIME_API_TOKEN 设置)")
	backupFlag := flag.String("backup", "", "一致性备份目标文件路径")
	restoreFlag := flag.String("restore", "", "一致性恢复源备份文件路径")
	rotateCredentialsFlag := flag.Bool("rotate-credentials", false, "执行 Master Key 离线凭据轮换 (不启动服务)")
	flag.Parse()

	if *backupFlag != "" && *restoreFlag != "" {
		log.Fatalf("错误: -backup 与 -restore 参数互斥，不能同时指定")
	}

	// 离线/一致性备份模式 (不要求管理员密码、不启动 HTTP/Worker/Apple 凭据)
	if *backupFlag != "" {
		absDataDir, err := filepath.Abs(*dataDir)
		if err != nil {
			log.Fatalf("数据目录路径错误: %v", err)
		}
		destPath, err := filepath.Abs(*backupFlag)
		if err != nil {
			log.Fatalf("备份目标路径错误: %v", err)
		}
		st, err := store.NewStore(absDataDir)
		if err != nil {
			log.Fatalf("打开数据库存储失败: %v", err)
		}
		defer st.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := st.CreateBackup(ctx, destPath); err != nil {
			log.Fatalf("创建一致性备份失败: %v", err)
		}
		fmt.Printf("一致性备份创建成功: %s\n", destPath)
		return
	}

	// 离线一致性恢复模式 (不要求管理员密码、不启动 HTTP/Worker/Apple 凭据)
	if *restoreFlag != "" {
		absDataDir, err := filepath.Abs(*dataDir)
		if err != nil {
			log.Fatalf("数据目录路径错误: %v", err)
		}
		srcPath, err := filepath.Abs(*restoreFlag)
		if err != nil {
			log.Fatalf("备份源路径错误: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := store.RestoreDatabase(ctx, absDataDir, srcPath); err != nil {
			log.Fatalf("恢复数据库失败: %v", err)
		}
		fmt.Printf("数据库恢复成功: %s -> %s\n", srcPath, absDataDir)
		return
	}

	// 离线凭据轮换模式 (不启动服务)
	if *rotateCredentialsFlag {
		absDataDir, err := filepath.Abs(*dataDir)
		if err != nil {
			log.Fatalf("数据目录路径错误: %v", err)
		}
		oldKey, err := security.LoadMasterKey()
		if err != nil {
			log.Fatalf("读取当前 Master Key 失败: %v", err)
		}
		oldCipher, err := security.NewSecretCipher(oldKey)
		if err != nil {
			log.Fatalf("当前 Master Key 无效: %v", err)
		}

		newKeyRaw := strings.TrimSpace(os.Getenv("ICLOUD_HME_NEW_MASTER_KEY"))
		if newKeyFile := strings.TrimSpace(os.Getenv("ICLOUD_HME_NEW_MASTER_KEY_FILE")); newKeyFile != "" {
			data, readErr := os.ReadFile(newKeyFile)
			if readErr != nil {
				log.Fatalf("读取新 Master Key 文件失败 (%s): %v", newKeyFile, readErr)
			}
			newKeyRaw = string(data)
		}
		if newKeyRaw == "" {
			log.Fatalf("缺少新 Master Key 配置: 请设置环境变量 ICLOUD_HME_NEW_MASTER_KEY 或 ICLOUD_HME_NEW_MASTER_KEY_FILE")
		}
		newKey, err := security.ParseMasterKey(newKeyRaw)
		if err != nil {
			log.Fatalf("解析新 Master Key 失败: %v", err)
		}
		newCipher, err := security.NewSecretCipher(newKey)
		if err != nil {
			log.Fatalf("新 Master Key 无效: %v", err)
		}

		if err := store.RotateCredentials(absDataDir, oldCipher, newCipher); err != nil {
			log.Fatalf("凭据轮换失败: %v", err)
		}
		fmt.Printf("凭据 Master Key 离线轮换成功！\n请将生产环境变量 ICLOUD_HME_MASTER_KEY / 文件更新为新密钥后重新启动服务。\n")
		return
	}

	// 自动从系统配置文件、当前目录或数据目录读取并装入 .env 中所有环境变量
	loadEnvFiles("/etc/icloud-hme.env", ".env", filepath.Join(*dataDir, ".env"))

	addrSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "addr" {
			addrSet = true
		}
	})
	listenAddr := resolveListenAddr(*addr, addrSet)

	adminPassword := os.Getenv("ICLOUD_HME_ADMIN_PASSWORD")
	if adminPassword == "" && *passwordFlag != "" {
		adminPassword = *passwordFlag
	}
	if err := validateAdminPassword(adminPassword); err != nil {
		log.Fatalf("%v", err)
	}
	sessionTTL, err := parseSessionTTL(os.Getenv("ICLOUD_HME_SESSION_TTL"))
	if err != nil {
		log.Fatalf("ICLOUD_HME_SESSION_TTL 无效: %v", err)
	}
	cookieMonitorInterval, err := parseCookieMonitorInterval(os.Getenv("ICLOUD_HME_COOKIE_MONITOR_INTERVAL"))
	if err != nil {
		log.Fatalf("ICLOUD_HME_COOKIE_MONITOR_INTERVAL 无效: %v", err)
	}
	cookieThrottle, err := parseOptionalDuration("ICLOUD_HME_COOKIE_THROTTLE", 50*time.Millisecond, 5*time.Minute)
	if err != nil {
		log.Fatalf("%v", err)
	}
	startupSyncInterval, err := parseOptionalDuration("ICLOUD_HME_STARTUP_SYNC_INTERVAL", 50*time.Millisecond, 5*time.Minute)
	if err != nil {
		log.Fatalf("%v", err)
	}
	mailPollInterval, err := parseOptionalDuration("ICLOUD_HME_MAIL_POLL_INTERVAL", 500*time.Millisecond, time.Minute)
	if err != nil {
		log.Fatalf("%v", err)
	}
	leaseRetention, err := parseRetention(os.Getenv("ICLOUD_HME_LEASE_RETENTION"))
	if err != nil {
		log.Fatalf("%v", err)
	}
	secureCookie := os.Getenv("ICLOUD_HME_SECURE_COOKIE") == "true"
	trustedProxies := parseTrustedProxies(os.Getenv("ICLOUD_HME_TRUSTED_PROXIES"))

	log.Printf("iCloud Hide My Email 服务启动 addr=%s version=%s commit=%s built=%s", listenAddr, version, commit, buildTime)

	abs, err := filepath.Abs(*dataDir)
	if err != nil {
		log.Fatalf("数据目录路径错误: %v", err)
	}

	masterKey, err := security.LoadMasterKey()
	if err != nil {
		log.Fatalf("Master Key 配置错误 (Fail Closed): %v", err)
	}
	cipher, err := security.NewSecretCipher(masterKey)
	if err != nil {
		log.Fatalf("初始化 Master Key 加密机失败: %v", err)
	}

	st, err := store.NewStoreWithCipher(abs, cipher)
	if err != nil {
		log.Fatalf("初始化数据库存储失败: %v", err)
	}
	defer st.Close()

	mgr, err := account.NewManager(abs, st)
	if err != nil {
		log.Fatalf("初始化账号管理器失败: %v", err)
	}
	defer mgr.Close()
	count := len(mgr.ListAccounts())
	log.Printf("账号加载完成 count=%d data_dir=%s", count, abs)

	apiKey := os.Getenv("ICLOUD_HME_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("ICLOUD_PRIME_API_TOKEN")
	}
	if *apiKeyFlag != "" {
		apiKey = *apiKeyFlag
	} else if *apiTokenFlag != "" {
		apiKey = *apiTokenFlag
	}
	if apiKey != "" {
		log.Printf("自动化 API Key 鉴权已启用 (免 CSRF/Session)")
	}

	srv, err := server.New(mgr, st, server.Config{
		DataDir:               abs,
		Debug:                 *debug,
		AdminPassword:         adminPassword,
		APIKey:                apiKey,
		SessionTTL:            sessionTTL,
		SecureCookie:          secureCookie,
		CookieMonitorInterval: cookieMonitorInterval,
		CookieMonitorThrottle: cookieThrottle,
		StartupSyncInterval:   startupSyncInterval,
		MailPollInterval:      mailPollInterval,
		LeaseRetention:        leaseRetention,
		TrustedProxies:        trustedProxies,
	})
	if err != nil {
		log.Fatalf("初始化服务失败: %v", err)
	}
	defer srv.Close()

	// 密码只用于初始化认证,随后立即从进程环境清除
	_ = os.Unsetenv("ICLOUD_HME_ADMIN_PASSWORD")

	log.Printf("HTTP 服务就绪 addr=%s", listenAddr)
	if err := srv.Run(listenAddr); err != nil {
		log.Fatalf("服务启动失败: %v", err)
	}
}

// knownWeakPasswords 是随仓库/部署模板公开分发的占位口令。
//
// 这些值长度都 >= 8，能骗过单纯的长度校验；一旦照抄 compose/service 模板启动，
// 任何读过该仓库的人都能登录管理台读写母号凭据，因此必须在启动阶段硬拒绝。
var knownWeakPasswords = map[string]bool{
	"admin123456":                    true,
	"your_strong_password_here":      true,
	"change_this_to_strong_password": true,
	"your_secret_api_key_here":       true,
	"your_secret_api_key_123456":     true,
	"__change_me__":                  true,
	"changeme":                       true,
	"password":                       true,
	"12345678":                       true,
}

// validateAdminPassword 校验管理员口令：长度下限 + 拒绝仓库公开占位值。
func validateAdminPassword(pw string) error {
	if len(pw) < 8 {
		return errors.New("请通过环境变量 ICLOUD_HME_ADMIN_PASSWORD 或 -password 参数设置管理员密码(至少 8 个字符)")
	}
	if knownWeakPasswords[strings.ToLower(strings.TrimSpace(pw))] {
		return errors.New("检测到仓库公开的占位/示例口令，请改成强口令后再启动(否则等同于无鉴权暴露母号凭据)")
	}
	return nil
}

// parseSessionTTL 解析会话有效期,默认 12h,范围 15m-168h。
func parseSessionTTL(raw string) (time.Duration, error) {
	if raw == "" {
		return 12 * time.Hour, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err
	}
	if d < 15*time.Minute || d > 168*time.Hour {
		return 0, fmt.Errorf("session TTL %v 超出范围 [15m, 168h]", d)
	}
	return d, nil
}

// parseRetention 解析流水保留期。支持 "180d" 这样的天数写法，
// 也接受标准 Go 时长；空值返回 0 表示永久保留(不清理)。
func parseRetention(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	if n, ok := strings.CutSuffix(raw, "d"); ok {
		days, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("ICLOUD_HME_LEASE_RETENTION 无效: %s", raw)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("ICLOUD_HME_LEASE_RETENTION 无效: %w", err)
	}
	if d < 24*time.Hour {
		return 0, fmt.Errorf("ICLOUD_HME_LEASE_RETENTION (%v) 至少为 24h，否则会误删近期流水", d)
	}
	return d, nil
}

// parseOptionalDuration 解析可选时长环境变量；空值返回 0 表示「自动」，
// 非空时校验范围，防止把节流间隔配成 0 或数小时这类危险值。
func parseOptionalDuration(env string, min, max time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(env))
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s 无效: %w", env, err)
	}
	if d < min || d > max {
		return 0, fmt.Errorf("%s (%v) 超出允许范围 [%v, %v]", env, d, min, max)
	}
	return d, nil
}

// parseTrustedProxies 解析可信反向代理地址列表(逗号分隔的 IP 或 CIDR)。
//
// 留空返回 nil，表示不信任任何代理头 —— ClientIP() 取真实连接地址，
// X-Forwarded-For 无法伪造(最安全默认值)。经 Nginx/Caddy 反代部署时，
// 应填**仅**代理自身地址(如 127.0.0.1 或 172.17.0.0/16)，
// 否则所有请求共用同一个限流桶，攻击者可借此把管理员锁在登录页外。
func parseTrustedProxies(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseCookieMonitorInterval 解析 Cookie 健康监控周期,默认 30m,范围 5m-24h。
func parseCookieMonitorInterval(raw string) (time.Duration, error) {
	if raw == "" {
		return 30 * time.Minute, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err
	}
	if d < 5*time.Minute || d > 24*time.Hour {
		return 0, fmt.Errorf("cookie monitor interval %v 超出范围 [5m, 24h]", d)
	}
	return d, nil
}

// loadEnvFiles 从指定路径依次读取并装载 .env 文件中的环境变量(不覆盖已有变量)。
func loadEnvFiles(paths ...string) {
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				k := strings.TrimSpace(parts[0])
				v := strings.TrimSpace(parts[1])
				if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
					v = v[1 : len(v)-1]
				}
				if os.Getenv(k) == "" && k != "" {
					_ = os.Setenv(k, v)
				}
			}
		}
	}
}

// resolveListenAddr 智能解析监听地址，优先级: CLI显式参数 > ICLOUD_HME_ADDR > ADDR > PORT > 默认回环
func resolveListenAddr(cliAddr string, cliSet bool) string {
	if cliSet && strings.TrimSpace(cliAddr) != "" {
		return strings.TrimSpace(cliAddr)
	}
	if envAddr := strings.TrimSpace(os.Getenv("ICLOUD_HME_ADDR")); envAddr != "" {
		return envAddr
	}
	if envAddr := strings.TrimSpace(os.Getenv("ADDR")); envAddr != "" {
		return envAddr
	}
	if port := strings.TrimSpace(os.Getenv("PORT")); port != "" {
		if !strings.HasPrefix(port, ":") {
			port = ":" + port
		}
		return port
	}
	if strings.TrimSpace(cliAddr) != "" {
		return strings.TrimSpace(cliAddr)
	}
	return "127.0.0.1:8081"
}
