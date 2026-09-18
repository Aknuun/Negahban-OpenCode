package main

import (
	"context"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"negahban-opencode/internal/occlient"
)

// ocAPI اینترفیس API سرور opencode است؛ کانکریت آن occlient.Client است و در
// تست‌ها می‌توان یک پیاده‌سازی fake به ربات داد.
type ocAPI interface {
	CreateSession(context.Context) (*occlient.Session, error)
	ListSessions(context.Context, int) ([]occlient.Session, error)
	DeleteSession(context.Context, string) error
	GetSession(context.Context, string) (*occlient.Session, error)
	PromptAsync(context.Context, string, string, string) error
	LastMessage(context.Context, string) (*occlient.Message, error)
	ListMessages(context.Context, string, int) ([]occlient.Message, error)
	Abort(context.Context, string) error
	Summarize(context.Context, string, string, string) error
	ListQuestions(context.Context) ([]occlient.QuestionRequest, error)
	ReplyQuestion(context.Context, string, [][]string) error
	RejectQuestion(context.Context, string) error
}

// اطمینان از اینکه کلاینت واقعی اینترفیس را پیاده می‌کند
var _ ocAPI = (*occlient.Client)(nil)

const helpText = `ربات کنترل opencode روی سرور

ارسال پیام متنی / فایل => اجرا روی نشستِ فعال فعلی

دکمه‌های ثابت زیر پیام‌ها:
📊 وضعیت و هزینه — وضعیت نشست فعال
⚙️ تنظیمات — مدل، کلید API و agent
🗂 نشست‌ها — مدیریت نشست‌ها (ساخت/تعویض/توقف)

نشست‌ها می‌توانند هم‌زمان اجرا شوند؛ تعویض نشست، اجرای در جریان را متوقف نمی‌کند.

دستورات:
/new         ساخت نشست جدید و رفتن به آن
/use <id>    رفتن به نشست مشخص (با فهرست موضوعاتش)
/sessions    باز کردن مدیریت نشست‌ها
/list        فهرست نشست‌های اخیر سرور
/status      جزئیات نشست فعال
/agent       نمایش/تغییر agent
/cancel      توقف اجرای نشست فعال
/help        این راهنما

حین اجرا پاسخ به‌صورت زنده به‌روز می‌شود؛ روی پیام «در حال انجام» دکمهٔ ⏹ توقف همان نشست است.

در پیام وضعیت و پایان هر چت، دکمه‌های سریع نشست هست: 🗜 فشرده‌سازی (کاهش مصرف توکن)، ✖️ بستن نشست و 🗑 حذف نشست.

اگر مدل در میانهٔ کار سؤالی بپرسد (مثل خود CLI)، همان پیام گزینه‌ها را دارد؛ با دکمه‌ها پاسخ بده تا اجرا ادامه یابد. ✏️ یعنی می‌توانی پاسخ خودت را تایپ کنی.`

// botVersion نسخهٔ ربات است؛ هنگام انتشار نسخهٔ جدید آن را به‌روز کن
const botVersion = "v8.13"

const (
	btnStatus    = "وضعیت و هزینه"
	btnSettings  = "تنظیمات " + botVersion
	btnSessions  = "نشست‌ها"
	maxFileBytes = 30 << 20

	// حداکثر اجرای هم‌زمان برای هر کاربر
	maxConcurrentRuns = 6

	// اگر مدل این مدت هیچ متن/فعالیت تازه‌ای تولید نکند، اجرا متوقف می‌شود
	// (وقتی پروایدر پاسخ نمی‌دهد، poll نباید بی‌نهایت «در حال انجام» بماند)
	pollQuietLimit = 6 * time.Minute

	// نمایش «🗂 نشست‌ها» مستقیماً از سرور opencode (نه فقط فهرست محلی ربات)
	maxTrackedSessions = 80  // حداکثر نشستی که ربات در state نگه می‌دارد
	sessionsFetchMax   = 300 // چند نشست اخیر سرور برای همگام‌سازی واکشی شود
	sessionsPageSize   = 20  // هر صفحه از مدیر نشست‌ها چند نشست نشان دهد
)

// runCtl کنترل اجرای هم‌زمان یک نشست
type runCtl struct {
	SID     string
	UserID  int64
	ChatID  int64
	cancel  context.CancelFunc
	done    chan struct{}
	started time.Time
}

type Bot struct {
	cfg   *Config
	oc    ocAPI
	api   telegramAPI
	users *userStore // state ماندگار کاربران + ذخیره‌سازی اتمیک
	ui    *uiState   // داده‌های گذرای رابط کاربری (صفحه‌ها/حالت گروهی/کش هزینه)
	runs  *runManager
	qs    *qReg // سؤال‌های تعاملی در انتظار پاسخ
	fx    *fxStore

	cat   *modelCatalog // کاتالوگ مدل‌ها (models.dev) با کش
	peaks *peakStore    // ساعت پیک مصرف پروایدرها
}

func newBot(cfg *Config, api telegramAPI) *Bot {
	return &Bot{
		cfg:   cfg,
		oc:    occlient.New(cfg.BaseURL, cfg.Agent),
		api:   api,
		users: newUserStore(cfg.StateFile),
		ui:    newUIState(),
		runs:  newRunManager(),
		qs:    newQReg(),
		fx:    newFXStore(),
		cat:   newModelCatalog(filepath.Join(filepath.Dir(cfg.StateFile), "catalog-models.json"), cfg.BaseURL),
		peaks: newPeakStore(filepath.Join(filepath.Dir(cfg.StateFile), "peakhours.json")),
	}
}

// close ذخیرهٔ نهایی state و توقف ذخیره‌کننده را انجام می‌دهد (قبل از خروج).
func (b *Bot) close() error {
	err := b.users.Close()
	if perr := b.peaks.close(); err == nil {
		err = perr
	}
	return err
}

func (b *Bot) loadStates() error {
	if err := b.peaks.load(); err != nil {
		slog.Warn("بارگذاری ساعت پیک ناموفق بود", "error", err)
	}
	return b.users.load()
}

func (b *Bot) allowed(userID int64) bool {
	return b.cfg.Allowed[userID]
}

// faNum اعداد را به رقم فارسی تبدیل می‌کند
func faNum(n int) string {
	r := []rune(strconv.Itoa(n))
	for i, c := range r {
		if c >= '0' && c <= '9' {
			r[i] = '۰' + (c - '0')
		}
	}
	return string(r)
}

// sessionLabel نام نمایشی نشست را برمی‌گرداند (نام دستی یا تاریخ آخرین فعالیت).
func (b *Bot) sessionLabel(userID int64, sid string) string {
	return b.users.sessionLabel(userID, sid)
}

// sessionTitle عنوان نمایشی نشست: نام دستی کاربر، وگرنه عنوان خودکار opencode
// (خلاصهٔ مطالب نشست) و در نهایت برچسب تاریخ/شماره.
func (b *Bot) sessionTitle(userID int64, sid string, titles map[string]string) string {
	if l, ok := b.users.manualLabel(userID, sid); ok && l != "" {
		return l
	}
	if t := strings.TrimSpace(titles[sid]); t != "" {
		return clipHead(collapse(t), 90)
	}
	return b.sessionLabel(userID, sid)
}

// parseSessionNumber شمارهٔ نشست را از متن کاربر می‌خواند (ارقام فارسی/عربی/لاتین).
func parseSessionNumber(s string) (int, bool) {
	var digits []rune
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= '۰' && r <= '۹':
			digits = append(digits, '0'+r-'۰')
		case r >= '٠' && r <= '٩':
			digits = append(digits, '0'+r-'٠')
		case r >= '0' && r <= '9':
			digits = append(digits, r)
		case r == ' ' || r == '\u200c':
			// فاصله/نیم‌فاصله نادیده گرفته می‌شود
		default:
			return 0, false
		}
	}
	if len(digits) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(string(digits))
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseSessionNumbers همهٔ شماره‌های داخل متن را می‌خواند (چند عدد با کاما/فاصله).
func parseSessionNumbers(s string) []int {
	var out []int
	var cur []rune
	flush := func() {
		if len(cur) == 0 {
			return
		}
		if n, err := strconv.Atoi(string(cur)); err == nil {
			out = append(out, n)
		}
		cur = cur[:0]
	}
	for _, r := range s {
		switch {
		case r >= '۰' && r <= '۹':
			cur = append(cur, '0'+r-'۰')
		case r >= '٠' && r <= '٩':
			cur = append(cur, '0'+r-'٠')
		case r >= '0' && r <= '9':
			cur = append(cur, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// sessionsBounds بازهٔ اندیس (۰-پایه، [start,end)) یک صفحه را می‌دهد.
func sessionsBounds(page, total int) (int, int) {
	start := page * sessionsPageSize
	if start > total {
		start = total
	}
	end := start + sessionsPageSize
	if end > total {
		end = total
	}
	return start, end
}

// sessionsPageBounds بازهٔ اندیس نشست‌های صفحهٔ فعلی است.
func (b *Bot) sessionsPageBounds(chatID int64, total int) (int, int) {
	return sessionsBounds(b.lastSSPage(chatID), total)
}

// pickNumbers شماره‌های صفحه (۱-پایه) را می‌دهد.
func pickNumbers(start, end int) []int {
	var ns []int
	for n := start + 1; n <= end; n++ {
		ns = append(ns, n)
	}
	return ns
}

// pickedSids شماره‌های تیک‌خورده را به شناسهٔ نشست تبدیل می‌کند (مرتب‌شده).
func (b *Bot) pickedSids(chatID int64) []string {
	_, _, picked := b.ui.ssPick(chatID)
	list := b.ui.ssList(chatID)
	nums := make([]int, 0, len(picked))
	for n := range picked {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	var sids []string
	for _, n := range nums {
		if n >= 1 && n <= len(list) {
			sids = append(sids, list[n-1])
		}
	}
	return sids
}

// renderSessionsPicker صفحهٔ انتخابِ دکمه‌ای شماره‌ها را نشان می‌دهد.
func (b *Bot) renderSessionsPicker(chatID int64, msgID int) {
	action, page, picked := b.ui.ssPick(chatID)
	list := b.ui.ssList(chatID)
	start, end := sessionsBounds(page, len(list))

	var title string
	switch action {
	case "del":
		title = "🗑 کدام نشست‌ها حذف شوند؟"
	case "rn":
		title = "✏️ نام کدام نشست‌ها عوض شود؟"
	case "view":
		title = "👁 کدام نشست‌ها نمایش داده شوند؟"
	default:
		title = "🔀 عملیات گروهی"
	}
	var sb strings.Builder
	sb.WriteString(title + "\n")
	if end <= start {
		sb.WriteString("نشستی در این صفحه نیست.")
	} else {
		fmt.Fprintf(&sb, "روی شماره‌ها بزن تا تیک بخورند (صفحه: %s تا %s).", faNum(start+1), faNum(end))
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton
	for n := start + 1; n <= end; n++ {
		label := faNum(n)
		if picked[n] {
			label = "✅" + faNum(n)
		}
		row = append(row, inlineBtn(label, "ss:tgl:"+strconv.Itoa(n)))
		if len(row) == 5 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		inlineBtn("☑️ انتخاب همه", "ss:allsel:all"),
		inlineBtn("🔲 لغو انتخاب", "ss:none:all"),
	})
	if action == "" {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			inlineBtn("🗑 حذف انتخاب‌شده‌ها", "ss:go:del"),
			inlineBtn("✏️ تغییر نام انتخاب‌شده‌ها", "ss:go:rn"),
		})
	} else {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{inlineBtn("▶️ تأیید", "ss:ok:all")})
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{inlineBtn("🔙 بازگشت", "ss:refresh")})

	edit := tgbotapi.NewEditMessageText(chatID, msgID, sb.String())
	edit.ReplyMarkup = rowsOf(rows...)
	b.api.Send(edit)
}

// applySessionsAction عمل انتخاب‌شده را روی نشست‌های داده‌شده اجرا می‌کند.
func (b *Bot) applySessionsAction(userID, chatID int64, msgID int, action string, sids []string) {
	if len(sids) == 0 {
		b.send(chatID, "نشستی انتخاب نشد.")
		return
	}
	switch action {
	case "view":
		if len(sids) == 1 {
			b.useSession(userID, chatID, sids[0])
			return
		}
		for _, sid := range sids {
			head := "👁 " + b.sessionLabel(userID, sid) + "\n\n"
			b.sendChunks(chatID, head+b.sessionTopicsText(sid), 0)
		}
	case "rn":
		b.ui.setSSSel(chatID, "rn", sids)
		b.setPendingByChat(chatID, "ssrn")
		prompt := "✏️ نام جدید را برای " + faNum(len(sids)) + " نشست انتخاب‌شده بفرست:"
		if len(sids) == 1 {
			prompt = "✏️ نام جدید نشست «" + b.sessionLabel(userID, sids[0]) + "» را بفرست:"
		}
		if msgID != 0 {
			b.edit(chatID, msgID, prompt)
		} else {
			b.send(chatID, prompt)
		}
	case "del":
		b.ui.setSSSel(chatID, "del", sids)
		var names []string
		for i, sid := range sids {
			if i >= 10 {
				names = append(names, "…")
				break
			}
			names = append(names, "• "+html.EscapeString(b.sessionLabel(userID, sid)))
		}
		text := fmt.Sprintf("🗑 <b>%s نشست</b> برای همیشه حذف شوند؟\nاین نشست‌ها و تمام گفتگویشان از روی سرور opencode پاک می‌شود (غیرقابل بازگشت).\n\n%s", faNum(len(sids)), strings.Join(names, "\n"))
		kb := rowsOf(
			[]tgbotapi.InlineKeyboardButton{inlineBtn("🗑 بله، همه را حذف کن", "ss:delc:all")},
			[]tgbotapi.InlineKeyboardButton{inlineBtn("انصراف", "ss:refresh")},
		)
		if msgID != 0 {
			edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
			edit.ParseMode = "HTML"
			edit.ReplyMarkup = kb
			b.api.Send(edit)
		} else {
			msg := tgbotapi.NewMessage(chatID, text)
			msg.ParseMode = "HTML"
			msg.ReplyMarkup = kb
			b.api.Send(msg)
		}
	default:
		b.send(chatID, "عملیات نامشخص.")
	}
}

func (b *Bot) stateFor(userID, chatID int64) *UserState {
	return b.users.ensure(userID, chatID)
}

func (b *Bot) stateOf(userID int64) *UserState {
	return b.users.byID(userID)
}

func (b *Bot) setSession(userID int64, id string) {
	b.users.activate(userID, id)
}

func (b *Bot) setSessionLabel(userID int64, sid, label string) {
	b.users.rename(userID, sid, label)
}

// deleteSession حذف نشست از فهرست کاربر (+ توقف اجرا اگر در جریان باشد)
func (b *Bot) deleteSession(userID int64, sid string) {
	b.stopRun(sid)
	b.users.remove(userID, sid)
}

func (b *Bot) setAgentValue(userID int64, agent string) {
	b.users.setAgent(userID, agent)
}

func (b *Bot) ensureSession(userID, chatID int64) (string, error) {
	if st := b.stateOf(userID); st != nil && st.SessionID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := b.oc.GetSession(ctx, st.SessionID); err == nil {
			return st.SessionID, nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := b.oc.CreateSession(ctx)
	if err != nil {
		return "", fmt.Errorf("ساخت session ممکن نشد: %v", err)
	}
	b.setSession(userID, s.ID)
	b.stateFor(userID, chatID)
	return s.ID, nil
}

func (b *Bot) agentFor(userID int64) string {
	if a := b.users.agentOf(userID); a != "" {
		return a
	}
	if b.cfg != nil {
		return b.cfg.Agent
	}
	return "build"
}

func (b *Bot) userForChat(chatID int64) int64 {
	return b.users.userForChat(chatID)
}

func (b *Bot) costLabel(userID int64) string {
	sid := b.users.activeSession(userID)
	if sid == "" {
		return btnStatus
	}
	if label, ok := b.ui.costCached(userID); ok {
		return label
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	s, err := b.oc.GetSession(ctx, sid)
	label := btnStatus
	if err == nil && s.Cost > 0 {
		label = b.costButton(s.Cost)
	}
	b.ui.storeCost(userID, label)
	return label
}

func abbrev(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return strconv.Itoa(n)
	}
}

// ---------- کیبوردها ----------

func (b *Bot) replyKeyboard(chatID int64) tgbotapi.ReplyKeyboardMarkup {
	label := btnStatus
	if uid := b.userForChat(chatID); uid != 0 {
		label = b.costLabel(uid)
	}
	return tgbotapi.ReplyKeyboardMarkup{
		ResizeKeyboard: true,
		Keyboard: [][]tgbotapi.KeyboardButton{
			{
				{Text: label},
				{Text: btnSettings},
				{Text: btnSessions},
			},
		},
	}
}

func stopKeyboard(sid string) *tgbotapi.InlineKeyboardMarkup {
	data := "stop:" + sid
	return &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{{
			{Text: "⏹ توقف این نشست", CallbackData: &data},
		}},
	}
}

func (b *Bot) send(chatID int64, text string) (int, error) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = b.replyKeyboard(chatID)
	m, err := b.api.Send(msg)
	if err != nil {
		return 0, err
	}
	return m.MessageID, nil
}

func (b *Bot) sendProgress(chatID int64, text string, sid string) (int, error) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = stopKeyboard(sid)
	m, err := b.api.Send(msg)
	if err != nil {
		return 0, err
	}
	return m.MessageID, nil
}

// sendInline پیامی با کیبورد شیشه‌ای می‌فرستد (کیبورد ثابت پایین صفحه دست‌نخورده
// می‌ماند). برای دکمه‌هایی مثل فشرده‌سازی/بستن/حذف نشست.
func (b *Bot) sendInline(chatID int64, text string, kb *tgbotapi.InlineKeyboardMarkup) (int, error) {
	msg := tgbotapi.NewMessage(chatID, text)
	if kb != nil {
		msg.ReplyMarkup = kb
	}
	m, err := b.api.Send(msg)
	if err != nil {
		return 0, err
	}
	return m.MessageID, nil
}

func (b *Bot) edit(chatID int64, msgID int, text string) {
	edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
	b.api.Send(edit)
}

func (b *Bot) removeInline(chatID int64, msgID int) {
	rm := tgbotapi.NewEditMessageReplyMarkup(chatID, msgID, tgbotapi.InlineKeyboardMarkup{})
	b.api.Send(rm)
}

func (b *Bot) sendChunks(chatID int64, text string, progressMsgID int) {
	const max = 4000
	runes := []rune(text)
	if len(runes) == 0 {
		runes = []rune("(پاسخی نبود)")
	}
	if len(runes) <= max {
		if progressMsgID != 0 {
			b.edit(chatID, progressMsgID, text)
			b.removeInline(chatID, progressMsgID)
			return
		}
		b.send(chatID, text)
		return
	}
	var parts []string
	for len(runes) > max {
		parts = append(parts, string(runes[:max]))
		runes = runes[max:]
	}
	parts = append(parts, string(runes))
	if progressMsgID != 0 {
		b.edit(chatID, progressMsgID, "پاسخ کامل شد، در حال ارسال…")
	}
	b.send(chatID, parts[0])
	for _, p := range parts[1:] {
		b.send(chatID, p)
	}
	if progressMsgID != 0 {
		del := tgbotapi.NewDeleteMessage(chatID, progressMsgID)
		b.api.Send(del)
	}
}

// ---------- مدیریت اجراها ----------

func (b *Bot) runFor(sid string) (*runCtl, bool) {
	return b.runs.get(sid)
}

// isRunning می‌گوید آیا نشستی همین حالا در حال اجراست یا نه.
func (b *Bot) isRunning(sid string) bool {
	_, ok := b.runs.get(sid)
	return ok
}

func (b *Bot) activeRunsOf(userID int64) int {
	return b.runs.countFor(userID)
}

func (b *Bot) addRun(r *runCtl) {
	b.runs.add(r)
}

func (b *Bot) delRun(sid string) {
	b.runs.remove(sid)
}

func (b *Bot) stopRun(sid string) bool {
	r, ok := b.runFor(sid)
	if !ok {
		return false
	}
	r.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	b.oc.Abort(ctx, sid)
	return true
}

func (b *Bot) Handle(upd tgbotapi.Update) {
	if upd.CallbackQuery != nil {
		b.handleCallback(upd.CallbackQuery)
		return
	}
	if upd.Message == nil {
		return
	}
	userID := upd.Message.From.ID
	chatID := upd.Message.Chat.ID
	if !b.allowed(userID) {
		b.send(chatID, "شما مجاز به استفاده از این ربات نیستید.")
		return
	}
	b.stateFor(userID, chatID)

	if upd.Message.Document != nil {
		b.handleFile(userID, chatID, upd.Message.Document.FileID, upd.Message.Document.FileName, upd.Message.Caption, false)
		return
	}
	if len(upd.Message.Photo) > 0 {
		p := upd.Message.Photo[len(upd.Message.Photo)-1]
		b.handleFile(userID, chatID, p.FileID, "photo.jpg", upd.Message.Caption, true)
		return
	}
	if upd.Message.Text == "" {
		return
	}
	text := strings.TrimSpace(upd.Message.Text)
	if text == btnStatus || strings.HasPrefix(text, "$") {
		b.showStatus(userID, chatID)
		return
	}
	switch text {
	case btnSettings:
		b.openSettings(userID, chatID)
		return
	case btnSessions:
		b.openSessions(userID, chatID, 0)
		return
	}
	if uid, pending := b.users.pendingForChat(chatID); pending != "" {
		if strings.HasPrefix(text, "/") {
			b.users.clearPending(uid)
			b.handleCommand(upd, text)
			return
		}
		b.handlePending(uid, chatID, pending, text)
		return
	}
	if strings.HasPrefix(text, "/") {
		b.handleCommand(upd, text)
		return
	}
	b.submitPrompt(userID, chatID, text)
}

// cbRoute یک مسیر callback تلگرام است: پیشوند + تابع اجراکننده.
type cbRoute struct {
	prefix string
	run    func(b *Bot, cq *tgbotapi.CallbackQuery)
}

// cbRoutes روتر یکپارچهٔ callbackها: هر بخش از ربات پیشوند خودش را ثبت کرده و
// بدون وابستگی به بخش‌های دیگر فقط با `data` مسیر را پیدا می‌کند.
var cbRoutes = []cbRoute{
	{prefix: "s:", run: settingsAction},
	{prefix: "ss:", run: sessionsAction},
	{prefix: "qa:", run: questionAction},
	{prefix: "stop:", run: stopRunAction},
	{prefix: "cnt:", run: continueAction},
	{prefix: "pk:", run: peakAction},
	{prefix: "ds:", run: deleteSessionAction},
	{prefix: "cmp:", run: compactAction},
	{prefix: "close:", run: closeSessionAction},
}

func settingsAction(b *Bot, cq *tgbotapi.CallbackQuery) {
	b.onSettingsCallback(cq.From.ID, cq.Message.Chat.ID, cq.Message.MessageID, cq.Data)
}

func sessionsAction(b *Bot, cq *tgbotapi.CallbackQuery) {
	b.onSessionsCallback(cq.From.ID, cq.Message.Chat.ID, cq.Message.MessageID, cq.Data)
}

func questionAction(b *Bot, cq *tgbotapi.CallbackQuery) {
	b.onQCallback(cq.Message.Chat.ID, cq.Data)
}

func stopRunAction(b *Bot, cq *tgbotapi.CallbackQuery) {
	b.stopRun(strings.TrimPrefix(cq.Data, "stop:"))
}

// deleteSessionAction دکمهٔ «حذف این نشست» در پایان چت را انجام می‌دهد.
func deleteSessionAction(b *Bot, cq *tgbotapi.CallbackQuery) {
	sid := strings.TrimPrefix(cq.Data, "ds:")
	chatID := cq.Message.Chat.ID
	if sid == "" {
		return
	}
	userID := b.userForChat(chatID)
	if userID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	err := b.oc.DeleteSession(ctx, sid)
	cancel()
	if err != nil {
		b.editKeyboard(chatID, cq.Message.MessageID, "❌ حذف نشست ناموفق بود: "+err.Error(), &tgbotapi.InlineKeyboardMarkup{})
		return
	}
	b.deleteSession(userID, sid)
	b.editKeyboard(chatID, cq.Message.MessageID, "🗑 نشست حذف شد.", &tgbotapi.InlineKeyboardMarkup{})
}

// compactTimeout مهلت فشرده‌سازی (summarize) است؛ چون خودش یک فراخوانی LLM
// است ممکن است چند دقیقه طول بکشد.
const compactTimeout = 10 * time.Minute

// compactAction دکمهٔ «فشرده‌سازی» را انجام می‌دهد: نشست را روی سرور opencode
// خلاصه می‌کند تا مصرف توکنِ ادامهٔ گفتگو کاهش یابد.
func compactAction(b *Bot, cq *tgbotapi.CallbackQuery) {
	sid := strings.TrimPrefix(cq.Data, "cmp:")
	chatID := cq.Message.Chat.ID
	if sid == "" {
		return
	}
	userID := b.userForChat(chatID)
	if userID == 0 {
		return
	}
	providerID, modelID := b.sessionProviderModel(sid)
	if providerID == "" || modelID == "" {
		b.editKeyboard(chatID, cq.Message.MessageID,
			"❌ مدل نشست مشخص نیست؛ برای فشرده‌سازی مدل لازم است.", &tgbotapi.InlineKeyboardMarkup{})
		return
	}
	msgID, err := b.sendInline(chatID, "🗜 در حال فشرده‌سازی نشست… (ممکن است کمی طول بکشد)", nil)
	if err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), compactTimeout)
		defer cancel()
		if err := b.oc.Summarize(ctx, sid, providerID, modelID); err != nil {
			b.edit(chatID, msgID, "❌ فشرده‌سازی ناموفق بود: "+err.Error())
			return
		}
		b.edit(chatID, msgID, "✅ نشست فشرده شد؛ مصرف توکنِ ادامهٔ گفتگو کم می‌شود.")
	}()
}

// closeSessionAction دکمهٔ «بستن نشست» را انجام می‌دهد: اجرای در جریان آن
// متوقف و نشست از حالت فعال خارج می‌شود، ولی روی سرور باقی می‌ماند.
func closeSessionAction(b *Bot, cq *tgbotapi.CallbackQuery) {
	sid := strings.TrimPrefix(cq.Data, "close:")
	chatID := cq.Message.Chat.ID
	if sid == "" {
		return
	}
	userID := b.userForChat(chatID)
	if userID == 0 {
		return
	}
	b.stopRun(sid)
	b.users.deactivate(userID, sid)
	b.editKeyboard(chatID, cq.Message.MessageID,
		"✖️ نشست بسته شد؛ پیام بعدی یک نشست تازه می‌سازد.\nاین نشست روی سرور باقی است و از 🗂 نشست‌ها می‌توانی برگردی.",
		&tgbotapi.InlineKeyboardMarkup{})
}

// sessionProviderModel پروایدر و مدلِ یک نشست را برای عملیات‌هایی مثل فشرده‌سازی
// برمی‌گرداند؛ اگر روی نشست تنظیم نشده باشد به مدل کانفیگ برمی‌گردد.
func (b *Bot) sessionProviderModel(sid string) (providerID, modelID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if s, err := b.oc.GetSession(ctx, sid); err == nil && s != nil {
		providerID, modelID = s.Model.ProviderID, s.Model.ID
		if modelID == "" {
			modelID = s.ModelID
		}
	}
	if providerID == "" || modelID == "" {
		if full := b.envFor().currentModel(); full != "" {
			if pid, mid, ok := strings.Cut(full, "/"); ok {
				if providerID == "" {
					providerID = pid
				}
				if modelID == "" {
					modelID = mid
				}
			}
		}
	}
	return providerID, modelID
}

func (b *Bot) handleCallback(cq *tgbotapi.CallbackQuery) {
	if !b.allowed(cq.From.ID) {
		return
	}
	b.api.Request(tgbotapi.NewCallback(cq.ID, ""))
	for _, r := range cbRoutes {
		if strings.HasPrefix(cq.Data, r.prefix) {
			r.run(b, cq)
			return
		}
	}
	slog.Warn("callback ناشناخته", "data", cq.Data, "user", cq.From.ID)
}

// ---------- فایل ----------

func (b *Bot) handleFile(userID, chatID int64, fileID, fileName, caption string, isPhoto bool) {
	path, err := b.download(fileID, fileName)
	if err != nil {
		b.send(chatID, "دریافت فایل ناموفق بود: "+err.Error())
		return
	}
	prompt := strings.TrimSpace(caption)
	if prompt == "" {
		if isPhoto {
			prompt = "این تصویر پیوست‌شده را تحلیل کن و نتیجه را گزارش بده."
		} else {
			prompt = "محتوای این فایل پیوست‌شده را بررسی کن و خلاصه یا پاسخ مناسب بده."
		}
	}
	b.submitPrompt(userID, chatID, prompt+"\n\nفایل پیوست: "+path)
}

func (b *Bot) download(fileID, fileName string) (string, error) {
	file, err := b.api.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		return "", err
	}
	if file.FileSize > maxFileBytes {
		return "", fmt.Errorf("فایل بزرگ‌تر از حد مجاز (%d مگابایت) است", maxFileBytes>>20)
	}
	url := "https://api.telegram.org/file/bot" + b.cfg.Token + "/" + file.FilePath
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("دریافت فایل از تلگرام: %s", resp.Status)
	}
	dir := filepath.Join(filepath.Dir(b.cfg.StateFile), "downloads", "upload")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := filepath.Base(fileName)
	if name == "." || name == "/" || name == "" {
		name = "file"
	}
	dest := filepath.Join(dir, name)
	out, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return "", err
	}
	return dest, nil
}

// ---------- دستورات ----------

func (b *Bot) handleCommand(upd tgbotapi.Update, text string) {
	userID := upd.Message.From.ID
	chatID := upd.Message.Chat.ID
	cmd := text
	arg := ""
	if i := strings.IndexAny(cmd, " \n"); i >= 0 {
		cmd, arg = cmd[:i], strings.TrimSpace(cmd[i+1:])
	}
	switch cmd {
	case "/start", "/help":
		b.send(chatID, helpText)
	case "/new":
		b.newSession(userID, chatID)
	case "/sessions":
		b.openSessions(userID, chatID, 0)
	case "/use":
		if arg == "" {
			b.send(chatID, "استفاده: /use <session id>")
			return
		}
		b.useSession(userID, chatID, arg)
	case "/list":
		b.listSessions(chatID)
	case "/status":
		b.showStatus(userID, chatID)
	case "/agent":
		cur := b.agentFor(userID)
		if arg != "" {
			b.setAgentValue(userID, arg)
			b.send(chatID, "agent فعال: "+arg)
			return
		}
		b.send(chatID, "agent فعلی: "+cur+"\nبا دکمهٔ ⚙️ تنظیمات عوضش کن.")
	case "/cancel":
		st := b.stateOf(userID)
		if st == nil || st.SessionID == "" || !b.stopRun(st.SessionID) {
			b.send(chatID, "اجرایی برای نشست فعال در جریان نیست.")
			return
		}
		b.send(chatID, "اجرای نشست فعال متوقف شد.")
	default:
		b.send(chatID, "دستور ناشناخته. برای راهنما: /help")
	}
}

func (b *Bot) newSession(userID, chatID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := b.oc.CreateSession(ctx)
	if err != nil {
		b.send(chatID, "ساخت نشست ممکن نشد: "+err.Error())
		return
	}
	b.users.setActivity(s.ID, lastActivityOfOC(s))
	b.setSession(userID, s.ID)
	b.send(chatID, "نشست جدید ساخته و فعال شد.\n"+b.sessionLabel(userID, s.ID)+"\n"+s.ID)
}

func (b *Bot) listSessions(chatID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sessions, err := b.oc.ListSessions(ctx, 8)
	if err != nil {
		b.send(chatID, "خطا در دریافت فهرست: "+err.Error())
		return
	}
	var sb strings.Builder
	sb.WriteString("آخرین نشست‌های سرور:\n")
	for _, s := range sessions {
		title := strings.TrimSpace(s.Title)
		if len([]rune(title)) > 40 {
			title = string([]rune(title)[:40]) + "…"
		}
		fmt.Fprintf(&sb, "\n%s\n  %s\n  هزینه: %s", s.ID, title, b.costText(s.Cost))
	}
	b.send(chatID, sb.String())
}

func (b *Bot) showStatus(userID, chatID int64) {
	st := b.stateOf(userID)
	if st == nil || st.SessionID == "" {
		b.send(chatID, "هنوز نشستی ساخته نشده. /new بزن یا یک متن بفرست.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := b.oc.GetSession(ctx, st.SessionID)
	if err != nil {
		b.send(chatID, "خطا: "+err.Error())
		return
	}
	agent := b.agentFor(userID)
	msg := fmt.Sprintf("نشست فعال: %s\n", s.ID)
	if t := strings.TrimSpace(s.Title); t != "" {
		msg += "عنوان: " + t + "\n"
	}
	msg += fmt.Sprintf("agent: %s\n", agent)
	msg += fmt.Sprintf("مدل: %s\n", s.ModelID)
	msg += fmt.Sprintf("توکن ورودی: %d\n", s.Tokens.Input)
	msg += fmt.Sprintf("توکن خروجی: %d\n", s.Tokens.Output)
	msg += fmt.Sprintf("جمع توکن: %s\n", abbrev(s.Tokens.Input+s.Tokens.Output))
	msg += fmt.Sprintf("هزینه: %s", b.costText(s.Cost))
	b.sendInline(chatID, msg, sessionQuickMarkup(s.ID))
}

// sessionQuickMarkup دکمه‌های سریع مدیریت نشست (فشرده‌سازی/بستن/حذف) را می‌سازد.
func sessionQuickMarkup(sid string) *tgbotapi.InlineKeyboardMarkup {
	return rowsOf(
		[]tgbotapi.InlineKeyboardButton{
			inlineBtn("🗜 فشرده‌سازی", "cmp:"+sid),
			inlineBtn("✖️ بستن نشست", "close:"+sid),
		},
		[]tgbotapi.InlineKeyboardButton{inlineBtn("🗑 حذف این نشست", "ds:"+sid)},
	)
}

// abortAllRuns همهٔ اجرای‌های فعال را متوقف می‌کند (بعد از تغییر مدل)
// بعد از ری‌استارت سرور، تمام اجرای‌های قبلی بی‌اعتبار می‌شوند
func (b *Bot) abortAllRuns() {
	b.runs.abortAll()
}

// ---------- ارسال پرامپت و اجرا ----------

// runOutcome نتیجهٔ نهایی یک اجراست.
type runOutcome int

const (
	outcomeDone    runOutcome = iota // پاسخ کامل شد
	outcomeStopped                   // کاربر (یا سؤال تعاملی) اجرا را متوقف کرد
	outcomeError                     // قطع ارتباط / بی‌پاسخی / خطای دیگر
)

// pollResult خروجی حلقهٔ poll: متن نهایی + نتیجه + مدل/پروایدرِ استفاده‌شده.
type pollResult struct {
	outcome  runOutcome
	text     string
	model    string
	provider string
}

// usage آمار لحظه‌ای مصرف یک نشست.
type usage struct {
	in, out  int
	cost     float64
	model    string
	provider string
}

func (b *Bot) submitPrompt(userID, chatID int64, prompt string) {
	if !b.ocReady() {
		b.send(chatID, b.ocSetupHint())
		b.openSettings(userID, chatID)
		return
	}
	// تک‌کاره: فقط یک کار در یک زمان؛ اگر اجرایی فعال است بپرس توقف یا ادامه
	if b.runs.total() > 0 {
		if b.qs.forChat(chatID) != nil {
			b.send(chatID, "مدل سؤالی پرسیده؛ با دکمه‌های همان پیام پاسخ بده (یا ✏️ بنویس).")
			return
		}
		b.askStopOrContinue(userID, chatID)
		return
	}
	// اگر همین حالا در ساعت پیک گران پروایدر فعال باشیم، اول تأیید بگیر
	if b.maybeAskPeak(userID, chatID, prompt) {
		return
	}
	b.startPrompt(userID, chatID, prompt)
}

// activeProvider پروایدر مدلِ واقعیِ نشست فعال را برمی‌گرداند؛ اگر نشستی نبود یا
// مدلش خوانده نشد، به مدل پیش‌فرض کانفیگ برمی‌گردد.
func (b *Bot) activeProvider(userID int64) string {
	if st := b.stateOf(userID); st != nil && st.SessionID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if s, err := b.oc.GetSession(ctx, st.SessionID); err == nil && s != nil {
			if pid := b.providerFor(s.Model.ProviderID, s.Model.ID); pid != "" {
				return pid
			}
			if pid := b.providerFor("", s.ModelID); pid != "" {
				return pid
			}
		}
	}
	return b.providerFor("", b.envFor().currentModel())
}

// providerFor پروایدر را تعیین می‌کند: اول providerID واقعیِ opencode، بعد بخش
// پروایدر مدل (اگر «provider/model» باشد)، و در نهایت تطبیق مدل با کاتالوگ.
func (b *Bot) providerFor(providerID, model string) string {
	if p := strings.TrimSpace(providerID); p != "" {
		return normalizeProvider(p)
	}
	if model == "" {
		return ""
	}
	if i := strings.IndexByte(model, '/'); i > 0 {
		return normalizeProvider(model[:i])
	}
	if p, ok := b.cat.providerOfModel(model); ok {
		return normalizeProvider(p)
	}
	return normalizeProvider(model)
}

// maybeAskPeak اگر پروایدر مدل فعال در بازهٔ پیک باشد، پیام تأیید با دو دکمه
// می‌فرستد و true برمی‌گرداند؛ در این حالت اجرا هنوز شروع نشده و منتظر پاسخ
// کاربر می‌مانیم.
func (b *Bot) maybeAskPeak(userID, chatID int64, prompt string) bool {
	pid := b.activeProvider(userID)
	if pid == "" {
		return false
	}
	endMin, ok := b.peaks.peakEnd(pid, time.Now())
	if !ok {
		return false
	}
	b.ui.setPeakPending(chatID, peakPending{prompt: prompt, pid: pid})
	msg := tgbotapi.NewMessage(chatID, peakPromptText(pid, endMin))
	msg.ReplyMarkup = rowsOf([]tgbotapi.InlineKeyboardButton{
		inlineBtn("نه بعدا میام", "pk:n"),
		inlineBtn("بله ادامه بده", "pk:y"),
	})
	b.api.Send(msg)
	return true
}

// startPrompt اجرای واقعی پرامپت را آغاز می‌کند (بعد از عبور از بررسی‌ها).
func (b *Bot) startPrompt(userID, chatID int64, prompt string) {
	st := b.stateFor(userID, chatID)
	sid := st.SessionID
	if sid == "" {
		created, err := b.ensureSession(userID, chatID)
		if err != nil {
			b.send(chatID, err.Error())
			return
		}
		sid = created
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &runCtl{SID: sid, UserID: userID, ChatID: chatID, cancel: cancel, done: make(chan struct{}), started: time.Now()}
	b.addRun(r)
	go b.runTask(ctx, r, prompt)
}

// runTask درخواست را مستقیم با agent انتخابی اجرا می‌کند (بدون مرحلهٔ طرح/تأیید).
func (b *Bot) runTask(ctx context.Context, r *runCtl, prompt string) {
	defer b.delRun(r.SID)

	agent := b.agentFor(r.UserID)
	prog, err := b.sendProgress(r.ChatID, "در حال انجام…", r.SID)
	if err != nil {
		return
	}

	pre, preOK := b.sessionUsage(r.SID)
	if err := b.oc.PromptAsync(ctx, r.SID, prompt, agent); err != nil {
		b.edit(r.ChatID, prog, "ارسال دستور ناموفق بود: "+err.Error())
		b.removeInline(r.ChatID, prog)
		return
	}

	res := b.poll(ctx, r, prog, pre.model)
	b.sendChunks(r.ChatID, res.text, prog)
	// پیامِ جداگانهٔ خلاصه در زیر پاسخ: کار تمام شد + مصرف توکن/هزینه + جزئیات
	b.sendRunSummary(r, res, pre, preOK)
}

// askStopOrContinue وقتی کار دیگری در حال اجراست، از کاربر می‌پرسد اجرای فعلی
// را متوقف کند یا ادامه دهد (پیام جدیدش الان اجرا نمی‌شود).
func (b *Bot) askStopOrContinue(userID, chatID int64) {
	sid := b.runs.activeSID()
	if sid == "" {
		return
	}
	r, ok := b.runs.get(sid)
	if !ok {
		return
	}
	if r.UserID != userID || r.ChatID != chatID {
		b.send(chatID, "الان در حال انجام کار دیگری‌ام؛ چند لحظهٔ دیگر دوباره بفرست.")
		return
	}
	text := "الان یک کار در حال انجام است. می‌خواهی آن را متوقف کنم یا ادامه بدهم؟\n(پیام تازه‌ات بعد از انتخاب اجرا نمی‌شود؛ باید دوباره بفرستی.)"
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = rowsOf(
		[]tgbotapi.InlineKeyboardButton{inlineBtn("⏹ متوقفش کن", "stop:"+sid)},
		[]tgbotapi.InlineKeyboardButton{inlineBtn("▶️ ادامه بده", "cnt:"+sid)},
	)
	b.api.Send(msg)
}

// continueAction دکمهٔ «ادامه بده» اجرای در جریان را دست نمی‌زند.
func continueAction(b *Bot, cq *tgbotapi.CallbackQuery) {
	b.send(cq.Message.Chat.ID, "باشد؛ ادامه می‌دهم. وقتی تمام شد، پیامت را دوباره بفرست.")
}

// ---------- ساعت پیک ----------

func peakAction(b *Bot, cq *tgbotapi.CallbackQuery) {
	b.onPeakCallback(cq.Message.Chat.ID, cq.Message.MessageID, cq.Data)
}

func (b *Bot) onPeakCallback(chatID int64, msgID int, data string) {
	userID := b.userForChat(chatID)
	switch {
	case data == "pk:y":
		pp, ok := b.ui.takePeakPending(chatID)
		b.removeInline(chatID, msgID)
		if !ok || userID == 0 {
			return
		}
		b.startPrompt(userID, chatID, pp.prompt)
	case data == "pk:n":
		b.ui.takePeakPending(chatID)
		b.removeInline(chatID, msgID)
		if userID != 0 {
			b.send(chatID, "باشه؛ هر وقت خواستی دوباره بفرست. 👍")
		}
	case strings.HasPrefix(data, "pk:add:"):
		pid := strings.TrimPrefix(data, "pk:add:")
		windows, ok := b.lookupPeak(pid)
		b.removeInline(chatID, msgID)
		if !ok {
			b.send(chatID, "متأسفانه ساعت پیک «"+peakProviderName(pid)+"» را پیدا نکردم؛ می‌توانی بعداً دستی اضافه‌اش کنم.")
			return
		}
		b.peaks.enable(pid, windows)
		b.send(chatID, "✅ یادآوری ساعت پیک «"+peakProviderName(pid)+"» فعال شد.\n"+formatWindows(windows))
	case strings.HasPrefix(data, "pk:no:"):
		pid := strings.TrimPrefix(data, "pk:no:")
		b.peaks.disable(pid)
		b.removeInline(chatID, msgID)
	}
}

// lookupPeak ساعت پیک یک پروایدر را پیدا می‌کند. فعلاً از دانش داخلی (خوانده‌شده
// از منابع رسمی) استفاده می‌کند؛ برای پروایدر ناشناس چیزی پیدا نمی‌شود.
func (b *Bot) lookupPeak(pid string) ([]peakWindow, bool) {
	ws := b.peaks.windowsFor(pid)
	if len(ws) == 0 {
		return nil, false
	}
	return ws, true
}

// askPeakForProvider هنگام افزودن کلید API می‌پرسد آیا ساعت پیک این پروایدر
// پیدا/فعال شود.
func (b *Bot) askPeakForProvider(chatID int64, pid string) {
	name := peakProviderName(pid)
	ws := b.peaks.windowsFor(pid)
	var text string
	if len(ws) > 0 {
		text = "⏰ ساعت پیک (گران) «" + name + "» را می‌شناسم:\n" + formatWindows(ws) +
			"\n\nهر وقت در این ساعات ازش استفاده کنی، قبل از اجرا یادآوری کنم؟"
	} else {
		text = "⏰ ساعت پیک (گران) «" + name + "» را نمی‌دانم.\nمی‌خواهی برایت پیدا و فعالش کنم تا موقع استفاده یادآوری شود؟"
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = rowsOf([]tgbotapi.InlineKeyboardButton{
		inlineBtn("بله، پیدا و فعال کن", "pk:add:"+pid),
		inlineBtn("نه، لازم نیست", "pk:no:"+pid),
	})
	b.api.Send(msg)
}

// sessionUsage آمار فعلی مصرف نشست را از سرور می‌خواند.
func (b *Bot) sessionUsage(sid string) (usage, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := b.oc.GetSession(ctx, sid)
	if err != nil || s == nil {
		return usage{}, false
	}
	model := s.Model.ID
	if model == "" {
		model = s.ModelID
	}
	return usage{
		in:       s.Tokens.Input,
		out:      s.Tokens.Output,
		cost:     s.Cost,
		model:    model,
		provider: s.Model.ProviderID,
	}, true
}

// sendRunSummary بعد از پایان هر اجرا یک پیام جدا زیر پاسخ می‌فرستد با نتیجه،
// مدل، مدت و مصرف توکن/هزینهٔ همین اجرا.
func (b *Bot) sendRunSummary(r *runCtl, res pollResult, pre usage, preOK bool) {
	secs := int(time.Since(r.started).Seconds())

	cur, curOK := b.sessionUsage(r.SID)
	in, out := 0, 0
	cost := 0.0
	switch {
	case curOK && preOK:
		// تفاضل مصرفِ همین اجرا
		in = cur.in - pre.in
		out = cur.out - pre.out
		cost = cur.cost - pre.cost
		if in < 0 {
			in = 0
		}
		if out < 0 {
			out = 0
		}
		if cost < 0 {
			cost = 0
		}
	case curOK:
		// قبل از اجرا قابل خواندن نبود؛ کل مصرف نشست را نشان بده
		in, out, cost = cur.in, cur.out, cur.cost
	}

	model := res.model
	provider := res.provider
	if model == "" && curOK {
		model = cur.model
	}
	if provider == "" && curOK {
		provider = cur.provider
	}
	if provider == "" {
		provider = b.providerFor("", model)
	}

	var sb strings.Builder
	switch res.outcome {
	case outcomeDone:
		sb.WriteString("✅ کار انجام شد\n")
	case outcomeStopped:
		sb.WriteString("⛔ اجرا متوقف شد\n")
	default:
		sb.WriteString("⚠️ اجرا ناتمام ماند\n")
	}
	sb.WriteString("━━━━━━━━━━━━━━━━\n")
	if model != "" {
		fmt.Fprintf(&sb, "🧠 مدل: %s\n", model)
	}
	fmt.Fprintf(&sb, "🤖 agent: %s\n", b.agentFor(r.UserID))
	fmt.Fprintf(&sb, "⏱ مدت: %s\n", durText(secs))
	if curOK {
		fmt.Fprintf(&sb, "🔢 توکن: ورودی %s · خروجی %s\n", abbrev(in), abbrev(out))
		fmt.Fprintf(&sb, "💵 هزینه: %s\n", b.costText(cost))
	} else {
		sb.WriteString("💵 آمار هزینه در دسترس نبود\n")
	}
	// اطلاعات پیک مصرف پروایدرِ همین مکالمه (ثبت + نمایش وضعیت)
	if status := b.peaks.statusText(provider, time.Now()); status != "" {
		sb.WriteString("━━━━━━━━━━━━━━━━\n")
		sb.WriteString(status)
	}
	b.sendSummaryWithDelete(r.ChatID, sb.String(), r.SID)
}

// deleteWindow مدت باقی‌ماندن دکمه‌های سریعِ پایان چت (فشرده‌سازی/بستن/حذف نشست)
// است؛ اگر کاربر در این بازه نزند، دکمه‌ها برداشته می‌شوند و نشست باقی می‌ماند.
const deleteWindow = 60 * time.Second

// sendSummaryWithDelete پیامِ پایان چت را با دکمه‌های فشرده‌سازی/بستن/حذف نشست
// می‌فرستد و بعد از deleteWindow دکمه‌ها را برمی‌دارد.
func (b *Bot) sendSummaryWithDelete(chatID int64, text, sid string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = sessionQuickMarkup(sid)
	m, err := b.api.Send(msg)
	if err != nil || m.MessageID == 0 {
		return
	}
	go func(chatID int64, msgID int) {
		time.Sleep(deleteWindow)
		b.removeInline(chatID, msgID)
	}(chatID, m.MessageID)
}

func (b *Bot) poll(ctx context.Context, r *runCtl, progressMsg int, startModel string) pollResult {
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	start := time.Now()
	lastShown := ""
	lastEdit := time.Time{}
	errCount := 0
	lastSig := ""
	lastChange := time.Now()
	model := startModel
	provider := ""
	for {
		select {
		case <-ctx.Done():
			return pollResult{outcome: outcomeStopped, text: "⛔ متوقف شد."}
		case now := <-ticker.C:
			msg, err := b.oc.LastMessage(ctx, r.SID)
			if err != nil {
				errCount++
				if errCount > 5 {
					return pollResult{outcome: outcomeError, text: "⚠️ ارتباط با opencode قطع شد: " + err.Error(), model: model, provider: provider}
				}
				continue
			}
			errCount = 0

			curText := ""
			act := ""
			if msg != nil && msg.Info.Role == "assistant" {
				if msg.Info.ModelID != "" {
					model = msg.Info.ModelID
				}
				if msg.Info.ProviderID != "" {
					provider = msg.Info.ProviderID
				}
				curText = joinText(msg)
				act = activityLabel(msg)
				if isFinalFinish(msg.Info.Finish) {
					if curText == "" {
						curText = "⚠️ پاسخی دریافت نشد."
					}
					return pollResult{outcome: outcomeDone, text: curText, model: model, provider: provider}
				}
				if hasPendingQuestion(msg) {
					if b.answerQuestion(ctx, r, progressMsg, curText, msg.Info.ID) {
						return pollResult{outcome: outcomeStopped, text: "⛔ متوقف شد.", model: model, provider: provider}
					}
					// بعد از پاسخ به سؤال، وضعیت زنده را از نو نشان بده
					lastShown = ""
					lastEdit = time.Time{}
					lastSig = ""
					lastChange = now
					continue
				}
			}

			// اگر مدل مدت طولانی هیچ متن/فعالیت تازه‌ای تولید نکرد،
			// احتمالاً پروایدر پاسخ نمی‌دهد؛ حلقه را بی‌نهایت ادامه نده.
			sig := curText + "\x00" + act
			if sig != lastSig {
				lastSig = sig
				lastChange = now
			} else if now.Sub(lastChange) >= pollQuietLimit {
				return pollResult{outcome: outcomeError, text: "⚠️ بیش از " + pollQuietLimit.String() + " است که مدل هیچ پاسخ یا فعالیتی تولید نکرده؛ احتمالاً پروایدر مشکل دارد.\nبا /new یک نشست تازه بساز یا مدل را در ⚙️ تنظیمات عوض کن.", model: model, provider: provider}
			}

			interval := 900 * time.Millisecond
			show := preview(curText)
			if curText == "" {
				interval = 2 * time.Second
				label := act
				if label == "" {
					// وقتی مدل دارد «فکر می‌کند»، اسمش را هم نشان بده
					if model != "" {
						label = "🧠 " + model + " در حال فکر کردن…"
					} else {
						label = "🧠 در حال فکر کردن…"
					}
				}
				show = label + "\n⏳ " + durText(int(now.Sub(start).Seconds()))
			}
			if show != lastShown && now.Sub(lastEdit) >= interval {
				b.edit(r.ChatID, progressMsg, show)
				lastShown = show
				lastEdit = now
			}
		}
	}
}

// ---------- مدیریت نشست‌ها ----------

func shortSID(sid string) string {
	s := strings.TrimPrefix(sid, "ses_")
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func (b *Bot) sessionLine(st *UserState, sid string) string {
	_, running := b.runFor(sid)
	line := "`" + shortSID(sid) + "`"
	if sid == st.SessionID {
		line += " ← فعال"
	}
	if running {
		line += " ⏳ در حال اجرا"
	}
	return line
}

// useSession نشست را فعال می‌کند و فهرست موضوعات مطرح‌شده در آن را نشان می‌دهد
func (b *Bot) useSession(userID, chatID int64, sid string) {
	b.setSession(userID, sid)
	name := b.sessionLabel(userID, sid)
	full := "✅ نشست فعال شد: " + name + "\n" + sid + "\n\n" + b.sessionTopicsText(sid)
	b.sendChunks(chatID, full, 0)
	b.sessionActions(chatID, sid)
}

// sessionActions نوار اکشن زیر توضیحات نشستِ بازشده: تغییر نام، حذف و (اگر در
// حال اجراست) توقف. زدن «تغییر نام» مسیر ss:rn را ادامه می‌دهد.
func (b *Bot) sessionActions(chatID int64, sid string) {
	var btns [][]tgbotapi.InlineKeyboardButton
	btns = append(btns, []tgbotapi.InlineKeyboardButton{
		inlineBtn("✏️ تغییر نام", "ss:rn:"+sid),
		inlineBtn("🗑 حذف نشست", "ss:del:"+sid),
	})
	if b.isRunning(sid) {
		btns = append(btns, []tgbotapi.InlineKeyboardButton{inlineBtn("⏹ توقف اجرا", "ss:stop:"+sid)})
	}
	act := tgbotapi.NewMessage(chatID, "📌 این نشست چه‌کاری؟")
	act.ReplyMarkup = rowsOf(btns...)
	b.api.Send(act)
}

// sessionTopicsText موضوعاتی که تاکنون در نشست مطرح شده‌اند (یک مورد برای هر پیام کاربر)
func (b *Bot) sessionTopicsText(sid string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	msgs, err := b.oc.ListMessages(ctx, sid, 1000)
	if err != nil {
		return "⚠️ فهرست موضوعات در دسترس نبود: " + err.Error()
	}
	var topics []string
	for _, m := range msgs {
		if m.Info.Role != "user" {
			continue
		}
		t := ""
		for _, p := range m.Parts {
			if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
				t = strings.TrimSpace(p.Text)
				break
			}
		}
		if i := strings.Index(t, "\n\nفایل پیوست:"); i > 0 {
			t = t[:i]
		}
		t = collapse(t)
		if t == "" {
			t = "(پیام بدون متن)"
		}
		topics = append(topics, clipHead(t, 140))
	}
	if len(topics) == 0 {
		return "📋 هنوز موضوعی در این نشست مطرح نشده."
	}
	const show = 50
	hidden := 0
	if len(topics) > show {
		hidden = len(topics) - show
		topics = topics[len(topics)-show:]
	}
	var sb strings.Builder
	if hidden > 0 {
		fmt.Fprintf(&sb, "📋 موضوعات این نشست (آخرین %d از %d مورد):\n", show, hidden+show)
	} else {
		fmt.Fprintf(&sb, "📋 موضوعات مطرح‌شده در این نشست (%d مورد):\n", len(topics))
	}
	start := hidden + 1
	for i, t := range topics {
		fmt.Fprintf(&sb, "%s. %s\n", faNum(start+i), t)
	}
	return strings.TrimRight(sb.String(), "\n")
}

func (b *Bot) openSessions(userID, chatID int64, msgID int) {
	b.sessionsPage(userID, chatID, msgID, 0)
}

// lastSSPage آخرین صفحه‌ای که کاربر در مدیر نشست‌ها دیده را برمی‌گرداند
func (b *Bot) lastSSPage(chatID int64) int {
	return b.ui.ssPage(chatID)
}

func (b *Bot) setSSPage(chatID int64, page int) {
	b.ui.setSSPage(chatID, page)
}

func (b *Bot) setGroupMode(chatID int64, on bool) {
	b.ui.setGroupMode(chatID, on)
}

func (b *Bot) groupMode(chatID int64) bool {
	return b.ui.groupMode(chatID)
}

func (b *Bot) groupSel(chatID int64, sid string) bool {
	return b.ui.groupSel(chatID, sid)
}

func (b *Bot) toggleGroupSel(chatID int64, sid string) {
	b.ui.toggleGroupSel(chatID, sid)
}

func (b *Bot) clearGroupSel(chatID int64) {
	b.ui.clearGroupSel(chatID)
}

func (b *Bot) groupSelCount(chatID int64) int {
	return b.ui.groupSelCount(chatID)
}

func (b *Bot) groupSelList(chatID int64) []string {
	return b.ui.groupSelList(chatID)
}

// sessionsAtSamePage مدیر نشست‌ها را در همان صفحهٔ قبلی دوباره باز می‌کند
func (b *Bot) sessionsAtSamePage(userID, chatID int64, msgID int) {
	b.sessionsPage(userID, chatID, msgID, b.lastSSPage(chatID))
}

// sessionsPage یک صفحه از مدیر نشست‌ها را نشان می‌دهد؛ ابتدا از سرور همگام می‌شود
func (b *Bot) sessionsPage(userID, chatID int64, msgID, page int) {
	b.users.ensure(userID, chatID)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	list, err := b.oc.ListSessions(ctx, sessionsFetchMax)
	cancel()
	var warn error
	if err == nil {
		b.users.rememberActivity(list)
		b.users.syncLive(userID, list, b.isRunning)
	} else {
		warn = err
	}

	ids, active := b.users.snapshotSessions(userID)

	titles := make(map[string]string, len(list))
	for _, s := range list {
		if t := strings.TrimSpace(s.Title); t != "" {
			titles[s.ID] = t
		}
	}
	// ترتیب نمایش را نگه می‌داریم تا شمارهٔ بعدیِ کاربر به همان نشست نگاشت شود
	b.ui.setSSList(chatID, ids)

	if len(ids) == 0 {
		text := "🗂 <b>نشست‌ها</b>\n\n"
		if warn != nil {
			text += "⚠️ سرور در دسترس نبود.\n"
		}
		text += "هنوز نشستی روی سرور نیست.\n«➕ نشست جدید» را بزن یا یک متن بفرست تا خودکار ساخته شود."
		kb := rowsOf([]tgbotapi.InlineKeyboardButton{inlineBtn("➕ نشست جدید", "ss:new")})
		if msgID == 0 {
			msg := tgbotapi.NewMessage(chatID, text)
			msg.ParseMode = "HTML"
			msg.ReplyMarkup = kb
			b.api.Send(msg)
		} else {
			edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
			edit.ParseMode = "HTML"
			edit.ReplyMarkup = kb
			b.api.Send(edit)
		}
		return
	}

	total := len(ids)
	pages := (total + sessionsPageSize - 1) / sessionsPageSize
	if page < 0 {
		page = 0
	}
	if page >= pages {
		page = pages - 1
	}
	start := page * sessionsPageSize
	end := start + sessionsPageSize
	if end > total {
		end = total
	}
	b.setSSPage(chatID, page)

	var sb strings.Builder
	sb.WriteString("🗂 <b>نشست‌ها</b>")
	if pages > 1 {
		fmt.Fprintf(&sb, "  (صفحه %s از %s)", faNum(page+1), faNum(pages))
	}
	sb.WriteString("\n")
	if warn != nil {
		sb.WriteString("⚠️ سرور در دسترس نبود؛ نشست‌های ذخیره‌شده نمایش داده می‌شود.\n")
	}
	for i := start; i < end; i++ {
		sid := ids[i]
		_, running := b.runFor(sid)
		line := fmt.Sprintf("%s. %s", faNum(i+1), html.EscapeString(b.sessionTitle(userID, sid, titles)))
		if sid == active {
			line += "  ← فعال"
		}
		if running {
			line += "  ⏳"
		}
		sb.WriteString(line + "\n")
	}
	sb.WriteString("\nبرای حذف/ویرایش/مشاهده، دکمهٔ مربوط را بزن و بعد عدد نشست را بفرست.")

	var rows [][]tgbotapi.InlineKeyboardButton
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		inlineBtn("🗑 حذف", "ss:pick:del"),
		inlineBtn("✏️ ویرایش", "ss:pick:rn"),
		inlineBtn("👁 مشاهده", "ss:pick:view"),
	})
	rows = append(rows, []tgbotapi.InlineKeyboardButton{inlineBtn("🔀 عملیات گروهی", "ss:pick:grp")})
	if pages > 1 {
		nav := []tgbotapi.InlineKeyboardButton{}
		if page > 0 {
			nav = append(nav, inlineBtn("◀️ قبلی", "ss:pg:"+strconv.Itoa(page-1)))
		} else {
			nav = append(nav, inlineBtn("⏺", "ss:noop"))
		}
		nav = append(nav, inlineBtn(fmt.Sprintf("%s/%s", faNum(page+1), faNum(pages)), "ss:noop"))
		if page+1 < pages {
			nav = append(nav, inlineBtn("بعدی ▶️", "ss:pg:"+strconv.Itoa(page+1)))
		} else {
			nav = append(nav, inlineBtn("⏺", "ss:noop"))
		}
		rows = append(rows, nav)
	}
	rows = append(rows,
		[]tgbotapi.InlineKeyboardButton{inlineBtn("➕ نشست جدید", "ss:new"), inlineBtn("🔄 تازه‌سازی", "ss:refresh"), inlineBtn("❌ بستن", "ss:close")},
	)

	kb := rowsOf(rows...)
	if msgID == 0 {
		msg := tgbotapi.NewMessage(chatID, sb.String())
		msg.ParseMode = "HTML"
		msg.ReplyMarkup = kb
		b.api.Send(msg)
		return
	}
	edit := tgbotapi.NewEditMessageText(chatID, msgID, sb.String())
	edit.ParseMode = "HTML"
	edit.ReplyMarkup = kb
	b.api.Send(edit)
}

func (b *Bot) onSessionsCallback(userID, chatID int64, msgID int, data string) {
	switch data {
	case "ss:refresh":
		b.ui.clearSSSel(chatID)
		b.ui.clearSSPick(chatID)
		b.sessionsAtSamePage(userID, chatID, msgID)
	case "ss:close":
		edit := tgbotapi.NewEditMessageText(chatID, msgID, "بسته شد.")
		b.api.Send(edit)
	case "ss:new":
		b.edit(chatID, msgID, "در حال ساخت نشست جدید…")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		s, err := b.oc.CreateSession(ctx)
		if err != nil {
			b.edit(chatID, msgID, "ساخت نشست ممکن نشد: "+err.Error())
			return
		}
		b.setSession(userID, s.ID)
		b.send(chatID, "✅ نشست جدید ساخته و فعال شد:\n"+b.sessionLabel(userID, s.ID)+"\n"+s.ID)
		b.sessionsAtSamePage(userID, chatID, msgID)
	case "ss:gm":
		b.setGroupMode(chatID, true)
		b.clearGroupSel(chatID)
		b.sessionsAtSamePage(userID, chatID, msgID)
	case "ss:gcncl":
		b.setGroupMode(chatID, false)
		b.clearGroupSel(chatID)
		b.sessionsAtSamePage(userID, chatID, msgID)
	case "ss:gdel":
		b.openGroupDeleteConfirm(userID, chatID, msgID)
	default:
		parts := strings.SplitN(data, ":", 3)
		if len(parts) < 3 {
			return
		}
		switch parts[1] {
		case "pg":
			if p, err := strconv.Atoi(parts[2]); err == nil {
				b.sessionsPage(userID, chatID, msgID, p)
			}
		case "pick":
			// باز کردن انتخابگر دکمه‌ای؛ grp یعنی حالت گروهی بدون عملِ پیش‌فرض
			act := parts[2]
			if act == "grp" {
				act = ""
			}
			b.ui.startSSPick(chatID, act, b.lastSSPage(chatID))
			b.renderSessionsPicker(chatID, msgID)
		case "tgl":
			if n, err := strconv.Atoi(parts[2]); err == nil {
				b.ui.toggleSSPick(chatID, n)
				b.renderSessionsPicker(chatID, msgID)
			}
		case "allsel":
			_, page, _ := b.ui.ssPick(chatID)
			start, end := sessionsBounds(page, len(b.ui.ssList(chatID)))
			b.ui.setSSPickAll(chatID, pickNumbers(start, end), true)
			b.renderSessionsPicker(chatID, msgID)
		case "none":
			_, page, _ := b.ui.ssPick(chatID)
			start, end := sessionsBounds(page, len(b.ui.ssList(chatID)))
			b.ui.setSSPickAll(chatID, pickNumbers(start, end), false)
			b.renderSessionsPicker(chatID, msgID)
		case "ok":
			act, _, _ := b.ui.ssPick(chatID)
			sids := b.pickedSids(chatID)
			if len(sids) == 0 {
				b.send(chatID, "هیچ نشستی انتخاب نشده؛ اول شماره‌ها را بزن.")
				return
			}
			b.ui.clearSSPick(chatID)
			b.applySessionsAction(userID, chatID, msgID, act, sids)
		case "go":
			sids := b.pickedSids(chatID)
			if len(sids) == 0 {
				b.send(chatID, "هیچ نشستی انتخاب نشده؛ اول شماره‌ها را بزن.")
				return
			}
			b.ui.clearSSPick(chatID)
			b.applySessionsAction(userID, chatID, msgID, parts[2], sids)
		case "use":
			sid := parts[2]
			b.useSession(userID, chatID, sid)
			b.sessionsAtSamePage(userID, chatID, msgID)
		case "rn":
			sid := parts[2]
			b.setPendingByChat(chatID, "rn:"+sid)
			edit := tgbotapi.NewEditMessageText(chatID, msgID, "✏️ نام جدید این نشست را بفرست (فارسی یا هر اسم دلخواه):")
			b.api.Send(edit)
		case "del":
			sid := parts[2]
			edit := tgbotapi.NewEditMessageText(chatID, msgID, "🗑 نشست «"+b.sessionLabel(userID, sid)+"» برای همیشه حذف شود؟\nاین نشست و تمام گفتگویش از روی سرور opencode پاک می‌شود (غیرقابل بازگشت). اجرای در جریان هم متوقف می‌شود.")
			edit.ReplyMarkup = rowsOf(
				[]tgbotapi.InlineKeyboardButton{inlineBtn("🗑 بله، حذف کن", "ss:delc:"+sid)},
				[]tgbotapi.InlineKeyboardButton{inlineBtn("انصراف", "ss:refresh")},
			)
			b.api.Send(edit)
		case "delc":
			var sids []string
			if parts[2] == "all" {
				_, sids = b.ui.ssSel(chatID)
			} else {
				sids = []string{parts[2]}
			}
			b.deleteSessionsBySids(userID, chatID, msgID, sids)
		case "gtgl":
			b.toggleGroupSel(chatID, parts[2])
			b.sessionsAtSamePage(userID, chatID, msgID)
		case "gdelc":
			b.deleteGroupSessions(userID, chatID, msgID)
		case "stop":
			if b.stopRun(parts[2]) {
				b.send(chatID, "اجرای نشست متوقف شد.")
			}
			b.sessionsAtSamePage(userID, chatID, msgID)
		}
	}
}

// openGroupDeleteConfirm تأیید حذف دسته‌جمعی نشست‌های انتخاب‌شده را نشان می‌دهد
func (b *Bot) openGroupDeleteConfirm(userID, chatID int64, msgID int) {
	sids := b.groupSelList(chatID)
	if len(sids) == 0 {
		edit := tgbotapi.NewEditMessageText(chatID, msgID, "هیچ نشستی انتخاب نشده. روی نشست‌ها بزن تا انتخاب شوند.")
		edit.ReplyMarkup = rowsOf([]tgbotapi.InlineKeyboardButton{inlineBtn("🔄 بازگشت", "ss:refresh")})
		b.api.Send(edit)
		return
	}
	var names []string
	for i, sid := range sids {
		if i >= 8 {
			names = append(names, "…")
			break
		}
		names = append(names, "• "+b.sessionLabel(userID, sid))
	}
	text := fmt.Sprintf("🗑 <b>%s نشست</b> برای همیشه حذف شوند؟\nاین نشست‌ها و تمام گفتگویشان از روی سرور opencode پاک می‌شود (غیرقابل بازگشت). اجرای در جریان هم متوقف می‌شود.\n\n%s", faNum(len(sids)), strings.Join(names, "\n"))
	edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
	edit.ParseMode = "HTML"
	edit.ReplyMarkup = rowsOf(
		[]tgbotapi.InlineKeyboardButton{inlineBtn("🗑 بله، همه را حذف کن", "ss:gdelc")},
		[]tgbotapi.InlineKeyboardButton{inlineBtn("انصراف", "ss:refresh")},
	)
	b.api.Send(edit)
}

// deleteGroupSessions نشست‌های انتخاب‌شده را یک‌به‌یک روی سرور حذف می‌کند
func (b *Bot) deleteGroupSessions(userID, chatID int64, msgID int) {
	sids := b.groupSelList(chatID)
	if len(sids) == 0 {
		b.sessionsAtSamePage(userID, chatID, msgID)
		return
	}
	b.clearGroupSel(chatID)
	b.setGroupMode(chatID, false)
	b.edit(chatID, msgID, "در حال حذف "+faNum(len(sids))+" نشست…")

	ok, fail := 0, 0
	var errMsg []string
	for _, sid := range sids {
		b.stopRun(sid)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := b.oc.DeleteSession(ctx, sid)
		cancel()
		if err != nil {
			fail++
			if len(errMsg) < 3 {
				errMsg = append(errMsg, "• "+shortSID(sid)+": "+err.Error())
			}
			continue
		}
		b.deleteSession(userID, sid)
		ok++
	}
	text := fmt.Sprintf("🗑 حذف گروهی انجام شد: %s نشست حذف شد.", faNum(ok))
	if fail > 0 {
		text += fmt.Sprintf("\n⚠️ %s نشست حذف نشد.", faNum(fail))
		if len(errMsg) > 0 {
			text += "\n" + strings.Join(errMsg, "\n")
		}
	}
	b.send(chatID, text)
	b.sessionsPage(userID, chatID, msgID, 0)
}

// deleteSessionsBySids نشست‌های داده‌شده را یک‌به‌یک روی سرور حذف می‌کند و
// نتیجه را گزارش می‌دهد.
func (b *Bot) deleteSessionsBySids(userID, chatID int64, msgID int, sids []string) {
	b.ui.clearSSSel(chatID)
	if len(sids) == 0 {
		b.sessionsAtSamePage(userID, chatID, msgID)
		return
	}
	if msgID != 0 {
		b.edit(chatID, msgID, "در حال حذف "+faNum(len(sids))+" نشست…")
	}
	ok, fail := 0, 0
	var errMsg []string
	for _, sid := range sids {
		b.stopRun(sid)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := b.oc.DeleteSession(ctx, sid)
		cancel()
		if err != nil {
			fail++
			if len(errMsg) < 3 {
				errMsg = append(errMsg, "• "+shortSID(sid)+": "+err.Error())
			}
			continue
		}
		b.deleteSession(userID, sid)
		ok++
	}
	text := fmt.Sprintf("🗑 %s نشست حذف شد.", faNum(ok))
	if fail > 0 {
		text += fmt.Sprintf("\n⚠️ %s نشست حذف نشد.", faNum(fail))
		if len(errMsg) > 0 {
			text += "\n" + strings.Join(errMsg, "\n")
		}
	}
	b.send(chatID, text)
	b.sessionsPage(userID, chatID, msgID, 0)
}

func isFinalFinish(finish string) bool {
	switch finish {
	case "", "pending", "tool-calls":
		return false
	}
	return true
}

func hasPendingQuestion(msg *occlient.Message) bool {
	for _, p := range msg.Parts {
		if p.Type != "tool" || p.Tool != "question" || p.State == nil {
			continue
		}
		if p.State.Status == "completed" || p.State.Status == "error" {
			continue
		}
		return true
	}
	return false
}

func joinText(msg *occlient.Message) string {
	var sb strings.Builder
	for _, p := range msg.Parts {
		if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
			sb.WriteString(p.Text)
			sb.WriteString("\n")
		}
	}
	return strings.TrimSpace(sb.String())
}

func preview(text string) string {
	if text == "" {
		return "🧠 در حال فکر کردن…"
	}
	r := []rune(text)
	const max = 3500
	if len(r) > max {
		return string(r[:max]) + "\n… (در حال تولید)"
	}
	return text + "\n… (در حال تولید)"
}

func activityLabel(msg *occlient.Message) string {
	reasoning := ""
	lastTool := ""
	for i := len(msg.Parts) - 1; i >= 0; i-- {
		p := msg.Parts[i]
		switch p.Type {
		case "reasoning":
			if reasoning == "" {
				reasoning = collapse(p.Text)
			}
		case "tool":
			if label := toolLabel(p); label != "" && lastTool == "" {
				lastTool = label
			}
		}
	}
	var lines []string
	if reasoning != "" {
		lines = append(lines, "🤔 "+clipTail(reasoning, 180))
	}
	if lastTool != "" {
		lines = append(lines, lastTool)
	}
	return strings.Join(lines, "\n")
}

func toolLabel(p occlient.Part) string {
	name := p.Tool
	if name == "" {
		return ""
	}
	status := ""
	target := ""
	if p.State != nil {
		status = p.State.Status
		for _, k := range []string{"command", "filePath", "file_path", "path", "query"} {
			if v, ok := p.State.Input[k].(string); ok && strings.TrimSpace(v) != "" {
				target = v
				break
			}
		}
		if target == "" {
			target = p.State.Title
		}
	}
	target = clipHead(collapse(target), 90)
	icon := "🔄"
	switch status {
	case "completed":
		icon = "✅"
	case "error":
		icon = "❌"
	}
	if target != "" {
		return fmt.Sprintf("%s %s: %s", icon, name, target)
	}
	return fmt.Sprintf("%s %s", icon, name)
}

func collapse(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

func clipHead(s string, n int) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func clipTail(s string, n int) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}

func durText(secs int) string {
	if secs < 60 {
		return strconv.Itoa(secs) + " ثانیه"
	}
	return fmt.Sprintf("%d دقیقه و %d ثانیه", secs/60, secs%60)
}
