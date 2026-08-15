package qb

import (
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// AutoInit automatically initialises a qBittorrent instance reachable at
// host (e.g. "http://127.0.0.1:9021").
//
// NOTE: This function expects the default password "adminadmin", which was
// deprecated in qBittorrent >= 4.6.1. Modern qB versions auto-generate a
// random temporary password instead.
//
// The preferred approach is to pre-create qBittorrent.conf with a PBKDF2
// password hash before container startup (see scripts/init_qb_config.py).
// This function remains as a fallback for older qB versions.
func AutoInit(host string, downloadPath string) (username, password string, err error) {
	host = strings.TrimRight(host, "/")

	// 1. wait for qB API
	if err := waitForAPI(host, 30*time.Second); err != nil {
		return "", "", fmt.Errorf("qB 未就绪: %w", err)
	}

	// create a client with default credentials
	q, err := NewQBittorrent(host, "admin", "adminadmin", 30)
	if err != nil {
		return "", "", fmt.Errorf("创建 qB 客户端失败: %w", err)
	}
	defer q.Close()

	// login
	if err := q.Login(); err != nil {
		return "", "", fmt.Errorf("qB 默认凭据登录失败(可能已被修改过): %w", err)
	}

	// 2. change password
	newPass := generatePassword(16)
	if err := changePassword(q, "adminadmin", newPass); err != nil {
		return "", "", fmt.Errorf("修改 qB 密码失败: %w", err)
	}

	// 3. set default save path
	if err := setPreferences(q, map[string]any{
		"save_path":         downloadPath,
		"temp_path_enabled": false,
	}); err != nil {
		return "", "", fmt.Errorf("设置下载路径失败: %w", err)
	}

	// 4. set WebUI port (honour env, default 9021)
	webPort := 9021
	if v := getEnvAny("QB_WEBUI_PORT", "WEBUI_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			webPort = n
		}
	}
	if err := setPreferences(q, map[string]any{
		"web_ui_port": webPort,
	}); err != nil {
		return "", "", fmt.Errorf("设置 WebUI 端口失败: %w", err)
	}

	return "admin", newPass, nil
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

// waitForAPI polls /api/v2/app/version until it gets a 200 OK or timeout
// expires.
func waitForAPI(host string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	u := host + "/api/v2/app/version"
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("等待 qB API %s 就绪超时(%v)", u, timeout)
		}
		resp, err := client.Get(u)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(1 * time.Second)
	}
}

// changePassword calls POST /api/v2/auth/changePassword on the already
// logged-in client q.
func changePassword(q *QBittorrent, oldPass, newPass string) error {
	resp, err := q.postForm("/api/v2/auth/changePassword", url.Values{
		"old_password": {oldPass},
		"new_password": {newPass},
	})
	if err != nil {
		return err
	}
	return q.expectOK(resp, "auth/changePassword")
}

// setPreferences calls POST /api/v2/app/setPreferences with a JSON body.
func setPreferences(q *QBittorrent, prefs map[string]any) error {
	resp, err := q.postJSON("/api/v2/app/setPreferences", prefs)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := truncate(string(body), 200)
	if resp.StatusCode >= 400 {
		return reqErrf("setPreferences 失败(HTTP %d): %s", resp.StatusCode, text)
	}
	return nil
}

// generatePassword returns a random alphanumeric string of length n.
func generatePassword(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			b[i] = 'x'
			continue
		}
		b[i] = chars[idx.Int64()]
	}
	return string(b)
}

// getEnvAny returns the value of the first environment variable that is set.
func getEnvAny(keys ...string) string {
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
