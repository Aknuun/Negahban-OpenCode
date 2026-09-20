package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkScratchOCEnv(t *testing.T) *ocEnv {
	t.Helper()
	base := t.TempDir()
	cfgDir := filepath.Join(base, "config", "opencode")
	dataDir := filepath.Join(base, "data", "opencode")
	os.MkdirAll(cfgDir, 0o755)
	os.MkdirAll(dataDir, 0o755)
	e := &ocEnv{ConfigDir: cfgDir, DataDir: dataDir}
	return e
}

func TestSetModelAndRead(t *testing.T) {
	e := mkScratchOCEnv(t)
	start := "{\n  \"$schema\": \"https://opencode.ai/config.json\",\n  \"model\": \"deepseek/deepseek-v4-flash\",\n  \"small_model\": \"deepseek/deepseek-v4-flash\"\n}\n"
	if err := os.WriteFile(e.configPath(), []byte(start), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := e.currentModel(); got != "deepseek/deepseek-v4-flash" {
		t.Fatalf("currentModel=%q", got)
	}
	if err := e.setModel("anthropic/claude-sonnet-4-5"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(e.configPath())
	s := string(b)
	if !strings.Contains(s, `"model": "anthropic/claude-sonnet-4-5"`) {
		t.Fatalf("model not written:\n%s", s)
	}
	if !strings.Contains(s, `"small_model": "anthropic/claude-sonnet-4-5"`) {
		t.Fatalf("small_model not written:\n%s", s)
	}
	if got := e.currentModel(); got != "anthropic/claude-sonnet-4-5" {
		t.Fatalf("re-read=%q", got)
	}
	// بقیه فایل حفظ شده باشد
	if !strings.Contains(s, "$schema") {
		t.Fatalf("schema line lost:\n%s", s)
	}
}

func TestSetModelCreatesFile(t *testing.T) {
	e := mkScratchOCEnv(t)
	if err := e.setModel("openai/gpt-5"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(e.configPath())
	if !strings.Contains(string(b), `"model": "openai/gpt-5"`) {
		t.Fatalf("not created:\n%s", string(b))
	}
}

func TestSetModelRejectsBadFormat(t *testing.T) {
	e := mkScratchOCEnv(t)
	if err := e.setModel("nodash"); err == nil {
		t.Fatal("expected error for missing provider/model")
	}
}

// TestSetModelProducesValidJSONC: خروجی setModel باید JSON(C) معتبر و بدون
// خط تکراری باشد (کاماهای درست). باگ قبلی: خط `$schema` بدون کاما و تکرار
// model/small_model که باعث می‌شد opencode serve از استارت بایستد.
func TestSetModelProducesValidJSONC(t *testing.T) {
	e := mkScratchOCEnv(t)
	// فایلی شبیه به چیزی که نسخه‌های قدیمی بات می‌ساختند (کامای $schema جا مانده)
	broken := "{\n  \"$schema\": \"https://opencode.ai/config.json\"\n  \"model\": \"opencode/big-pickle\",\n  \"small_model\": \"opencode/big-pickle\",\n  \"model\": \"opencode/big-pickle\",\n  \"small_model\": \"opencode/big-pickle\"\n}\n"
	if err := os.WriteFile(e.configPath(), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.setModel("openrouter/openrouter/free"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(e.configPath())
	s := string(b)
	if got := strings.Count(s, `"model":`); got != 1 {
		t.Fatalf("model باید یک بار باشد، شد %d:\n%s", got, s)
	}
	if got := strings.Count(s, `"small_model":`); got != 1 {
		t.Fatalf("small_model باید یک بار باشد، شد %d:\n%s", got, s)
	}
	// کامای $schema باید ست شود و خط آخر (small_model) کاما نداشته باشد
	if !strings.Contains(s, "config.json\",") {
		t.Fatalf("کامای $schema نبود:\n%s", s)
	}
	if strings.Contains(s, ",,\n") || strings.Contains(s, ",,\r\n") {
		t.Fatalf("کامای دوبل:\n%s", s)
	}
}

func TestHasKeyForKeylessProvider(t *testing.T) {
	e := mkScratchOCEnv(t)
	// پروایدر opencode به کلید نیاز ندارد
	if !e.hasKey("opencode") {
		t.Fatal("opencode باید بدون کلید ready تلقی شود")
	}
	if !e.isKeyless("opencode") {
		t.Fatal("isKeyless(opencode) باید true باشد")
	}
	if e.isKeyless("openrouter") {
		t.Fatal("isKeyless(openrouter) باید false باشد")
	}
}

func TestAuthKeyCRUD(t *testing.T) {
	e := mkScratchOCEnv(t)
	if e.hasKey("deepseek") {
		t.Fatal("should not have key initially")
	}
	if err := e.addAuthKey("deepseek", "sk-abc123"); err != nil {
		t.Fatal(err)
	}
	if !e.hasKey("deepseek") {
		t.Fatal("key should exist now")
	}
	got := e.providersConfigured()
	if len(got) != 1 || got[0] != "deepseek" {
		t.Fatalf("providersConfigured=%v", got)
	}
	if err := e.removeAuthKey("deepseek"); err != nil {
		t.Fatal(err)
	}
	if e.hasKey("deepseek") {
		t.Fatal("key should be gone")
	}
}
