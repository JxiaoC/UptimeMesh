package kuma

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// UptimeKuma 2.x(master)把认证换成了 better-auth:Socket.IO 不再有 login/loginByToken
// 事件,连接时改为从 **会话 Cookie** 取身份(server/server.js:
// `session = await getSession(socket.request.headers.cookie)`)。因此 2.x 的导入要分两步:
//
//	① HTTP 登录 better-auth 拿会话 Cookie:
//	   POST {base}/api/auth/sign-in/username  {username, password, rememberMe}
//	   开了两步验证时先返回 {"twoFactorRedirect":true},再
//	   POST {base}/api/auth/two-factor/verify-totp  {code}
//	   (端点与前端 src/auth-client.ts 的 signIn.username / twoFactor.verifyTotp 一致,
//	    better-auth 默认 basePath 为 /api/auth)
//	② 把 Cookie 放进 Socket.IO 握手的 Cookie 头,服务端认会话后推送 monitorList。
//
// 凭据与 Cookie 都只在本次导入内使用,不落库。1.x 没有这些端点,由 socket 事件登录兜底。

const (
	// betterAuthSignInPath 用户名密码登录端点(username 插件)。
	betterAuthSignInPath = "/api/auth/sign-in/username"
	// betterAuthVerifyTOTPPath 两步验证校验端点(twoFactor 插件)。
	betterAuthVerifyTOTPPath = "/api/auth/two-factor/verify-totp"
	// betterAuthTrustDevicePath 勾选"信任此设备"时调用,避免同一浏览器反复要求验证码。
	betterAuthTrustDevicePath = "/api/auth/two-factor/trust-device"
	// betterAuthMaxBody 认证响应体上限。
	betterAuthMaxBody = 1 << 20
	// betterAuthTimeout 单次认证请求超时。
	betterAuthTimeout = 15 * time.Second
)

// ErrSessionRejected 会话 Cookie 未被 UptimeKuma 接受(2.x 握手后仍要求登录)。
var ErrSessionRejected = errors.New("登录会话未被 UptimeKuma 接受")

// ErrNoBetterAuth 目标上没有 better-auth 登录端点:通常意味着并非新版,
// 或地址/子路径不对,或反代只转发了 socket.io 而没转发 /api/auth/*。
var ErrNoBetterAuth = errors.New("未找到 UptimeKuma 的登录接口")

// BetterAuthAvailable 探测目标是否有 better-auth 登录接口:GET /api/auth/ok 是
// better-auth 自带的健康检查,回 JSON {"ok":true};1.x 与 2.x 的正式发布没有这个路径,
// 会落到 Kuma 的 SPA catch-all 而返回 HTML,据此可区分。
//
// 探测只用于选路:返回 false 时仍会按 login 事件登录;判断不准还有事件登录无应答后的
// 兜底切换,因此不会因为探测失误而走错路。
func BetterAuthAvailable(ctx context.Context, base *BaseURL, insecureTLS bool) bool {
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureTLS}},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.RESTURL("/api/auth/ok"), nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return false
	}
	return strings.Contains(string(body), `"ok"`)
}

// LoginViaBetterAuth 按 2.x 的方式登录,返回可直接放进 Socket.IO 握手头的 Cookie 串。
func LoginViaBetterAuth(ctx context.Context, base *BaseURL, login PasswordLogin, insecureTLS bool) (string, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return "", fmt.Errorf("初始化 Cookie 容器失败: %w", err)
	}
	client := &http.Client{
		Timeout:   betterAuthTimeout,
		Jar:       jar,
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureTLS}},
	}

	// ① 用户名 + 密码。rememberMe 让会话 Cookie 带上较长有效期,省得导入反复要登录。
	status, body, err := postJSON(ctx, client, base.RESTURL(betterAuthSignInPath), map[string]any{
		"username": login.Username, "password": login.Password, "rememberMe": true,
	})
	if err != nil {
		return "", fmt.Errorf("无法连接 UptimeKuma(%s): %w", base.Origin(), err)
	}
	switch {
	case status == http.StatusOK:
	case status == http.StatusUnauthorized, status == http.StatusBadRequest, status == http.StatusTooManyRequests:
		return "", fmt.Errorf("%w(%s)", ErrLoginFailed, authMessage(body, status))
	case status == http.StatusNotFound:
		// Kuma 1.x 没有这些端点(其 GET catch-all 不处理 POST,会落到 404)。
		return "", fmt.Errorf("%w(%s)", ErrNoBetterAuth, base.RESTURL(betterAuthSignInPath))
	default:
		return "", fmt.Errorf("UptimeKuma 返回 HTTP %d", status)
	}

	// ② 两步验证:better-auth 的 twoFactor 插件先回 twoFactorRedirect,再单独校验 TOTP。
	if twoFactorRedirect(body) {
		if strings.TrimSpace(login.TwoFACode) == "" {
			return "", ErrTwoFARequired
		}
		status, body, err = postJSON(ctx, client, base.RESTURL(betterAuthVerifyTOTPPath), map[string]any{
			"code": strings.TrimSpace(login.TwoFACode),
		})
		if err != nil {
			return "", fmt.Errorf("无法连接 UptimeKuma(%s): %w", base.Origin(), err)
		}
		if status != http.StatusOK {
			return "", fmt.Errorf("%w(%s)", ErrTwoFAInvalid, authMessage(body, status))
		}
		// 尽力而为:验证通过后标记本设备可信,不影响导入结果。
		_, _, _ = postJSON(ctx, client, base.RESTURL(betterAuthTrustDevicePath), map[string]any{})
	}

	cookie := cookieHeader(jar, base)
	if cookie == "" {
		return "", errors.New("登录成功但没有拿到会话 Cookie:请确认 UptimeKuma 未关闭 Cookie 会话")
	}
	return cookie, nil
}

// postJSON 发一个 JSON POST,返回状态码与响应体(已限长)。
func postJSON(ctx context.Context, client *http.Client, endpoint string, payload any) (int, []byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, betterAuthMaxBody))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// twoFactorRedirect 判断 better-auth 是否要求两步验证。
func twoFactorRedirect(body []byte) bool {
	parsed := map[string]any{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	v, ok := parsed["twoFactorRedirect"]
	if !ok {
		return false
	}
	b, isBool := v.(bool)
	return isBool && b
}

// authMessage 取出 better-auth 的错误文案(message/code),取不到时回落到状态码。
func authMessage(body []byte, status int) string {
	parsed := map[string]any{}
	if err := json.Unmarshal(body, &parsed); err == nil {
		for _, key := range []string{"message", "error", "code"} {
			if s, ok := parsed[key].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	if text := strings.TrimSpace(string(body)); text != "" && len(text) <= 200 {
		return text
	}
	return fmt.Sprintf("HTTP %d", status)
}

// cookieHeader 把 Cookie 容器里该站点的 Cookie 拼成握手用的 Cookie 头。
// better-auth 的会话 Cookie 是 httpOnly,只能这样透传给 Socket.IO。
func cookieHeader(jar http.CookieJar, base *BaseURL) string {
	u, err := url.Parse(base.Origin())
	if err != nil {
		return ""
	}
	parts := make([]string, 0, 4)
	for _, c := range jar.Cookies(u) {
		if c == nil || c.Name == "" {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}
