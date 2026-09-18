package occlient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// startTestServer سرور HTTP آزمایشی می‌سازد که هر درخواست را با handler پاسخ می‌دهد.
func startTestServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, New(srv.URL, "build")
}

func TestListSessions(t *testing.T) {
	srv, c := startTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method=%s", r.Method)
		}
		if r.URL.Path != "/session" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.URL.Query().Get("limit") != "8" {
			t.Fatalf("limit=%q", r.URL.Query().Get("limit"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]Session{
			{ID: "ses_1", Cost: 0.5, ModelID: "deepseek/deepseek-v4-flash"},
			{ID: "ses_2", Cost: 1.25},
		})
	})
	list, err := c.ListSessions(context.Background(), 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "ses_1" || list[0].Cost != 0.5 {
		t.Fatalf("unexpected list: %+v", list)
	}
	_ = srv
}

func TestCreateAndPromptAsync(t *testing.T) {
	got := ""
	srv, c := startTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			json.NewEncoder(w).Encode(Session{ID: "ses_new", ModelID: "m"})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/session/ses_new/prompt_async"):
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			got = body["agent"].(string)
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	ctx := context.Background()
	s, err := c.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "ses_new" {
		t.Fatalf("id=%q", s.ID)
	}
	if err := c.PromptAsync(ctx, s.ID, "hi", ""); err != nil {
		t.Fatal(err)
	}
	if got != "build" {
		t.Fatalf("default agent=%q", got)
	}
	if err := c.PromptAsync(ctx, s.ID, "hi", "plan"); err != nil {
		t.Fatal(err)
	}
	if got != "plan" {
		t.Fatalf("agent=%q", got)
	}
	_ = srv
}

func TestLastMessage(t *testing.T) {
	srv, c := startTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"info":{"id":"msg1","role":"assistant","sessionID":"ses_1","modelID":"m","finish":"done"},"parts":[{"type":"text","text":"سلام"}]}]`)
	})
	msg, err := c.LastMessage(context.Background(), "ses_1")
	if err != nil {
		t.Fatal(err)
	}
	if msg == nil || msg.Info.Role != "assistant" || msg.Parts[0].Text != "سلام" {
		t.Fatalf("unexpected msg: %+v", msg)
	}
	_ = srv
}

func TestServerErrorSurfaced(t *testing.T) {
	_, c := startTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if _, err := c.ListSessions(context.Background(), 10); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected server error, got %v", err)
	}
}

func TestDeleteSession404IsOK(t *testing.T) {
	_, c := startTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("method=%s", r.Method)
		}
		http.Error(w, "not found", http.StatusNotFound)
	})
	if err := c.DeleteSession(context.Background(), "ses_gone"); err != nil {
		t.Fatalf("404 should be swallowed, got %v", err)
	}
}

func TestSummarize(t *testing.T) {
	var gotProvider, gotModel string
	_, c := startTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/session/ses_1/summarize" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		gotProvider, _ = body["providerID"].(string)
		gotModel, _ = body["modelID"].(string)
		io.WriteString(w, `true`)
	})
	if err := c.Summarize(context.Background(), "ses_1", "deepseek", "deepseek-flash"); err != nil {
		t.Fatal(err)
	}
	if gotProvider != "deepseek" || gotModel != "deepseek-flash" {
		t.Fatalf("body provider=%q model=%q", gotProvider, gotModel)
	}
	if err := c.Summarize(context.Background(), "ses_1", "", ""); err == nil {
		t.Fatal("expected error when provider/model missing")
	}
}
