package notifier

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- test doubles ---

// fakeClock is a manually advanced time source shared across router/breaker.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// recordingNotifier is an in-memory Notifier that captures every delivered
// message.
type recordingNotifier struct {
	mu    sync.Mutex
	calls []Message
	err   error
}

func (n *recordingNotifier) Send(_ context.Context, msg Message) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, msg)
	return n.err
}

func (n *recordingNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.calls)
}

func (n *recordingNotifier) last() Message {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.calls) == 0 {
		return Message{}
	}
	return n.calls[len(n.calls)-1]
}

func (n *recordingNotifier) messages() []Message {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]Message(nil), n.calls...)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// --- breaker ---

func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	b := NewBreaker()
	sentinel := errors.New("boom")

	for i := 0; i < 5; i++ {
		if err := b.Do(func() error { return sentinel }); err != sentinel {
			t.Fatalf("fail %d: err = %v, want sentinel", i, err)
		}
	}
	if got := b.State(); got != "open" {
		t.Fatalf("state = %q, want open", got)
	}

	called := false
	err := b.Do(func() error { called = true; return nil })
	if !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("err = %v, want ErrBreakerOpen", err)
	}
	if called {
		t.Fatal("fn should be skipped while open")
	}
	if got := b.SkipCount(); got != 1 {
		t.Fatalf("skip count = %d, want 1", got)
	}
}

func TestBreakerHalfOpenProbeSuccessCloses(t *testing.T) {
	clock := newFakeClock(time.Now())
	b := NewBreaker(WithBreakerClock(clock.now))
	sentinel := errors.New("boom")

	for i := 0; i < 5; i++ {
		_ = b.Do(func() error { return sentinel })
	}
	if b.State() != "open" {
		t.Fatalf("state = %q, want open", b.State())
	}

	clock.advance(defaultOpenDuration + time.Second)
	called := false
	if err := b.Do(func() error { called = true; return nil }); err != nil {
		t.Fatalf("probe err = %v, want nil", err)
	}
	if !called {
		t.Fatal("probe should execute in half-open")
	}
	if b.State() != "closed" {
		t.Fatalf("state = %q, want closed after successful probe", b.State())
	}
	if b.Failures() != 0 {
		t.Fatalf("failures = %d, want 0", b.Failures())
	}
}

func TestBreakerHalfOpenProbeFailureReopens(t *testing.T) {
	clock := newFakeClock(time.Now())
	b := NewBreaker(WithBreakerClock(clock.now))
	sentinel := errors.New("boom")

	for i := 0; i < 5; i++ {
		_ = b.Do(func() error { return sentinel })
	}
	clock.advance(defaultOpenDuration + time.Second)

	// Probe fails → re-open immediately.
	if err := b.Do(func() error { return sentinel }); err != sentinel {
		t.Fatalf("probe err = %v, want sentinel", err)
	}
	if b.State() != "open" {
		t.Fatalf("state = %q, want open after failed probe", b.State())
	}

	// Still within the fresh open window → skip.
	called := false
	if err := b.Do(func() error { called = true; return nil }); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("err = %v, want ErrBreakerOpen", err)
	}
	if called {
		t.Fatal("fn should be skipped after re-open")
	}
}

// TestBreakerConcurrentFnOutsideLock proves fn() is not serialized under the
// breaker lock: all n calls must enter fn concurrently. If Do still held the
// lock during fn(), the first goroutine would block on release while every
// other goroutine blocks on the mutex, and the timeout below would fire.
func TestBreakerConcurrentFnOutsideLock(t *testing.T) {
	b := NewBreaker()
	const n = 32
	entered := make(chan struct{}, n)
	release := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer doRelease()

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.Do(func() error {
				entered <- struct{}{}
				<-release
				return nil
			})
		}()
	}

	done := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			<-entered
		}
		doRelease()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Do() held the lock during fn(): calls did not run concurrently")
	}
	wg.Wait()
	if got := b.State(); got != "closed" {
		t.Fatalf("state = %q, want closed", got)
	}
}

// --- router ---

func TestRouterCriticalImmediate(t *testing.T) {
	n := &recordingNotifier{}
	r := NewRouter()
	r.Add("inst", n, LevelCritical, LevelWarning, LevelInfo)

	if err := r.Notify(context.Background(), LevelCritical, Message{Title: "t", Body: "b"}); err != nil {
		t.Fatalf("Notify err = %v", err)
	}
	if n.count() != 1 {
		t.Fatalf("critical calls = %d, want 1 (immediate)", n.count())
	}
}

func TestRouterWarningAggregation(t *testing.T) {
	n := &recordingNotifier{}
	r := NewRouter()
	r.Add("inst", n, LevelWarning)

	ctx := context.Background()
	_ = r.Notify(ctx, LevelWarning, Message{Title: "w1", Body: "first"})
	_ = r.Notify(ctx, LevelWarning, Message{Title: "w2", Body: "second"})

	if n.count() != 0 {
		t.Fatalf("calls before flush = %d, want 0 (buffered)", n.count())
	}

	r.Flush(ctx)
	if n.count() != 1 {
		t.Fatalf("calls after flush = %d, want 1 (aggregated)", n.count())
	}
	got := n.last()
	if got.Title != "2 条 warning" {
		t.Fatalf("aggregate title = %q, want %q", got.Title, "2 条 warning")
	}
	if !strings.Contains(got.Body, "first") || !strings.Contains(got.Body, "second") {
		t.Fatalf("aggregate body = %q, want both events", got.Body)
	}
}

func TestRouterAggregatesPerEvent(t *testing.T) {
	n := &recordingNotifier{}
	r := NewRouter()
	r.Add("inst", n, LevelWarning)

	ctx := context.Background()
	_ = r.Notify(ctx, LevelWarning, Message{Title: "d1", Body: "disk one", Event: "disk"})
	_ = r.Notify(ctx, LevelWarning, Message{Title: "s1", Body: "slow one", Event: "low_speed"})
	_ = r.Notify(ctx, LevelWarning, Message{Title: "d2", Body: "disk two", Event: "disk"})

	r.Flush(ctx)
	if n.count() != 2 {
		t.Fatalf("calls after flush = %d, want 2 (one per event)", n.count())
	}

	disk, slow := 0, 0
	for _, m := range n.messages() {
		switch m.Event {
		case "disk":
			disk++
			if m.Title != "2 条 warning" || !strings.Contains(m.Body, "disk one") || !strings.Contains(m.Body, "disk two") {
				t.Fatalf("disk aggregate = %+v, want 2 lines titled %q", m, "2 条 warning")
			}
		case "low_speed":
			slow++
			if m.Title != "1 条 warning" || !strings.Contains(m.Body, "slow one") {
				t.Fatalf("low_speed aggregate = %+v, want 1 line titled %q", m, "1 条 warning")
			}
		default:
			t.Fatalf("unexpected aggregate event %q", m.Event)
		}
	}
	if disk != 1 || slow != 1 {
		t.Fatalf("disk aggregates=%d slow aggregates=%d, want 1 each", disk, slow)
	}
}

func TestRouterWindowExpiryFlushes(t *testing.T) {
	clock := newFakeClock(time.Now())
	n := &recordingNotifier{}
	r := NewRouter(WithRouterClock(clock.now))
	r.Add("inst", n, LevelWarning)

	_ = r.Notify(context.Background(), LevelWarning, Message{Body: "one"})
	if n.count() != 0 {
		t.Fatalf("calls = %d, want 0 before expiry", n.count())
	}

	clock.advance(defaultAggregationWindow + time.Second)
	r.flushExpired(context.Background())

	if n.count() != 1 {
		t.Fatalf("calls after expiry = %d, want 1", n.count())
	}
	if got := n.last().Title; got != "1 条 warning" {
		t.Fatalf("title = %q, want %q", got, "1 条 warning")
	}
}

func TestRouterRoutingMatrix(t *testing.T) {
	a := &recordingNotifier{}
	b := &recordingNotifier{}
	r := NewRouter()
	r.Add("a", a, LevelCritical, LevelWarning)
	r.Add("b", b, LevelInfo)

	ctx := context.Background()

	if err := r.Notify(ctx, LevelCritical, Message{Body: "c"}); err != nil {
		t.Fatalf("critical err = %v", err)
	}
	if a.count() != 1 || b.count() != 0 {
		t.Fatalf("critical routing: a=%d b=%d, want a=1 b=0", a.count(), b.count())
	}

	_ = r.Notify(ctx, LevelWarning, Message{Body: "w"})
	r.Flush(ctx)
	if a.count() != 2 || b.count() != 0 {
		t.Fatalf("warning routing: a=%d b=%d, want a=2 b=0", a.count(), b.count())
	}

	_ = r.Notify(ctx, LevelInfo, Message{Body: "i"})
	r.Flush(ctx)
	if a.count() != 2 || b.count() != 1 {
		t.Fatalf("info routing: a=%d b=%d, want a=2 b=1", a.count(), b.count())
	}
}

func TestAllOffline(t *testing.T) {
	var reqCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reqCount.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n, err := New(Config{Type: TypeWebhook, WebhookURL: srv.URL})
	if err != nil {
		t.Fatalf("New webhook: %v", err)
	}
	r := NewRouter()
	r.Add("wh", n, LevelCritical)

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_ = r.Notify(ctx, LevelCritical, Message{Body: "x"})
	}
	if got := reqCount.Load(); got != 5 {
		t.Fatalf("requests after 5 failures = %d, want 5", got)
	}

	// The 6th call should be skipped by the open breaker.
	err = r.Notify(ctx, LevelCritical, Message{Body: "x"})
	if !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("6th Notify err = %v, want ErrBreakerOpen", err)
	}
	if got := reqCount.Load(); got != 5 {
		t.Fatalf("requests after skip = %d, want 5 (unchanged)", got)
	}
}

// TestRouterStartIdempotentAndStop verifies Start is idempotent (a second
// call does not launch another flusher) and Stop cancels and waits for the
// flusher goroutine.
func TestRouterStartIdempotentAndStop(t *testing.T) {
	n := &recordingNotifier{}
	r := NewRouter()
	r.Add("inst", n, LevelWarning)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r.Start(ctx, time.Millisecond)
	r.Start(ctx, time.Millisecond) // idempotent: must not double-open

	r.mu.Lock()
	started := r.started
	r.mu.Unlock()
	if !started {
		t.Fatal("router not marked started after Start")
	}

	r.Stop() // waits for the flusher goroutine to exit

	r.mu.Lock()
	started = r.started
	r.mu.Unlock()
	if started {
		t.Fatal("router still marked started after Stop")
	}
}

// --- providers (httptest fake endpoints) ---
func TestWebhookProvider(t *testing.T) {
	type captured struct {
		title, body, level, auth string
	}
	var got captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]string
		_ = json.NewDecoder(r.Body).Decode(&p)
		got = captured{title: p["title"], body: p["body"], level: p["level"], auth: r.Header.Get("Authorization")}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, err := New(Config{
		Type:           TypeWebhook,
		WebhookURL:     srv.URL,
		WebhookHeaders: map[string]string{"Authorization": "Bearer t"},
	})
	if err != nil {
		t.Fatalf("New webhook: %v", err)
	}
	if err := n.Send(context.Background(), Message{Title: "T", Body: "B", Level: LevelCritical}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.title != "T" || got.body != "B" || got.level != "critical" || got.auth != "Bearer t" {
		t.Fatalf("captured = %+v", got)
	}
}

func TestNtfyProvider(t *testing.T) {
	var body, title, priority string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		title = r.Header.Get("Title")
		priority = r.Header.Get("Priority")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, err := New(Config{Type: TypeNtfy, NtfyURL: srv.URL + "/mytopic"})
	if err != nil {
		t.Fatalf("New ntfy: %v", err)
	}
	if err := n.Send(context.Background(), Message{Title: "T", Body: "B", Level: LevelCritical}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if body != "B" || title != "T" || priority != "urgent" {
		t.Fatalf("body=%q title=%q priority=%q", body, title, priority)
	}
}

func TestGotifyProvider(t *testing.T) {
	var token string
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token = r.URL.Query().Get("token")
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, err := New(Config{Type: TypeGotify, GotifyURL: srv.URL, GotifyToken: "SECRET"})
	if err != nil {
		t.Fatalf("New gotify: %v", err)
	}
	if err := n.Send(context.Background(), Message{Title: "T", Body: "B", Level: LevelWarning}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if token != "SECRET" {
		t.Fatalf("token = %q, want SECRET", token)
	}
	if payload["title"] != "T" || payload["message"] != "B" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestServerChanProvider(t *testing.T) {
	var title, desp string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		title = r.Form.Get("title")
		desp = r.Form.Get("desp")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, err := New(Config{Type: TypeServerChan, ServerChanURL: srv.URL, ServerChanKey: "SCTKEY"})
	if err != nil {
		t.Fatalf("New serverchan: %v", err)
	}
	if err := n.Send(context.Background(), Message{Title: "T", Body: "B", Level: LevelInfo}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if title != "T" || desp != "B" {
		t.Fatalf("title=%q desp=%q", title, desp)
	}
}

func TestPushPlusProvider(t *testing.T) {
	var p map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&p)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, err := New(Config{Type: TypePushPlus, PushPlusURL: srv.URL, PushPlusToken: "PPTOKEN"})
	if err != nil {
		t.Fatalf("New pushplus: %v", err)
	}
	if err := n.Send(context.Background(), Message{Title: "T", Body: "B", Level: LevelInfo}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if p["token"] != "PPTOKEN" || p["title"] != "T" || p["content"] != "B" {
		t.Fatalf("payload = %+v", p)
	}
}

func TestTelegramEditReuse(t *testing.T) {
	var sendCount, editCount atomic.Int64
	var nextID atomic.Int64
	var lastEditedID atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/botTOKEN/sendMessage", func(w http.ResponseWriter, _ *http.Request) {
		sendCount.Add(1)
		writeJSON(w, map[string]any{"ok": true, "result": map[string]any{"message_id": nextID.Add(1)}})
	})
	mux.HandleFunc("/botTOKEN/editMessageText", func(w http.ResponseWriter, r *http.Request) {
		editCount.Add(1)
		var req struct {
			MessageID int64 `json:"message_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		lastEditedID.Store(req.MessageID)
		writeJSON(w, map[string]any{"ok": true, "result": map[string]any{"message_id": req.MessageID}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	n, err := New(Config{
		Type:            TypeTelegram,
		TelegramToken:   "TOKEN",
		TelegramChatID:  "123",
		TelegramBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("New telegram: %v", err)
	}
	tg := n.(*telegramNotifier)
	clock := newFakeClock(time.Now())
	tg.now = clock.now
	tg.editWindow = time.Minute

	ctx := context.Background()

	// First message → new sendMessage, ref updated to message_id 1.
	if err := tg.Send(ctx, Message{Title: "A", Body: "1"}); err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	// Within window → edit the same message.
	clock.advance(30 * time.Second)
	if err := tg.Send(ctx, Message{Title: "A", Body: "2"}); err != nil {
		t.Fatalf("Send 2: %v", err)
	}
	// Past window → new sendMessage (message_id 2).
	clock.advance(2 * time.Minute)
	if err := tg.Send(ctx, Message{Title: "A", Body: "3"}); err != nil {
		t.Fatalf("Send 3: %v", err)
	}
	// Within window again → edit message_id 2.
	clock.advance(10 * time.Second)
	if err := tg.Send(ctx, Message{Title: "A", Body: "4"}); err != nil {
		t.Fatalf("Send 4: %v", err)
	}

	if got := sendCount.Load(); got != 2 {
		t.Fatalf("sendMessage calls = %d, want 2", got)
	}
	if got := editCount.Load(); got != 2 {
		t.Fatalf("editMessageText calls = %d, want 2", got)
	}
	if got := lastEditedID.Load(); got != 2 {
		t.Fatalf("last edited message_id = %d, want 2", got)
	}
}

// --- smtp (message construction only) ---

func TestSMTPMessageBuild(t *testing.T) {
	raw := string(buildSMTPMessage("from@example.com", []string{"a@example.com", "b@example.com"},
		Message{Title: "Subject 行", Body: "邮件正文"}))

	for _, want := range []string{
		"From: from@example.com",
		"To: a@example.com, b@example.com",
		"Subject: Subject 行",
		"Content-Type: text/plain; charset=UTF-8",
		"邮件正文",
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("message missing %q\nraw:\n%s", want, raw)
		}
	}
}
