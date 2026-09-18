package main

import (
	"testing"
)

func TestFaNum(t *testing.T) {
	cases := map[int]string{0: "۰", 1: "۱", 9: "۹", 12: "۱۲", 100: "۱۰۰"}
	for in, want := range cases {
		if got := faNum(in); got != want {
			t.Fatalf("faNum(%d)=%q want %q", in, got, want)
		}
	}
}

func TestParseSessionNumber(t *testing.T) {
	cases := map[string]int{"۳": 3, "12": 12, " ۱٢ ": 12, "۱۰": 10}
	for in, want := range cases {
		got, ok := parseSessionNumber(in)
		if !ok || got != want {
			t.Fatalf("parseSessionNumber(%q)=(%d,%v) want %d", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "الف", "۲x", "-۱"} {
		if _, ok := parseSessionNumber(bad); ok {
			t.Fatalf("parseSessionNumber(%q) should fail", bad)
		}
	}
}

func TestParseSessionNumbers(t *testing.T) {
	got := parseSessionNumbers("۱، ۳ و ۵")
	want := []int{1, 3, 5}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	if len(parseSessionNumbers("بدون عدد")) != 0 {
		t.Fatal("expected no numbers")
	}
}

func TestRenumberAutoLabels(t *testing.T) {
	st := &UserState{
		Sessions: []string{"a", "b", "c"},
		Labels:   map[string]string{"a": "قدیمی", "b": "نام دستی", "c": ""},
		Manual:   map[string]bool{"b": true},
	}
	renumberAutoLabels(st)
	if st.Labels["a"] != "۱" {
		t.Fatalf("a=%q", st.Labels["a"])
	}
	if st.Labels["b"] != "نام دستی" {
		t.Fatalf("b=%q", st.Labels["b"])
	}
	if st.Labels["c"] != "۳" {
		t.Fatalf("c=%q", st.Labels["c"])
	}
}

func TestRenumberAfterDelete(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{BaseURL: "http://127.0.0.1:1", StateFile: dir + "/state.json"}
	b := newBot(cfg, nil)
	b.users.states[1] = &UserState{
		UserID:    1,
		Sessions:  []string{"a", "b", "c"},
		Labels:    map[string]string{"a": "۱", "b": "۲", "c": "۳"},
		SessionID: "b",
	}
	b.deleteSession(1, "b")
	st := b.users.states[1]
	if st.Labels["a"] != "۱" || st.Labels["c"] != "۲" {
		t.Fatalf("labels=%v", st.Labels)
	}
	if st.SessionID != "" {
		t.Fatalf("active should be cleared")
	}
}

func TestDeactivateKeepsSession(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{BaseURL: "http://127.0.0.1:1", StateFile: dir + "/state.json"}
	b := newBot(cfg, nil)
	b.users.states[1] = &UserState{
		UserID:    1,
		Sessions:  []string{"a", "b"},
		Labels:    map[string]string{"a": "۱", "b": "۲"},
		SessionID: "b",
	}
	b.users.deactivate(1, "b")
	st := b.users.states[1]
	if st.SessionID != "" {
		t.Fatalf("active should be cleared, got %q", st.SessionID)
	}
	if len(st.Sessions) != 2 {
		t.Fatalf("sessions should be kept, got %v", st.Sessions)
	}
}

func TestSetSessionLabelMarksManual(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{BaseURL: "http://127.0.0.1:1", StateFile: dir + "/state.json"}
	b := newBot(cfg, nil)
	b.users.states[1] = &UserState{UserID: 1, Sessions: []string{"a"}}
	b.setSessionLabel(1, "a", "مدل جدید")
	st := b.users.states[1]
	if st.Labels["a"] != "مدل جدید" || !st.Manual["a"] {
		t.Fatalf("labels=%v manual=%v", st.Labels, st.Manual)
	}
	// شماره‌گذاری خودکار نباید روی نام دستی بنشیند
	renumberAutoLabels(st)
	if st.Labels["a"] != "مدل جدید" {
		t.Fatalf("manual label overwritten: %q", st.Labels["a"])
	}
}

func TestPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.json"
	cfg := &Config{BaseURL: "http://127.0.0.1:1", StateFile: path}
	b := newBot(cfg, nil)
	b.users.states[1] = &UserState{
		UserID:    1,
		ChatID:    99,
		Sessions:  []string{"a", "b"},
		Labels:    map[string]string{"a": "۱", "b": "نام"},
		Manual:    map[string]bool{"b": true},
		SessionID: "b",
	}
	b.users.saver.MarkDirty()
	if err := b.users.Close(); err != nil {
		t.Fatal(err)
	}
	// بازخوانی از دیسک
	b2 := newBot(&Config{BaseURL: "http://127.0.0.1:1", StateFile: path}, nil)
	if err := b2.users.load(); err != nil {
		t.Fatal(err)
	}
	st := b2.users.byID(1)
	if st == nil || st.SessionID != "b" || len(st.Sessions) != 2 {
		t.Fatalf("roundtrip failed: %+v", st)
	}
	if st.Labels["b"] != "نام" || !st.Manual["b"] {
		t.Fatalf("labels lost: %+v", st)
	}
	b2.users.Close()
}
