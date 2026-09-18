// Package occlient یک کلاینت سبک HTTP برای REST API سرور opencode است.
// نوع‌های داده‌ای این بسته دقیقاً ساختار JSON خود opencode هستند.
package occlient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Session یک نشست (گفتگو) روی سرور opencode است.
type Session struct {
	ID        string  `json:"id"`
	Slug      string  `json:"slug"`
	Title     string  `json:"title"`
	Directory string  `json:"directory"`
	Cost      float64 `json:"cost"`
	Tokens    struct {
		Input  int `json:"input"`
		Output int `json:"output"`
	} `json:"tokens"`
	Summary struct {
		Files int `json:"files"`
	} `json:"summary"`
	ModelID string `json:"modelID"`
	Model   struct {
		ID         string `json:"id"`
		ProviderID string `json:"providerID"`
	} `json:"model"`
	Time struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
}

// Part یک بخش از پیام است: متن، reasoning یا ابزار.
type Part struct {
	Type  string     `json:"type"`
	Text  string     `json:"text"`
	Tool  string     `json:"tool"`
	State *PartState `json:"state"`
}

// PartState وضعیت اجرای یک ابزار است.
type PartState struct {
	Status string         `json:"status"`
	Title  string         `json:"title"`
	Input  map[string]any `json:"input"`
}

// Message یک پیام کامل (اطلاعات + بخش‌ها) در نشست است.
type Message struct {
	Info struct {
		ID         string `json:"id"`
		Role       string `json:"role"`
		SessionID  string `json:"sessionID"`
		ModelID    string `json:"modelID"`
		ProviderID string `json:"providerID"`
		Finish     string `json:"finish"`
	} `json:"info"`
	Parts []Part `json:"parts"`
}

// QuestionOption یک گزینهٔ قابل انتخاب در سؤال تعاملی است.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// QuestionInfo یک سؤالِ مطرح‌شده توسط مدل است (مثل سؤال CLI خود opencode).
type QuestionInfo struct {
	Question string           `json:"question"`
	Header   string           `json:"header"`
	Options  []QuestionOption `json:"options"`
	Multiple bool             `json:"multiple,omitempty"`
	Custom   *bool            `json:"custom,omitempty"` // nil یعنی true (پاسخ آزاد مجاز)
}

// QuestionRequest درخواست سؤالِ در انتظار پاسخ برای یک نشست است.
type QuestionRequest struct {
	ID        string         `json:"id"`
	SessionID string         `json:"sessionID"`
	Questions []QuestionInfo `json:"questions"`
	Tool      *struct {
		MessageID string `json:"messageID"`
		CallID    string `json:"callID"`
	} `json:"tool,omitempty"`
}

// Client یک کلاینت HTTP برای سرور opencode است.
type Client struct {
	base   string
	agent  string
	client *http.Client
	long   *http.Client // برای عملیات طولانی مثل فشرده‌سازی (summarize)
}

func New(base, agent string) *Client {
	return &Client{
		base:  strings.TrimRight(base, "/"),
		agent: agent,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		long: &http.Client{
			Timeout: 10 * time.Minute,
		},
	}
}

func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any) error {
	return c.doJSONWith(ctx, c.client, method, path, body, out)
}

func (c *Client) doJSONWith(ctx context.Context, hc *http.Client, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return fmt.Errorf("opencode %s %s: %s %s", method, path, resp.Status, string(b))
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) CreateSession(ctx context.Context) (*Session, error) {
	var s Session
	err := c.doJSON(ctx, http.MethodPost, "/session", map[string]any{}, &s)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *Client) ListSessions(ctx context.Context, limit int) ([]Session, error) {
	if limit <= 0 {
		limit = 100
	}
	var list []Session
	// limit سمت سرور اعمال می‌شود؛ سرور به‌ترتیب «آخرین فعالیت» برمی‌گرداند
	if err := c.doJSON(ctx, http.MethodGet, "/session?limit="+strconv.Itoa(limit), nil, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// DeleteSession نشست و تمام داده‌هایش را روی سرور حذف می‌کند
func (c *Client) DeleteSession(ctx context.Context, id string) error {
	err := c.doJSON(ctx, http.MethodDelete, "/session/"+url.PathEscape(id), nil, nil)
	if err != nil && strings.Contains(err.Error(), "404") {
		// از قبل حذف شده؛ اشکالی ندارد
		return nil
	}
	return err
}

func (c *Client) GetSession(ctx context.Context, id string) (*Session, error) {
	var s Session
	if err := c.doJSON(ctx, http.MethodGet, "/session/"+url.PathEscape(id), nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *Client) PromptAsync(ctx context.Context, sessionID, prompt, agent string) error {
	if agent == "" {
		agent = c.agent
	}
	body := map[string]any{
		"agent": agent,
		"parts": []map[string]any{{"type": "text", "text": prompt}},
	}
	return c.doJSON(ctx, http.MethodPost,
		"/session/"+url.PathEscape(sessionID)+"/prompt_async", body, nil)
}

func (c *Client) LastMessage(ctx context.Context, sessionID string) (*Message, error) {
	var list []Message
	err := c.doJSON(ctx, http.MethodGet,
		"/session/"+url.PathEscape(sessionID)+"/message?limit=1", nil, &list)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	return &list[0], nil
}

// ListMessages فهرست پیام‌های نشست (قدیمی→جدید)؛ limit تعداد پیام‌های آخر
func (c *Client) ListMessages(ctx context.Context, sessionID string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 100
	}
	var list []Message
	err := c.doJSON(ctx, http.MethodGet,
		"/session/"+url.PathEscape(sessionID)+"/message?limit="+strconv.Itoa(limit), nil, &list)
	if err != nil {
		return nil, err
	}
	return list, nil
}

func (c *Client) Abort(ctx context.Context, sessionID string) error {
	return c.doJSON(ctx, http.MethodPost,
		"/session/"+url.PathEscape(sessionID)+"/abort", nil, nil)
}

// Summarize نشست را فشرده می‌کند (خلاصه‌سازی تاریخچه برای کاهش مصرف توکن).
// چون خودِ خلاصه‌سازی یک فراخوانی LLM است ممکن است طول بکشد؛ از کلاینت
// بلندمدت استفاده می‌کند و مهلت را context تعیین می‌کند.
func (c *Client) Summarize(ctx context.Context, sessionID, providerID, modelID string) error {
	if providerID == "" || modelID == "" {
		return fmt.Errorf("summarize: providerID و modelID لازم است")
	}
	body := map[string]any{"providerID": providerID, "modelID": modelID}
	return c.doJSONWith(ctx, c.long, http.MethodPost,
		"/session/"+url.PathEscape(sessionID)+"/summarize", body, nil)
}

func (c *Client) ListQuestions(ctx context.Context) ([]QuestionRequest, error) {
	var list []QuestionRequest
	if err := c.doJSON(ctx, http.MethodGet, "/question", nil, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (c *Client) ReplyQuestion(ctx context.Context, requestID string, answers [][]string) error {
	body := map[string]any{"answers": answers}
	return c.doJSON(ctx, http.MethodPost,
		"/question/"+url.PathEscape(requestID)+"/reply", body, nil)
}

func (c *Client) RejectQuestion(ctx context.Context, requestID string) error {
	return c.doJSON(ctx, http.MethodPost,
		"/question/"+url.PathEscape(requestID)+"/reject", nil, nil)
}
