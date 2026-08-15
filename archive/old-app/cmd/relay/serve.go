package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/autoseedrelay/go-relay/internal/config"
	"github.com/autoseedrelay/go-relay/internal/engine"
	"github.com/autoseedrelay/go-relay/internal/web"
)

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "启动 v3 Web 管理面板 + 引擎",
		RunE:  runServe,
	}
	cmd.Flags().String("config", "", "配置文件路径")
	cmd.Flags().String("addr", "", "Web 监听地址")
	return cmd
}

func runServe(cmd *cobra.Command, args []string) error {
	configPath, _ := cmd.Flags().GetString("config")
	listenAddr, _ := cmd.Flags().GetString("addr")

	cfg, err := config.LoadAppConfig(configPath)
	if err != nil {
		if strings.Contains(err.Error(), "config file not found") || strings.Contains(err.Error(), "no config file found") {
			slog.Warn("未找到配置文件，以设置向导模式启动")
			cfg = config.NewAppConfig()
		} else {
			return fmt.Errorf("加载配置失败: %w", err)
		}
	}

	if listenAddr != "" {
		cfg.Web.ListenAddr = listenAddr
	}

	setupLogging(cfg.LogLevel)
	slog.Info("AutoSeedRelay v3 启动中...", "version", "3.0.0", "listen", cfg.Web.ListenAddr)

	// Auto-fix qB password on startup.
	if cfg.QB.Host != "" {
		if err := ensureQBPassword(cfg); err != nil {
			slog.Warn("qB 密码自动修复失败，请通过 Web 面板配置", "error", err)
		}
	}

	eng, err := engine.New(cfg)
	if err != nil {
		return fmt.Errorf("初始化引擎失败: %w", err)
	}
	if err := eng.Start(); err != nil {
		return fmt.Errorf("启动引擎失败: %w", err)
	}

	go func() {
		if err := web.StartServer(cfg.Web.ListenAddr, eng, cfg); err != nil {
			slog.Error("web server 错误", "error", err)
		}
	}()

	slog.Info("AutoSeedRelay v3 已就绪", "addr", cfg.Web.ListenAddr)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	slog.Info("收到信号，正在关闭...", "signal", sig.String())
	eng.Stop()
	slog.Info("AutoSeedRelay 已关闭")
	return nil
}

// ensureQBPassword auto-fixes qB password on startup.
func ensureQBPassword(cfg *config.AppConfig) error {
	// Try configured password first.
	ok, _ := qbLoginOK(cfg)
	if ok {
		slog.Debug("qB 密码正确，无需修复")
		return nil
	}

	slog.Warn("qB 密码不匹配，尝试从容器日志读取临时密码...")
	tempPW := readQBTempPassword()
	if tempPW == "" {
		return fmt.Errorf("无法获取 qB 临时密码")
	}

	// Set permanent password to CHANGE_ME via qB API.
	if err := setQBPasswordViaAPI(cfg, tempPW); err != nil {
		return err
	}

	cfg.QB.Password = "CHANGE_ME"
	slog.Info("qB 密码已自动设置为 CHANGE_ME")
	return nil
}

func qbLoginOK(cfg *config.AppConfig) (bool, error) {
	u := qbBaseURL(cfg) + "/api/v2/auth/login"
	resp, err := http.PostForm(u, url.Values{"username": {cfg.QB.Username}, "password": {cfg.QB.Password}})
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	return resp.StatusCode == 200 || resp.StatusCode == 204, nil
}

func readQBTempPassword() string {
	out, err := exec.Command("docker", "logs", "qbittorrent").CombinedOutput()
	if err != nil {
		return ""
	}
	m := regexp.MustCompile(`session:\s*(\S+)`).FindStringSubmatch(string(out))
	if m != nil {
		return m[1]
	}
	return ""
}

func setQBPasswordViaAPI(cfg *config.AppConfig, tempPW string) error {
	base := qbBaseURL(cfg)
	client := &http.Client{}

	// Login with temp password.
	resp, err := client.PostForm(base+"/api/v2/auth/login",
		url.Values{"username": {cfg.QB.Username}, "password": {tempPW}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Extract SID.
	var sid string
	for _, c := range resp.Cookies() {
		if c.Name == "SID" {
			sid = c.Value
			break
		}
	}
	if sid == "" {
		m := regexp.MustCompile(`SID=([^;]+)`).FindStringSubmatch(resp.Header.Get("Set-Cookie"))
		if m != nil {
			sid = m[1]
		}
	}
	if sid == "" {
		return fmt.Errorf("无法获取 qB SID cookie")
	}

	// Set permanent password.
	body := `{"web_ui_password":"CHANGE_ME"}`
	req, _ := http.NewRequest("POST", base+"/api/v2/app/setPreferences", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "SID="+sid)
	r2, err := client.Do(req)
	if err != nil {
		return err
	}
	r2.Body.Close()
	return nil
}

func qbBaseURL(cfg *config.AppConfig) string {
	scheme := "http"
	if cfg.QB.UseSSL {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, cfg.QB.Host, cfg.QB.Port)
}

func setupLogging(level string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	slog.SetDefault(slog.New(handler))
}
