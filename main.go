package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

var db *sql.DB

type Member struct {
	Name     string
	UserID   sql.NullInt64
	Username string
}

// ── Database ──────────────────────────────────────────────────────────────────

func initDB() error {
	var err error
	db, err = sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	if err = db.Ping(); err != nil {
		return fmt.Errorf("ping: %w", err)
	}

	// جدول اعضا — هر اسم در هر گروه یکتاست
	if _, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS members (
			id         SERIAL PRIMARY KEY,
			group_id   BIGINT       NOT NULL,
			name       VARCHAR(100) NOT NULL,
			user_id    BIGINT,
			username   VARCHAR(100),
			added_by   BIGINT,
			created_at TIMESTAMP DEFAULT NOW(),
			UNIQUE(group_id, name)
		)
	`); err != nil {
		return fmt.Errorf("create members: %w", err)
	}

	// جدول تنظیمات گروه
	if _, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS group_settings (
			group_id             BIGINT PRIMARY KEY,
			allow_admin_register BOOLEAN NOT NULL DEFAULT FALSE,
			updated_at           TIMESTAMP DEFAULT NOW()
		)
	`); err != nil {
		return fmt.Errorf("create settings: %w", err)
	}

	return nil
}

func getMembers(groupID int64) ([]Member, error) {
	rows, err := db.Query(
		`SELECT name, user_id, COALESCE(username,'')
		 FROM members WHERE group_id=$1 ORDER BY id`,
		groupID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.Name, &m.UserID, &m.Username); err != nil {
			continue
		}
		list = append(list, m)
	}
	return list, nil
}

// upsertMember: ثبت یا آپدیت عضو
// اگه اسم قبلاً ثبت شده: user_id رو آپدیت میکنه (اگه جدیدتر باشه)
func upsertMember(groupID, addedBy int64, name string, userID sql.NullInt64, username string) error {
	_, err := db.Exec(`
		INSERT INTO members (group_id, name, user_id, username, added_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (group_id, name) DO UPDATE SET
			user_id  = COALESCE(EXCLUDED.user_id, members.user_id),
			username = COALESCE(EXCLUDED.username, members.username)
	`, groupID, name, userID, nullStr(username), addedBy)
	return err
}

// autoResolve: وقتی کسی پیام میده و @username مچ خورد، آیدی عددیشو ذخیره میکنه
func autoResolve(groupID int64, username string, realUserID int64) {
	if username == "" {
		return
	}
	result, err := db.Exec(`
		UPDATE members SET user_id=$1
		WHERE group_id=$2 AND username=$3 AND user_id IS NULL
	`, realUserID, groupID, username)
	if err == nil {
		if n, _ := result.RowsAffected(); n > 0 {
			log.Printf("✅ auto-resolved @%s → %d (group %d)", username, realUserID, groupID)
		}
	}
}

func deleteMember(groupID int64, name string) (bool, error) {
	res, err := db.Exec(`DELETE FROM members WHERE group_id=$1 AND name=$2`, groupID, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func deleteAllMembers(groupID int64) error {
	_, err := db.Exec(`DELETE FROM members WHERE group_id=$1`, groupID)
	return err
}

func getAllowAdmin(groupID int64) bool {
	var allow bool
	err := db.QueryRow(`SELECT allow_admin_register FROM group_settings WHERE group_id=$1`, groupID).Scan(&allow)
	return err == nil && allow
}

func setAllowAdmin(groupID int64, allow bool) error {
	_, err := db.Exec(`
		INSERT INTO group_settings (group_id, allow_admin_register)
		VALUES ($1,$2)
		ON CONFLICT (group_id) DO UPDATE SET
			allow_admin_register=EXCLUDED.allow_admin_register, updated_at=NOW()
	`, groupID, allow)
	return err
}

func nullStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func isOwner(botAPI *tgbotapi.BotAPI, chatID int64, userID int) bool {
	cm, err := botAPI.GetChatMember(tgbotapi.ChatConfigWithUser{ChatID: chatID, UserID: userID})
	return err == nil && cm.IsCreator()
}

func isAdmin(botAPI *tgbotapi.BotAPI, chatID int64, userID int) bool {
	cm, err := botAPI.GetChatMember(tgbotapi.ChatConfigWithUser{ChatID: chatID, UserID: userID})
	return err == nil && (cm.IsAdministrator() || cm.IsCreator())
}

func canRegister(botAPI *tgbotapi.BotAPI, chatID int64, userID int) bool {
	if getAllowAdmin(chatID) {
		return isAdmin(botAPI, chatID, userID)
	}
	return isOwner(botAPI, chatID, userID)
}

func mentionByID(name string, userID int64) string {
	return fmt.Sprintf(`<a href="tg://user?id=%d">%s</a>`, userID, name)
}

func mentionByUsername(name, username string) string {
	return fmt.Sprintf(`<a href="https://t.me/%s">%s</a>`, username, name)
}

func buildMention(m Member) string {
	if m.UserID.Valid {
		return mentionByID(m.Name, m.UserID.Int64)
	}
	if m.Username != "" {
		return mentionByUsername(m.Name, m.Username)
	}
	return m.Name
}

func send(botAPI *tgbotapi.BotAPI, chatID int64, text string, replyTo int) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	if replyTo != 0 {
		msg.ReplyToMessageID = replyTo
	}
	botAPI.Send(msg)
}

// parseFromText: مستقیم توی متن دنبال @ یا آیدی عددی میگرده — مشکل RTL/LTR نداره
func parseFromText(afterCmd string) (name, identifier string) {
	if idx := strings.Index(afterCmd, "@"); idx >= 0 {
		end := idx + 1
		for end < len(afterCmd) && afterCmd[end] != ' ' && afterCmd[end] != '\t' {
			end++
		}
		username := afterCmd[idx+1 : end]
		identifier = "@" + username
		before := strings.TrimSpace(afterCmd[:idx])
		after := strings.TrimSpace(afterCmd[end:])
		if before != "" {
			name = before
		} else {
			name = after
		}
		return
	}
	parts := strings.Fields(afterCmd)
	var nameParts []string
	for _, p := range parts {
		if id, err := strconv.ParseInt(p, 10, 64); err == nil && id > 0 && identifier == "" {
			identifier = p
		} else {
			nameParts = append(nameParts, p)
		}
	}
	name = strings.Join(nameParts, " ")
	return
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func handleRegister(botAPI *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	msgID := msg.MessageID

	if !canRegister(botAPI, chatID, msg.From.ID) {
		return
	}

	afterCmd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(msg.Text), "ثبت"))

	// ── روش ۱: reply روی پیام (بهترین)
	if msg.ReplyToMessage != nil {
		name := afterCmd
		if name == "" {
			send(botAPI, chatID, "❌ اسم رو بنویس!\nمثال: <code>ثبت فرهاد</code>", msgID)
			return
		}
		target := msg.ReplyToMessage.From
		if target.IsBot {
			send(botAPI, chatID, "❌ نمیشه ربات ثبت کرد!", msgID)
			return
		}
		uid := sql.NullInt64{Int64: int64(target.ID), Valid: true}
		if err := upsertMember(chatID, int64(msg.From.ID), name, uid, target.UserName); err != nil {
			log.Println("upsertMember reply:", err)
			send(botAPI, chatID, "❌ خطا در ثبت.", msgID)
			return
		}
		send(botAPI, chatID,
			fmt.Sprintf("✅ %s با اسم <b>%s</b> ثبت شد! 🔒", mentionByID(name, int64(target.ID)), name), msgID)
		return
	}

	// ── روش ۲ و ۳: بدون reply
	name, identifier := parseFromText(afterCmd)

	if afterCmd == "" || (name == "" && identifier == "") {
		send(botAPI, chatID,
			"❌ سه روش ثبت:\n\n"+
				"۱. <b>reply</b> روی پیام + <code>ثبت فرهاد</code> ✅ بهترین\n"+
				"۲. <code>ثبت فرهاد 123456789</code> آیدی عددی\n"+
				"۳. <code>ثبت فرهاد @farhad</code> یوزرنیم", msgID)
		return
	}

	if identifier == "" {
		send(botAPI, chatID,
			fmt.Sprintf("❌ «%s» @username نداره?\n\nروی پیامش <b>reply</b> کن و بنویس:\n<code>ثبت %s</code>", name, name), msgID)
		return
	}
	if name == "" {
		send(botAPI, chatID, "❌ اسم رو هم بنویس.", msgID)
		return
	}

	if strings.Contains(identifier, "@") {
		username := strings.Trim(identifier, "@")
		nullID := sql.NullInt64{Valid: false}
		if err := upsertMember(chatID, int64(msg.From.ID), name, nullID, username); err != nil {
			log.Println("upsertMember @username:", err)
			send(botAPI, chatID, "❌ خطا در ثبت.", msgID)
			return
		}
		send(botAPI, chatID, fmt.Sprintf("✅ <b>%s</b> (@%s) ثبت شد!", name, username), msgID)
		return
	}

	// آیدی عددی مستقیم
	userID, err := strconv.ParseInt(strings.Trim(identifier, "@"), 10, 64)
	if err != nil || userID <= 0 {
		send(botAPI, chatID, "❌ آیدی معتبر نیست!\nمثال: <code>ثبت فرهاد 123456789</code>", msgID)
		return
	}
	uid := sql.NullInt64{Int64: userID, Valid: true}
	if err := upsertMember(chatID, int64(msg.From.ID), name, uid, ""); err != nil {
		log.Println("upsertMember numericID:", err)
		send(botAPI, chatID, "❌ خطا در ثبت.", msgID)
		return
	}
	send(botAPI, chatID,
		fmt.Sprintf("✅ %s با اسم <b>%s</b> ثبت شد! 🔒", mentionByID(name, userID), name), msgID)
}

func handleAlias(botAPI *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	msgID := msg.MessageID

	if !canRegister(botAPI, chatID, msg.From.ID) {
		return
	}

	afterCmd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(msg.Text), "لقب"))
	parts := strings.Fields(afterCmd)
	if len(parts) != 2 {
		send(botAPI, chatID,
			"❌ فرمت: <code>لقب فری فرهاد</code>\n(لقب جدید + اسم اصلی که قبلاً ثبت شده)", msgID)
		return
	}
	aliasName := parts[0]
	mainName := parts[1]

	var mainUserID sql.NullInt64
	var mainUsername string
	err := db.QueryRow(
		`SELECT user_id, COALESCE(username,'') FROM members WHERE group_id=$1 AND name=$2`,
		chatID, mainName,
	).Scan(&mainUserID, &mainUsername)
	if err != nil {
		send(botAPI, chatID, fmt.Sprintf("❌ «%s» در لیست پیدا نشد.", mainName), msgID)
		return
	}
	if !mainUserID.Valid {
		send(botAPI, chatID,
			fmt.Sprintf("❌ «%s» هنوز آیدی عددی نداره. صبر کن توی گروه پیام بده.", mainName), msgID)
		return
	}

	if err := upsertMember(chatID, int64(msg.From.ID), aliasName, mainUserID, mainUsername); err != nil {
		log.Println("upsertMember alias:", err)
		send(botAPI, chatID, "❌ خطا در ثبت لقب.", msgID)
		return
	}
	send(botAPI, chatID,
		fmt.Sprintf("✅ «%s» به عنوان لقب %s ثبت شد!", aliasName, mentionByID(mainName, mainUserID.Int64)), msgID)
}

func handleRemove(botAPI *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	msgID := msg.MessageID

	if !canRegister(botAPI, chatID, msg.From.ID) {
		return
	}

	afterCmd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(msg.Text), "حذف"))

	if afterCmd == "کل" {
		if !isOwner(botAPI, chatID, msg.From.ID) {
			send(botAPI, chatID, "❌ فقط مالک گروه میتونه همه لیست رو پاک کنه.", msgID)
			return
		}
		if err := deleteAllMembers(chatID); err != nil {
			send(botAPI, chatID, "❌ خطا در حذف.", 0)
			return
		}
		send(botAPI, chatID, "✅ همه اسم‌ها از لیست حذف شدند.", 0)
		return
	}

	if afterCmd == "" {
		send(botAPI, chatID,
			"❌ اسم رو بنویس!\n<code>حذف فرهاد</code> ← یه اسم\n<code>حذف کل</code> ← همه (فقط مالک)", msgID)
		return
	}

	found, err := deleteMember(chatID, afterCmd)
	if err != nil {
		send(botAPI, chatID, "❌ خطا در حذف.", 0)
		return
	}
	if !found {
		send(botAPI, chatID, fmt.Sprintf("❌ «%s» در لیست پیدا نشد.", afterCmd), msgID)
		return
	}
	send(botAPI, chatID, fmt.Sprintf("✅ «%s» از لیست حذف شد.", afterCmd), 0)
}

func handleList(botAPI *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	members, err := getMembers(chatID)
	if err != nil || len(members) == 0 {
		send(botAPI, chatID, "📋 هنوز کسی در این گروه ثبت نشده.", 0)
		return
	}

	// گروه‌بندی بر اساس user_id: اسم اول = اصلی، بقیه = لقب
	type PersonInfo struct {
		Names   []string
		MainRef Member
	}
	personMap := make(map[string]*PersonInfo) // key: user_id یا username
	var order []string

	for _, m := range members {
		var key string
		if m.UserID.Valid {
			key = fmt.Sprintf("id:%d", m.UserID.Int64)
		} else {
			key = fmt.Sprintf("un:%s", m.Username)
		}
		if _, exists := personMap[key]; !exists {
			personMap[key] = &PersonInfo{MainRef: m}
			order = append(order, key)
		}
		personMap[key].Names = append(personMap[key].Names, m.Name)
	}

	lines := []string{"📋 <b>لیست اعضای ثبت‌شده:</b>\n"}
	for i, key := range order {
		p := personMap[key]
		m := p.MainRef
		m.Name = p.Names[0]
		tag := buildMention(m)
		if !m.UserID.Valid {
			tag += " ⏳"
		}
		line := fmt.Sprintf("%d. %s", i+1, tag)
		if len(p.Names) > 1 {
			line += fmt.Sprintf("\n    └ لقب‌ها: %s", strings.Join(p.Names[1:], "، "))
		}
		lines = append(lines, line)
	}
	send(botAPI, chatID, strings.Join(lines, "\n"), 0)
}

func handleToggleAdmin(botAPI *tgbotapi.BotAPI, msg *tgbotapi.Message, enable bool) {
	chatID := msg.Chat.ID
	if !isOwner(botAPI, chatID, msg.From.ID) {
		send(botAPI, chatID, "❌ فقط مالک گروه میتونه این تنظیم رو عوض کنه.", msg.MessageID)
		return
	}
	if err := setAllowAdmin(chatID, enable); err != nil {
		send(botAPI, chatID, "❌ خطا در ذخیره تنظیمات.", 0)
		return
	}
	if enable {
		send(botAPI, chatID, "✅ ادمین‌ها هم میتونن ثبت و حذف کنن.", 0)
	} else {
		send(botAPI, chatID, "✅ فقط مالک گروه میتونه ثبت و حذف کنه.", 0)
	}
}

func handleMessage(botAPI *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	text := msg.Text
	if text == "" {
		return
	}

	members, err := getMembers(chatID)
	if err != nil || len(members) == 0 {
		return
	}

	senderID := int64(msg.From.ID)
	tagged := map[int64]bool{senderID: true}
	taggedUN := map[string]bool{}
	var mentions []string

	for _, m := range members {
		if !strings.Contains(text, m.Name) {
			continue
		}
		if m.UserID.Valid {
			if !tagged[m.UserID.Int64] {
				tagged[m.UserID.Int64] = true
				mentions = append(mentions, mentionByID(m.Name, m.UserID.Int64))
			}
		} else if m.Username != "" {
			if !taggedUN[m.Username] {
				taggedUN[m.Username] = true
				mentions = append(mentions, mentionByUsername(m.Name, m.Username))
			}
		}
	}

	if len(mentions) == 0 {
		return
	}

	body := strings.Join(mentions, "\n") + "\n\nپشت سرت دارن غیبت میکنن 😉"
	send(botAPI, chatID, body, msg.MessageID)
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	_ = godotenv.Load()

	if err := initDB(); err != nil {
		log.Fatal("DB init failed:", err)
	}
	defer db.Close()
	log.Println("✅ Database connected")

	botAPI, err := tgbotapi.NewBotAPI(os.Getenv("TELEGRAM_BOT_TOKEN"))
	if err != nil {
		log.Panic(err)
	}
	log.Printf("✅ Bot running as @%s", botAPI.Self.UserName)

	go func() {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "anti-gossip-bot is alive ✅")
		})
		log.Printf("Health server on :%s", port)
		http.ListenAndServe(":"+port, nil)
	}()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates, err := botAPI.GetUpdatesChan(u)
	if err != nil {
		log.Panic(err)
	}

	for update := range updates {
		if update.Message == nil || update.Message.From == nil {
			continue
		}
		msg := update.Message
		if msg.Chat.Type != "group" && msg.Chat.Type != "supergroup" {
			continue
		}
		if msg.From.IsBot {
			continue
		}

		if msg.From.UserName != "" {
			go autoResolve(msg.Chat.ID, msg.From.UserName, int64(msg.From.ID))
		}

		text := strings.TrimSpace(msg.Text)

		switch {
		case text == "ثبت" || strings.HasPrefix(text, "ثبت "):
			handleRegister(botAPI, msg)
		case text == "لقب" || strings.HasPrefix(text, "لقب "):
			handleAlias(botAPI, msg)
		case text == "حذف" || strings.HasPrefix(text, "حذف "):
			handleRemove(botAPI, msg)
		case text == "لیست":
			handleList(botAPI, msg)
		case text == "ادمین فعال":
			handleToggleAdmin(botAPI, msg, true)
		case text == "ادمین غیرفعال":
			handleToggleAdmin(botAPI, msg, false)
		default:
			handleMessage(botAPI, msg)
		}
	}
}