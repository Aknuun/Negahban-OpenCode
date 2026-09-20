package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ocEnv مدیریت فایل‌های کانفیگ و auth خود opencode روی سرور
type ocEnv struct {
	ConfigDir string // پوشه‌ای که opencode.json[c] در آن است
	DataDir   string // پوشه‌ای که auth.json در آن است
	Service   string // نام سرویس systemd سرور opencode (خالی = غیرقابل ری‌استارت)
	Port      string // پورت سرور opencode (برای شناسایی سرویس)
}

// keylessProviders پروایدرهایی که خودِ opencode هستند و به API key نیاز ندارند
// (مثل opencode/big-pickle). برایشان نباید هشدار «کلید ندارد» نمایش داده شود.
var keylessProviders = map[string]bool{
	"opencode": true,
}

// isKeyless آیا این پروایدر بدون کلید API کار می‌کند؟
func (e *ocEnv) isKeyless(provider string) bool {
	return keylessProviders[provider]
}

var (
	reModelLine = regexp.MustCompile(`^([ \t]{0,2})"(model|small_model)"[ \t]*:[ \t]*"[^"]*"(.*)$`)
	reModelVal  = regexp.MustCompile(`^([ \t]{0,2})"(model|small_model)"[ \t]*:[ \t]*"([^"]*)"(.*)$`)
	reSchema    = regexp.MustCompile(`^([ \t]{0,2})"\$schema"`)
)

func newOCEnv(cfg *Config) *ocEnv {
	e := &ocEnv{Port: portOf(cfg.BaseURL), Service: cfg.OCService}
	home := homeDir()
	xdgCfg := os.Getenv("XDG_CONFIG_HOME")
	if xdgCfg == "" {
		xdgCfg = filepath.Join(home, ".config")
	}
	xdgData := os.Getenv("XDG_DATA_HOME")
	if xdgData == "" {
		xdgData = filepath.Join(home, ".local", "share")
	}
	if cfg.OCConfigHome != "" {
		e.ConfigDir = cfg.OCConfigHome
	} else {
		e.ConfigDir = filepath.Join(xdgCfg, "opencode")
	}
	if cfg.OCDataHome != "" {
		e.DataDir = cfg.OCDataHome
	} else {
		e.DataDir = filepath.Join(xdgData, "opencode")
	}
	if e.Service == "" {
		e.Service = detectOCService(e.Port)
	}
	return e
}

// homeDir مسیر خانه؛ در سرویس‌های systemd گاهی HOME ست نیست، پس به /root برمی‌گردیم
func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	if os.Geteuid() == 0 {
		return "/root"
	}
	return "."
}

func portOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if p := u.Port(); p != "" {
		return p
	}
	return ""
}

// detectOCService پیدا کردن سرویس systemd در حال اجرای opencode serve
// مهم: هرگز نباید سرویسِ خودِ ربات (negahban-opencode) را برگرداند، چون
// ری‌استارت آن یعنی خاموش شدن ربات وسط عملیات (signal: terminated).
func detectOCService(port string) string {
	out, err := exec.Command("systemctl", "--no-legend", "list-units", "--type=service", "--state=running").Output()
	if err != nil {
		return ""
	}
	var cands []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.Contains(f[0], "opencode") {
			continue
		}
		cands = append(cands, f[0])
	}
	execOf := func(u string) string {
		es, _ := exec.Command("systemctl", "show", "-p", "ExecStart", u).Output()
		return string(es)
	}
	// اول سرویس‌های سرور (کسانی که «serve» اجرا می‌کنند)؛ اگر پورت هم می‌دانیم
	// سروری که روی همان پورت است را برمی‌گردانیم.
	var served []string
	for _, u := range cands {
		es := execOf(u)
		if !strings.Contains(es, " serve ") {
			continue
		}
		served = append(served, u)
		if port != "" && strings.Contains(es, ":"+port) {
			return u
		}
	}
	if len(served) > 0 {
		return served[0]
	}
	// fallback: هر واحد opencode دیگری که روی همین پورت گوش می‌دهد
	for _, u := range cands {
		if port != "" && strings.Contains(execOf(u), ":"+port) {
			return u
		}
	}
	return ""
}

func (e *ocEnv) authPath() string {
	return filepath.Join(e.DataDir, "auth.json")
}

func (e *ocEnv) configPath() string {
	for _, name := range []string{"opencode.jsonc", "opencode.json"} {
		p := filepath.Join(e.ConfigDir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(e.ConfigDir, "opencode.jsonc")
}

// readAuth خواندن auth.json
func (e *ocEnv) readAuth() (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	b, err := os.ReadFile(e.authPath())
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, err
	}
	return out, nil
}

// providersConfigured پروایدرهایی که کلید دارند
func (e *ocEnv) providersConfigured() []string {
	seen := map[string]bool{}
	var ids []string
	m, err := e.readAuth()
	if err == nil {
		for id, v := range m {
			if v == nil {
				continue
			}
			if k, _ := v["key"]; k != "" {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	for _, cand := range []string{"deepseek", "openai", "anthropic", "google", "xai", "openrouter", "mistral", "groq"} {
		if !seen[cand] && os.Getenv(envVarFor(cand)) != "" {
			seen[cand] = true
			ids = append(ids, cand)
		}
	}
	sort.Strings(ids)
	return ids
}

func (e *ocEnv) hasKey(provider string) bool {
	if e.isKeyless(provider) {
		return true
	}
	m, err := e.readAuth()
	if err == nil {
		if v, ok := m[provider]; ok && v != nil && v["key"] != "" {
			return true
		}
	}
	// بعضی‌ها کلید را با متغیر محیطی می‌دهند
	if envVar := envVarFor(provider); envVar != "" {
		if os.Getenv(envVar) != "" {
			return true
		}
	}
	return false
}

func envVarFor(provider string) string {
	m := map[string]string{
		"deepseek":   "DEEPSEEK_API_KEY",
		"openai":     "OPENAI_API_KEY",
		"anthropic":  "ANTHROPIC_API_KEY",
		"google":     "GOOGLE_GENERATIVE_AI_API_KEY",
		"xai":        "XAI_API_KEY",
		"openrouter": "OPENROUTER_API_KEY",
		"mistral":    "MISTRAL_API_KEY",
		"groq":       "GROQ_API_KEY",
		"github":     "GITHUB_TOKEN",
	}
	return m[provider]
}

// addAuthKey افزودن/به‌روزرسانی کلید یک پروایدر
func (e *ocEnv) addAuthKey(provider, key string) error {
	m, err := e.readAuth()
	if err != nil {
		return err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("کلید خالی است")
	}
	entry := m[provider]
	if entry == nil {
		entry = map[string]string{}
	}
	entry["type"] = "api"
	entry["key"] = key
	m[provider] = entry
	if err := os.MkdirAll(e.DataDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(e.authPath(), append(b, '\n'), 0o600); err != nil {
		return err
	}
	return nil
}

// removeAuthKey حذف کلید یک پروایدر از auth.json
func (e *ocEnv) removeAuthKey(provider string) error {
	m, err := e.readAuth()
	if err != nil {
		return err
	}
	if _, ok := m[provider]; !ok {
		return nil
	}
	delete(m, provider)
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(e.authPath(), append(b, '\n'), 0o600)
}

// currentModel خواندن مدل پیش‌فرض از کانفیگ
func (e *ocEnv) currentModel() string {
	b, err := os.ReadFile(e.configPath())
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		m := reModelVal.FindStringSubmatch(line)
		if m != nil && m[2] == "model" && m[3] != "" {
			return m[3]
		}
	}
	return ""
}

// setModel تنظیم model و small_model در کانفیگ (با حفظ بقیه فایل)
// خروجی همیشه JSON(C) معتبر است: بدون خط تکراری و با کامای درست بین کلیدها.
func (e *ocEnv) setModel(full string) error {
	full = strings.TrimSpace(full)
	if !strings.Contains(full, "/") {
		return fmt.Errorf("قالب مدل باید provider/model باشد، مثل deepseek/deepseek-v4-flash")
	}
	path := e.configPath()
	var lines []string
	if b, err := os.ReadFile(path); err == nil {
		lines = strings.Split(string(b), "\n")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(e.ConfigDir, 0o755); err != nil {
		return err
	}
	modelLine := fmt.Sprintf(`  "model": "%s",`, full)
	smallLine := fmt.Sprintf(`  "small_model": "%s",`, full)

	var out []string
	inserted := false
	for _, l := range lines {
		if m := reModelLine.FindStringSubmatch(l); m != nil {
			// خطوط قدیمی model/small_model را حذف کن تا تکراری نمانند
			continue
		}
		if reSchema.MatchString(l) {
			out = append(out, l)
			out = append(out, modelLine)
			out = append(out, smallLine)
			inserted = true
			continue
		}
		out = append(out, l)
	}
	if !inserted {
		// هیچ خط schema/modelی نبود؛ به‌صورت امن قبل از براکت بستن اضافه کن
		if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "}" {
			out = append(out[:len(out)-1], modelLine, smallLine, "}")
		} else {
			out = append([]string{"{", modelLine, smallLine, "}"}, out...)
		}
	}
	out = fixJSONCLines(out)
	return os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o644)
}

// fixJSONCLines اطمینان از کاماگذاری درست بین کلیدهای JSON(C)
// هر خط کلید/مقدار بدون کاما که خط بعدی‌اش بستن براکت نیست، کاما می‌گیرد.
func fixJSONCLines(lines []string) []string {
	var out []string
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || !strings.Contains(t, `":`) || strings.HasPrefix(t, "//") ||
			strings.HasPrefix(t, "/*") || t == "}" || strings.HasSuffix(t, "{") || strings.HasSuffix(t, "}") {
			out = append(out, l)
			continue
		}
		hasComma := strings.HasSuffix(t, ",")
		next := ""
		for _, n := range lines[i+1:] {
			if nt := strings.TrimSpace(n); nt != "" {
				next = nt
				break
			}
		}
		switch {
		case next == "}":
			out = append(out, strings.TrimSuffix(strings.TrimSpace(l), ","))
		case hasComma:
			out = append(out, l)
		default:
			out = append(out, t+",")
		}
	}
	return out
}

// restart ری‌استارت سرویس systemd سرور opencode
func (e *ocEnv) restart() error {
	if e.Service == "" {
		return fmt.Errorf("سرویس opencode پیدا نشد؛ سشن را با /new عوض کن یا سرور را دستی ری‌استارت کن")
	}
	out, err := exec.Command("systemctl", "restart", e.Service).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ری‌استارت %s ناموفق بود: %v %s", e.Service, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// agentDirs لیست agentهای سفارشی تعریف‌شده در پوشه کانفیگ
func (e *ocEnv) customAgents() []string {
	dir := filepath.Join(e.ConfigDir, "agent")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, en := range entries {
		name := en.Name()
		if en.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if strings.HasSuffix(name, ".md") {
			name = strings.TrimSuffix(name, ".md")
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
