package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"negahban-opencode/internal/occlient"
)

// fakeTG پیاده‌سازی تستی از telegramAPI که همهٔ پیام‌های ارسالی را ضبط می‌کند.
type fakeTG struct {
	mu   sync.Mutex
	sent []string
	n    int
}

func (f *fakeTG) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	if t := chatText(c); t != "" {
		f.sent = append(f.sent, t)
	}
	return tgbotapi.Message{MessageID: f.n}, nil
}

func (f *fakeTG) Request(c tgbotapi.Chattable) (*tgbotapi.APIResponse, error) {
	return &tgbotapi.APIResponse{Ok: true}, nil
}

func (f *fakeTG) GetFile(c tgbotapi.FileConfig) (tgbotapi.File, error) {
	return tgbotapi.File{}, nil
}

func (f *fakeTG) hasText(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sent {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// chatText متن یک پیام قابل ارسال به تلگرام را درمی‌آورد.
func chatText(c tgbotapi.Chattable) string {
	switch v := c.(type) {
	case tgbotapi.MessageConfig:
		return v.Text
	case tgbotapi.EditMessageTextConfig:
		return v.Text
	}
	return ""
}

// fakeOC پیاده‌سازی تستی از ocAPI است.
type fakeOC struct {
	created int
	session *occlient.Session
	sessErr error
}

func (f *fakeOC) CreateSession(ctx context.Context) (*occlient.Session, error) {
	f.created++
	return &occlient.Session{ID: fmt.Sprintf("ses_%d", f.created)}, nil
}
func (f *fakeOC) ListSessions(ctx context.Context, limit int) ([]occlient.Session, error) {
	return nil, nil
}
func (f *fakeOC) DeleteSession(ctx context.Context, id string) error { return nil }
func (f *fakeOC) GetSession(ctx context.Context, id string) (*occlient.Session, error) {
	if f.session != nil || f.sessErr != nil {
		return f.session, f.sessErr
	}
	return nil, errFake
}
func (f *fakeOC) PromptAsync(ctx context.Context, sessionID, prompt, agent string) error {
	return nil
}
func (f *fakeOC) LastMessage(ctx context.Context, sessionID string) (*occlient.Message, error) {
	return nil, nil
}
func (f *fakeOC) ListMessages(ctx context.Context, sessionID string, limit int) ([]occlient.Message, error) {
	return nil, nil
}
func (f *fakeOC) Abort(ctx context.Context, sessionID string) error { return nil }
func (f *fakeOC) Summarize(ctx context.Context, sessionID, providerID, modelID string) error {
	return nil
}
func (f *fakeOC) ListQuestions(ctx context.Context) ([]occlient.QuestionRequest, error) {
	return nil, nil
}
func (f *fakeOC) ReplyQuestion(ctx context.Context, requestID string, answers [][]string) error {
	return nil
}
func (f *fakeOC) RejectQuestion(ctx context.Context, requestID string) error { return nil }

var _ ocAPI = (*fakeOC)(nil)

var errFake = errors.New("fake error")

func testBot(t *testing.T, allowed int64) (*Bot, *fakeTG) {
	t.Helper()
	cfg := &Config{
		BaseURL:   "http://127.0.0.1:1",
		Agent:     "build",
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Allowed:   map[int64]bool{allowed: true},
	}
	tg := &fakeTG{}
	b := newBot(cfg, tg)
	b.oc = &fakeOC{}
	t.Cleanup(func() { b.users.Close() })
	return b, tg
}

func updateFrom(userID, chatID int64, text string) tgbotapi.Update {
	return tgbotapi.Update{
		Message: &tgbotapi.Message{
			MessageID: 1,
			Text:      text,
			From:      &tgbotapi.User{ID: userID, UserName: "u"},
			Chat:      &tgbotapi.Chat{ID: chatID},
		},
	}
}

func TestUnauthorizedUserBlocked(t *testing.T) {
	b, tg := testBot(t, 1)
	b.Handle(updateFrom(2, 10, "سلام"))
	if !tg.hasText("مجاز") {
		t.Fatalf("unauthorized user should be rejected; sent=%v", tg.sent)
	}
	if b.users.byID(2) != nil {
		t.Fatalf("blocked user must not create state")
	}
}

func TestNewSessionCommand(t *testing.T) {
	b, tg := testBot(t, 1)
	b.Handle(updateFrom(1, 10, "/new"))
	if !tg.hasText("نشست جدید ساخته و فعال شد") || !tg.hasText("ses_1") {
		t.Fatalf("expected new session message; sent=%v", tg.sent)
	}
	if sid := b.users.byID(1).SessionID; sid != "ses_1" {
		t.Fatalf("active session=%q", sid)
	}
}

func TestUnknownCommandHelp(t *testing.T) {
	b, tg := testBot(t, 1)
	b.Handle(updateFrom(1, 10, "/unknownxyz"))
	if !tg.hasText("دستور ناشناخته") {
		t.Fatalf("expected unknown-command hint; sent=%v", tg.sent)
	}
}
