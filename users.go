package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"negahban-opencode/internal/occlient"
	"negahban-opencode/internal/statefile"
)

// UserState state ماندگار هر کاربر است (در state.json ذخیره می‌شود).
type UserState struct {
	UserID    int64             `json:"user_id"`
	ChatID    int64             `json:"chat_id"`
	SessionID string            `json:"session_id,omitempty"`
	Sessions  []string          `json:"sessions,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"` // نام نمایشی نشست‌ها
	Manual    map[string]bool   `json:"manual,omitempty"` // نشست‌هایی که کاربر با ✏️ دستی نامشان را عوض کرده
	Agent     string            `json:"agent,omitempty"`
	Pending   string            `json:"pending,omitempty"`
}

// defaultSessionName نام پیش‌فرض نشست: فقط شماره (۱، ۲، ۳…)
func defaultSessionName(n int) string {
	return faNum(n)
}

// renumberAutoLabels نشست‌های بدون نام دستی را بر اساس جایگاهشان شماره‌گذاری می‌کند
// (نام‌هایی که کاربر با ✏️ گذاشته دست‌نخورده می‌مانند)
func renumberAutoLabels(st *UserState) {
	if st.Labels == nil {
		st.Labels = map[string]string{}
	}
	for i, sid := range st.Sessions {
		if st.Manual != nil && st.Manual[sid] {
			continue
		}
		st.Labels[sid] = defaultSessionName(i + 1)
	}
}

func addSessionID(list []string, id string) []string {
	for _, s := range list {
		if s == id {
			return list
		}
	}
	return append(list, id)
}

// userStore مالک state کاربران، کش فعالیت نشست‌ها و ذخیره‌سازی اتمیک/جمع‌شدهٔ آن است.
// همهٔ تغییرات state باید از متدهای همین ساختار بگذرند تا زیر قفل‌اش باشند.
type userStore struct {
	mu     sync.Mutex
	path   string
	states map[int64]*UserState
	act    map[string]int64 // زمان آخرین فعالیت نشست (epoch ms) — برای برچسب خودکار تاریخ‌دار
	saver  *statefile.Saver
}

func newUserStore(path string) *userStore {
	u := &userStore{path: path, states: map[int64]*UserState{}, act: map[string]int64{}}
	u.saver = statefile.NewSaver(path, func() ([]byte, error) {
		u.mu.Lock()
		defer u.mu.Unlock()
		return json.Marshal(u.states)
	})
	u.saver.SetErrorHandler(func(err error) {
		slog.Error("ذخیرهٔ state روی دیسک ناموفق بود", "path", path, "error", err)
	})
	return u
}

// load stateها را از دیسک می‌خواند، هنجار می‌کند و در صورت نیاز دوباره ذخیره می‌کند.
func (u *userStore) load() error {
	data, err := os.ReadFile(u.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := json.Unmarshal(data, &u.states); err != nil {
		return err
	}
	// نام/برچسب نشست‌ها را از نو هنجار کن (نام پیش‌فرض شماره است) و ذخیره کن
	u.mu.Lock()
	for _, st := range u.states {
		if st.Labels == nil {
			st.Labels = map[string]string{}
		}
		// مطمئن شو نشست فعال در لیست هست
		if st.SessionID != "" {
			st.Sessions = addSessionID(st.Sessions, st.SessionID)
		}
		renumberAutoLabels(st)
	}
	u.mu.Unlock()
	u.saver.MarkDirty()
	return nil
}

// Close همهٔ تغییرات در انتظار را قطعی می‌نویسد و ذخیره‌کننده را می‌بندد.
func (u *userStore) Close() error {
	return u.saver.Close()
}

// ensure state کاربر را برمی‌گرداند و اگر نبود می‌سازد.
func (u *userStore) ensure(userID, chatID int64) *UserState {
	u.mu.Lock()
	defer u.mu.Unlock()
	st, ok := u.states[userID]
	if !ok {
		st = &UserState{UserID: userID, ChatID: chatID}
		u.states[userID] = st
		u.saver.MarkDirty()
		return st
	}
	if st.ChatID != chatID {
		st.ChatID = chatID
		u.saver.MarkDirty()
	}
	return st
}

func (u *userStore) byID(userID int64) *UserState {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.states[userID]
}

// activeSession نشست فعال کاربر را برمی‌گرداند (خالی اگر نشستی فعال نیست).
func (u *userStore) activeSession(userID int64) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if st := u.states[userID]; st != nil {
		return st.SessionID
	}
	return ""
}

// snapshotSessions یک کپی از فهرست نشست‌ها و نشست فعال کاربر می‌دهد.
func (u *userStore) snapshotSessions(userID int64) ([]string, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if st := u.states[userID]; st != nil {
		return append([]string(nil), st.Sessions...), st.SessionID
	}
	return nil, ""
}

// userForChat کاربرِ دارای این چت را پیدا می‌کند (برای برچسب هزینه روی کیبورد).
func (u *userStore) userForChat(chatID int64) int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	for uid, st := range u.states {
		if st.ChatID == chatID {
			return uid
		}
	}
	return 0
}

// activate نشست را به فهرست اضافه و فعال می‌کند (نام پیش‌فرض می‌گیرد اگر نداشت).
func (u *userStore) activate(userID int64, id string) {
	if id == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.states[userID]
	if st == nil {
		return
	}
	if !containsString(st.Sessions, id) {
		st.Sessions = append(st.Sessions, id)
	}
	st.SessionID = id
	if st.Labels == nil {
		st.Labels = map[string]string{}
	}
	if st.Labels[id] == "" {
		st.Labels[id] = defaultSessionName(len(st.Sessions))
	}
	u.saver.MarkDirty()
}

// rename نام دستی (با ✏️) نشست را ثبت می‌کند.
func (u *userStore) rename(userID int64, sid, label string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.states[userID]
	if st == nil {
		return
	}
	if st.Labels == nil {
		st.Labels = map[string]string{}
	}
	if st.Manual == nil {
		st.Manual = map[string]bool{}
	}
	st.Labels[sid] = label
	st.Manual[sid] = true
	u.saver.MarkDirty()
}

// manualLabel نام دستیِ ثبت‌شدهٔ یک نشست را برمی‌گرداند (اگر کاربر با ✏️ عوض کرده باشد).
func (u *userStore) manualLabel(userID int64, sid string) (string, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.states[userID]
	if st == nil || st.Manual == nil || !st.Manual[sid] {
		return "", false
	}
	return st.Labels[sid], true
}

// deactivate نشست فعال را غیرفعال می‌کند بدون حذف از فهرست یا سرور؛ پیام بعدی
// نشست تازه می‌سازد و کاربر بعداً با مدیر نشست‌ها می‌تواند به آن برگردد.
func (u *userStore) deactivate(userID int64, sid string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.states[userID]
	if st == nil {
		return
	}
	if st.SessionID == sid {
		st.SessionID = ""
		u.saver.MarkDirty()
	}
}

// remove نشست را از فهرست کاربر حذف می‌کند (بدون دست زدن به اجرا/سرور).
func (u *userStore) remove(userID int64, sid string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.states[userID]
	if st == nil {
		return
	}
	keep := st.Sessions[:0]
	for _, s := range st.Sessions {
		if s != sid {
			keep = append(keep, s)
		}
	}
	st.Sessions = keep
	if st.Labels != nil {
		delete(st.Labels, sid)
	}
	if st.Manual != nil {
		delete(st.Manual, sid)
	}
	if st.SessionID == sid {
		st.SessionID = ""
	}
	renumberAutoLabels(st)
	u.saver.MarkDirty()
}

func (u *userStore) setAgent(userID int64, agent string) {
	if agent == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if st := u.states[userID]; st != nil {
		st.Agent = agent
		u.saver.MarkDirty()
	}
}

func (u *userStore) agentOf(userID int64) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if st := u.states[userID]; st != nil {
		return st.Agent
	}
	return ""
}

// ---------- pending (انتظارِ تایپ متن برای مدل/نام/کلید و…) ----------

// setPending متنِ در انتظار را روی state کاربرِ دارای این چت می‌نشاند.
func (u *userStore) setPending(chatID int64, pending string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, st := range u.states {
		if st.ChatID == chatID {
			st.Pending = pending
			u.saver.MarkDirty()
			return
		}
	}
}

// pendingForChat pendingِ کاربر دارای این چت را برمی‌گرداند (بدون پاک کردن).
func (u *userStore) pendingForChat(chatID int64) (userID int64, pending string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for uid, st := range u.states {
		if st.ChatID == chatID && st.Pending != "" {
			return uid, st.Pending
		}
	}
	return 0, ""
}

func (u *userStore) clearPending(userID int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if st := u.states[userID]; st != nil && st.Pending != "" {
		st.Pending = ""
		u.saver.MarkDirty()
	}
}

// clearPendingToken پاک کردن pending از نوع «qtext:<token>» (پایان سؤال تعاملی).
func (u *userStore) clearPendingToken(token string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, st := range u.states {
		if st.Pending == "qtext:"+token {
			st.Pending = ""
			u.saver.MarkDirty()
			return
		}
	}
}

// ---------- برچسب نشست و کش فعالیت ----------

// lastActivityOfOC آخرین فعالیت یک نشست سرور را می‌دهد؛ اگر سرور آن را ندهد به زمان ساخت برمی‌گردد
func lastActivityOfOC(s *occlient.Session) int64 {
	if s.Time.Updated > 0 {
		return s.Time.Updated
	}
	return s.Time.Created
}

func (u *userStore) setActivity(sid string, ms int64) {
	if sid == "" || ms <= 0 {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.act[sid] = ms
}

func (u *userStore) rememberActivity(list []occlient.Session) {
	if len(list) == 0 {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, s := range list {
		if ms := lastActivityOfOC(&s); ms > 0 {
			u.act[s.ID] = ms
		}
	}
}

// sessionLabel نام نمایشی نشست را برمی‌گرداند؛ نام دستی کاربر همان می‌ماند و
// نام خودکار به «تاریخ و ساعت آخرین فعالیت» (شمسی، به وقت ایران) تبدیل می‌شود.
func (u *userStore) sessionLabel(userID int64, sid string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	st, ok := u.states[userID]
	if !ok {
		return shortSID(sid)
	}
	if st.Manual != nil && st.Manual[sid] {
		if l := st.Labels[sid]; l != "" {
			return l
		}
	}
	if ms, ok := u.act[sid]; ok {
		return persianTimeLabel(ms, time.Now())
	}
	if st.Labels != nil {
		if l := st.Labels[sid]; l != "" {
			return l
		}
	}
	for i, s := range st.Sessions {
		if s == sid {
			return defaultSessionName(i + 1)
		}
	}
	return shortSID(sid)
}

// ---------- همگام‌سازی زنده با سرور ----------

// syncLive فهرست نشست‌های کاربر را با sessionهای واقعی سرور همگام می‌کند:
// نشست‌های تازهٔ سرور (مثل نشست‌های ساخته‌شده از CLI) اضافه و نشست‌های حذف‌شده
// حذف می‌شوند. شماره‌گذاری و نام‌های دستی کاربر دست‌نخورده می‌مانند.
// isRunning مشخص می‌کند آیا نشستی همین حالا در حال اجراست (تا در برش حذف نشود).
func (u *userStore) syncLive(userID int64, list []occlient.Session, isRunning func(string) bool) {
	if isRunning == nil {
		isRunning = func(string) bool { return false }
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.states[userID]
	if st == nil {
		return
	}
	onSrv := make(map[string]bool, len(list))
	order := make([]string, 0, len(list))
	for _, s := range list {
		onSrv[s.ID] = true
		order = append(order, s.ID) // سرور به‌ترتیب «آخرین فعالیت» برمی‌گرداند
	}
	if st.Labels == nil {
		st.Labels = map[string]string{}
	}
	if st.Manual == nil {
		st.Manual = map[string]bool{}
	}

	// نشست فعال اگر روی سرور حذف شده باشد دیگر معتبر نیست
	if st.SessionID != "" && !onSrv[st.SessionID] {
		st.SessionID = ""
	}

	// نشست‌هایی که دیگر روی سرور نیستند از فهرست حذف شوند
	keep := st.Sessions[:0]
	for _, sid := range st.Sessions {
		if onSrv[sid] {
			keep = append(keep, sid)
		}
	}
	st.Sessions = keep

	// نشست‌های تازهٔ سرور به انتهای فهرست افزوده شوند (تا شماره‌ها ثابت بمانند)
	have := make(map[string]bool, len(st.Sessions))
	for _, sid := range st.Sessions {
		have[sid] = true
	}
	for _, sid := range order {
		if have[sid] {
			continue
		}
		st.Sessions = append(st.Sessions, sid)
		have[sid] = true
	}

	// برش به حداکثر مجاز؛ نشست فعال/درحال‌اجرا/نام‌گذاری‌شده هرگز حذف نمی‌شود
	if len(st.Sessions) > maxTrackedSessions {
		cut := st.Sessions[:0]
		over := len(st.Sessions) - maxTrackedSessions
		for _, sid := range st.Sessions {
			if over > 0 && sid != st.SessionID && !isRunning(sid) && !st.Manual[sid] {
				over--
				continue
			}
			cut = append(cut, sid)
		}
		st.Sessions = cut
	}

	// پاکسازی برچسب نشست‌هایی که دیگر در فهرست نیستند
	st.Sessions = addSessionID(st.Sessions, st.SessionID)
	kept := make(map[string]bool, len(st.Sessions))
	for _, sid := range st.Sessions {
		kept[sid] = true
	}
	for sid := range st.Labels {
		if !kept[sid] {
			delete(st.Labels, sid)
		}
	}
	for sid := range st.Manual {
		if !kept[sid] {
			delete(st.Manual, sid)
		}
	}
	renumberAutoLabels(st)
	u.saver.MarkDirty()
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
