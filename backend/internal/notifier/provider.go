// Package notifier implements the notification delivery pipeline for M2b:
// seven built-in providers, tier-based routing with a 10-minute aggregation
// window, and per-instance circuit breakers. Business rules follow
// docs/BIZ-SPEC.md §8 (notification matrix).
package notifier

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Level is a notification tier (critical / warning / info). See BIZ-SPEC §8.
type Level string

const (
	LevelCritical Level = "critical"
	LevelWarning  Level = "warning"
	LevelInfo     Level = "info"
)

// Message is a single notification to deliver.
type Message struct {
	Title string
	Body  string
	Level Level
}

// String renders the message as one line, used when aggregating a window.
func (m Message) String() string {
	switch {
	case m.Title == "":
		return m.Body
	case m.Body == "":
		return m.Title
	default:
		return m.Title + ": " + m.Body
	}
}

// Notifier delivers a single message to one destination.
type Notifier interface {
	Send(ctx context.Context, msg Message) error
}

// ProviderType names a built-in provider.
type ProviderType string

const (
	TypeWebhook    ProviderType = "webhook"
	TypeTelegram   ProviderType = "telegram"
	TypeSMTP       ProviderType = "smtp"
	TypeNtfy       ProviderType = "ntfy"
	TypeGotify     ProviderType = "gotify"
	TypeServerChan ProviderType = "serverchan"
	TypePushPlus   ProviderType = "pushplus"
)

// Config holds the plaintext configuration for one notifier instance.
// Sensitive fields (tokens, passwords) are stored in cleartext here; the
// storage layer is responsible for encrypting them at rest.
type Config struct {
	Type    ProviderType
	Enabled bool
	Name    string // instance name used by the router's subscription matrix

	// webhook
	WebhookURL     string
	WebhookHeaders map[string]string

	// telegram
	TelegramToken   string
	TelegramChatID  string
	TelegramBaseURL string // default https://api.telegram.org

	// smtp
	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string
	SMTPFrom string
	SMTPTo   []string

	// ntfy
	NtfyURL   string // full topic URL; defaults to https://ntfy.sh/<NtfyTopic>
	NtfyTopic string
	NtfyUser  string
	NtfyPass  string

	// gotify
	GotifyURL   string
	GotifyToken string

	// serverchan
	ServerChanURL string
	ServerChanKey string

	// pushplus
	PushPlusURL   string
	PushPlusToken string
}

const (
	defaultTelegramBaseURL = "https://api.telegram.org"
	defaultTelegramEditWin = 10 * time.Minute
	defaultNtfyBaseURL     = "https://ntfy.sh"
	defaultServerChanURL   = "https://sctapi.ftqq.com"
	defaultPushPlusURL     = "https://www.pushplus.plus/send"
	defaultSMTPPort        = 25
)

// defaultHTTPClient is shared by all HTTP-based providers.
var defaultHTTPClient = &http.Client{Timeout: 10 * time.Second}

// New builds a provider from its Config. It does not inspect cfg.Enabled; the
// caller decides which instances to register with the Router.
func New(cfg Config) (Notifier, error) {
	switch cfg.Type {
	case TypeWebhook:
		return newWebhook(cfg)
	case TypeTelegram:
		return newTelegram(cfg)
	case TypeSMTP:
		return newSMTP(cfg)
	case TypeNtfy:
		return newNtfy(cfg)
	case TypeGotify:
		return newGotify(cfg)
	case TypeServerChan:
		return newServerChan(cfg)
	case TypePushPlus:
		return newPushPlus(cfg)
	default:
		return nil, fmt.Errorf("notifier: unknown provider type %q", cfg.Type)
	}
}

// --- HTTP plumbing ---

func doRequest(ctx context.Context, client *http.Client, req *http.Request) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("notifier: %s returned %s: %s",
			req.URL.Redacted(), resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func doJSON(ctx context.Context, client *http.Client, target string, payload any, headers map[string]string) ([]byte, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return doRequest(ctx, client, req)
}

func postJSON(ctx context.Context, client *http.Client, target string, payload any, headers map[string]string) error {
	_, err := doJSON(ctx, client, target, payload, headers)
	return err
}

func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// --- webhook ---

// webhookNotifier POSTs a generic JSON body {"title","body","level"} to a URL,
// with optional extra headers (e.g. Authorization).
type webhookNotifier struct {
	url     string
	headers map[string]string
	client  *http.Client
}

func newWebhook(cfg Config) (*webhookNotifier, error) {
	if cfg.WebhookURL == "" {
		return nil, errors.New("notifier: webhook requires webhook_url")
	}
	return &webhookNotifier{
		url:     cfg.WebhookURL,
		headers: cfg.WebhookHeaders,
		client:  defaultHTTPClient,
	}, nil
}

func (w *webhookNotifier) Send(ctx context.Context, msg Message) error {
	payload := map[string]string{
		"title": msg.Title,
		"body":  msg.Body,
		"level": string(msg.Level),
	}
	return postJSON(ctx, w.client, w.url, payload, w.headers)
}

// --- telegram ---

// Ref tracks the last Telegram message posted to a route (chat_id), so a
// follow-up within the edit window reuses it via editMessageText instead of
// posting a brand-new message. A newly sent message carries its message_id and
// updates the Ref.
type Ref struct {
	ChatID    string    `json:"chat_id"`
	MessageID int64     `json:"message_id"`
	At        time.Time `json:"at"`
}

type telegramNotifier struct {
	token      string
	chatID     string
	baseURL    string
	editWindow time.Duration
	client     *http.Client
	now        func() time.Time

	mu  sync.Mutex
	ref Ref
}

func newTelegram(cfg Config) (*telegramNotifier, error) {
	if cfg.TelegramToken == "" {
		return nil, errors.New("notifier: telegram requires token")
	}
	if cfg.TelegramChatID == "" {
		return nil, errors.New("notifier: telegram requires chat_id")
	}
	base := strings.TrimRight(cfg.TelegramBaseURL, "/")
	if base == "" {
		base = defaultTelegramBaseURL
	}
	return &telegramNotifier{
		token:      cfg.TelegramToken,
		chatID:     cfg.TelegramChatID,
		baseURL:    base,
		editWindow: defaultTelegramEditWin,
		client:     defaultHTTPClient,
		now:        time.Now,
	}, nil
}

func (t *telegramNotifier) Send(ctx context.Context, msg Message) error {
	text := msg.String()

	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	if t.ref.MessageID != 0 && t.ref.ChatID == t.chatID && now.Sub(t.ref.At) < t.editWindow {
		id, err := t.editMessageText(ctx, t.ref.MessageID, text)
		if err != nil {
			return err
		}
		t.ref.MessageID = id
		t.ref.At = now
		return nil
	}

	id, err := t.sendMessage(ctx, text)
	if err != nil {
		return err
	}
	t.ref = Ref{ChatID: t.chatID, MessageID: id, At: now}
	return nil
}

func (t *telegramNotifier) sendMessage(ctx context.Context, text string) (int64, error) {
	target := t.baseURL + "/bot" + t.token + "/sendMessage"
	return t.call(ctx, target, map[string]any{"chat_id": t.chatID, "text": text})
}

func (t *telegramNotifier) editMessageText(ctx context.Context, messageID int64, text string) (int64, error) {
	target := t.baseURL + "/bot" + t.token + "/editMessageText"
	return t.call(ctx, target, map[string]any{
		"chat_id":    t.chatID,
		"message_id": messageID,
		"text":       text,
	})
}

func (t *telegramNotifier) call(ctx context.Context, target string, payload any) (int64, error) {
	body, err := doJSON(ctx, t.client, target, payload, nil)
	if err != nil {
		return 0, err
	}
	var resp struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("notifier: telegram: decode response: %w", err)
	}
	if !resp.OK {
		return 0, fmt.Errorf("notifier: telegram: not ok: %s", string(body))
	}
	return resp.Result.MessageID, nil
}

// --- smtp ---

type smtpNotifier struct {
	host string
	port int
	user string
	pass string
	from string
	to   []string
}

func newSMTP(cfg Config) (*smtpNotifier, error) {
	if cfg.SMTPHost == "" {
		return nil, errors.New("notifier: smtp requires host")
	}
	if cfg.SMTPFrom == "" {
		return nil, errors.New("notifier: smtp requires from")
	}
	if len(cfg.SMTPTo) == 0 {
		return nil, errors.New("notifier: smtp requires at least one recipient")
	}
	port := cfg.SMTPPort
	if port == 0 {
		port = defaultSMTPPort
	}
	return &smtpNotifier{
		host: cfg.SMTPHost,
		port: port,
		user: cfg.SMTPUser,
		pass: cfg.SMTPPass,
		from: cfg.SMTPFrom,
		to:   cfg.SMTPTo,
	}, nil
}

func (s *smtpNotifier) Send(ctx context.Context, msg Message) error {
	addr := net.JoinHostPort(s.host, strconv.Itoa(s.port))
	content := buildSMTPMessage(s.from, s.to, msg)

	var auth smtp.Auth
	if s.user != "" {
		auth = smtp.PlainAuth("", s.user, s.pass, s.host)
	}
	// smtp.SendMail has no context variant; the caller's ctx is only used for
	// cancellation upstream.
	return smtp.SendMail(addr, auth, s.from, s.to, content)
}

// buildSMTPMessage assembles a minimal RFC 5322 message. It is a plain function
// so the construction can be unit-tested without touching the network.
func buildSMTPMessage(from string, to []string, msg Message) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", msg.Title)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(msg.Body)
	b.WriteString("\r\n")
	return []byte(b.String())
}

// --- ntfy ---

type ntfyNotifier struct {
	url    string
	user   string
	pass   string
	client *http.Client
}

func newNtfy(cfg Config) (*ntfyNotifier, error) {
	u := cfg.NtfyURL
	if u == "" {
		if cfg.NtfyTopic == "" {
			return nil, errors.New("notifier: ntfy requires ntfy_url or ntfy_topic")
		}
		u = strings.TrimRight(defaultNtfyBaseURL, "/") + "/" + cfg.NtfyTopic
	}
	return &ntfyNotifier{
		url:    u,
		user:   cfg.NtfyUser,
		pass:   cfg.NtfyPass,
		client: defaultHTTPClient,
	}, nil
}

func (n *ntfyNotifier) Send(ctx context.Context, msg Message) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, strings.NewReader(msg.Body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", msg.Title)
	req.Header.Set("Priority", ntfyPriority(msg.Level))
	if n.user != "" {
		req.Header.Set("Authorization", "Basic "+basicAuth(n.user, n.pass))
	}
	_, err = doRequest(ctx, n.client, req)
	return err
}

func ntfyPriority(l Level) string {
	switch l {
	case LevelCritical:
		return "urgent"
	case LevelWarning:
		return "high"
	default:
		return "low"
	}
}

// --- gotify ---

type gotifyNotifier struct {
	url    string
	token  string
	client *http.Client
}

func newGotify(cfg Config) (*gotifyNotifier, error) {
	if cfg.GotifyURL == "" {
		return nil, errors.New("notifier: gotify requires url")
	}
	if cfg.GotifyToken == "" {
		return nil, errors.New("notifier: gotify requires token")
	}
	return &gotifyNotifier{
		url:    strings.TrimRight(cfg.GotifyURL, "/"),
		token:  cfg.GotifyToken,
		client: defaultHTTPClient,
	}, nil
}

func (g *gotifyNotifier) Send(ctx context.Context, msg Message) error {
	target := g.url + "/message?token=" + url.QueryEscape(g.token)
	payload := map[string]any{
		"title":    msg.Title,
		"message":  msg.Body,
		"priority": gotifyPriority(msg.Level),
	}
	return postJSON(ctx, g.client, target, payload, nil)
}

func gotifyPriority(l Level) int {
	switch l {
	case LevelCritical:
		return 10
	case LevelWarning:
		return 5
	default:
		return 1
	}
}

// --- serverchan ---

type serverChanNotifier struct {
	base   string
	key    string
	client *http.Client
}

func newServerChan(cfg Config) (*serverChanNotifier, error) {
	if cfg.ServerChanKey == "" {
		return nil, errors.New("notifier: serverchan requires key")
	}
	base := strings.TrimRight(cfg.ServerChanURL, "/")
	if base == "" {
		base = defaultServerChanURL
	}
	return &serverChanNotifier{
		base:   base,
		key:    cfg.ServerChanKey,
		client: defaultHTTPClient,
	}, nil
}

func (s *serverChanNotifier) Send(ctx context.Context, msg Message) error {
	target := s.base + "/" + s.key + ".send"
	form := url.Values{}
	form.Set("title", msg.Title)
	form.Set("desp", msg.Body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_, err = doRequest(ctx, s.client, req)
	return err
}

// --- pushplus ---

type pushPlusNotifier struct {
	url    string
	token  string
	client *http.Client
}

func newPushPlus(cfg Config) (*pushPlusNotifier, error) {
	if cfg.PushPlusToken == "" {
		return nil, errors.New("notifier: pushplus requires token")
	}
	u := cfg.PushPlusURL
	if u == "" {
		u = defaultPushPlusURL
	}
	return &pushPlusNotifier{
		url:    u,
		token:  cfg.PushPlusToken,
		client: defaultHTTPClient,
	}, nil
}

func (p *pushPlusNotifier) Send(ctx context.Context, msg Message) error {
	payload := map[string]string{
		"token":   p.token,
		"title":   msg.Title,
		"content": msg.Body,
	}
	return postJSON(ctx, p.client, p.url, payload, nil)
}
