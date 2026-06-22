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

	// بدون UNIQUE روی (group_id, name) — دو نفر با یه اسم مجازه
	if _, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS members (
			id         SERIAL PRIMARY KEY,
			group_id   BIGINT       NOT NULL,
			name       VARCHAR(100) NOT NULL,
			user_id    BIGINT,
			username   VARCHAR(100),
			added_by   BIGINT,
			created_at TIMESTAMP DEFAULT NOW()
		)
	`); err != nil {
		return fmt.Errorf("create members: %w", err)
	}

	if _, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS group_settings (
			group_id             BIGINT PRIMARY KEY,
			allow_admin_register BOOLEAN NOT NULL DEFAULT FALSE,
			updated_at           TIMESTAMP DEFAULT NOW()
		)
	`); err != nil {
		return fmt.Errorf("create settings: %w", err)
	}

	// حذف constraint قدیمی اگه وجود داشت
	db.Exec(`ALTER TABLE members DROP CONSTRAINT IF EXISTS members_group_id_name_key`)

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
			log.Println("scan error:", err)
			continue
		}
		list = append(list, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

// insertMember: ثبت عضو — اگه همین (group_id, name, user_id) قبلاً بود فقط username آپدیت میشه
func insertMember(groupID, addedBy int64, name string, userID sql.NullInt64, username string) error {
	if userID.Valid {
		// چک کن همین ترکیب وجود داره؟
		var count int
		db.QueryRow(
			`SELECT COUNT(*) FROM members WHERE group_id=$1 AND name=$2 AND user_id=$3`,
			groupID, name, userID.Int64,
		).Scan(&count)
		if count > 0 {
			// فقط username آپدیت
			_, err := db.Exec(
				`UPDATE members SET username=$1 WHERE group_id=$2 AND name=$3 AND user_id=$4`,
				nullStr(username), groupID, name, userID.Int64,
			)
			return err
		}
	}
	// ثبت جدید
	_, err := db.Exec(
		`INSERT INTO members (group_id, name, user_id, username, added_by)
		 VALUES ($1,$2,$3,$4,$5)`,
		groupID, name, userID, nullStr(username), addedBy,
	)
	return err
}

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
		INSERT INTO group_settings (group_id, allow_admin_register) VALUES ($1,$2)
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

// isWordPresent: اسم باید به عنوان کلمه مستقل باشه — جلوگیری از مچ "اسی" در "اسیاب"
func isWordPresent(text, name string) bool {
	words := strings.Fields(text)
	for _, w := range words {
		w = strings.Trim(w, "!?.،؟؛:\"'()-_…")
		if w == name {
			return true
		}
		for _, suffix := range []string{"ی", "و", "رو", "را", "ام", "ات", "اش"} {
			if w == name+suffix {
				return true
			}
		}
	}
	return false
}

// parseFromText: مستقیم توی متن دنبال @ یا آیدی عددی میگرده
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

	// ── روش ۱: reply روی پیام
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
		if err := insertMember(chatID, int64(msg.From.ID), name, uid, target.UserName); err != nil {
			log.Println("insertMember reply:", err)
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
		if err := insertMember(chatID, int64(msg.From.ID), name, nullID, username); err != nil {
			log.Println("insertMember @username:", err)
			send(botAPI, chatID, "❌ خطا در ثبت.", msgID)
			return
		}
		send(botAPI, chatID, fmt.Sprintf("✅ <b>%s</b> (@%s) ثبت شد!", name, username), msgID)
		return
	}

	userID, err := strconv.ParseInt(strings.Trim(identifier, "@"), 10, 64)
	if err != nil || userID <= 0 {
		send(botAPI, chatID, "❌ آیدی معتبر نیست!\nمثال: <code>ثبت فرهاد 123456789</code>", msgID)
		return
	}
	uid := sql.NullInt64{Int64: userID, Valid: true}
	if err := insertMember(chatID, int64(msg.From.ID), name, uid, ""); err != nil {
		log.Println("insertMember numericID:", err)
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
		`SELECT user_id, COALESCE(username,'') FROM members WHERE group_id=$1 AND name=$2 LIMIT 1`,
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
	if err := insertMember(chatID, int64(msg.From.ID), aliasName, mainUserID, mainUsername); err != nil {
		log.Println("insertMember alias:", err)
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
	if err != nil {
		log.Println("getMembers error:", err)
		send(botAPI, chatID, "❌ خطا در دریافت لیست.", 0)
		return
	}
	if len(members) == 0 {
		send(botAPI, chatID, "📋 هنوز کسی در این گروه ثبت نشده.", 0)
		return
	}

	// گروه‌بندی بر اساس user_id یا username
	type PersonInfo struct {
		Names   []string
		MainRef Member
	}
	personMap := make(map[string]*PersonInfo)
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
	taggedIDs := map[int64]bool{senderID: true}
	taggedUN := map[string]bool{}

	// گروه‌بندی بر اساس اسم — یه اسم ممکنه چند نفر داشته باشه
	type nameGroup struct{ members []Member }
	nameGroups := make(map[string]*nameGroup)

	for _, m := range members {
		if m.UserID.Valid && m.UserID.Int64 == senderID {
			continue
		}
		if !isWordPresent(text, m.Name) {
			continue
		}
		if _, ok := nameGroups[m.Name]; !ok {
			nameGroups[m.Name] = &nameGroup{}
		}
		nameGroups[m.Name].members = append(nameGroups[m.Name].members, m)
	}

	if len(nameGroups) == 0 {
		return
	}

	var normalMentions []string

	for name, group := range nameGroups {
		// حذف تکراری‌ها در این گروه اسمی
		seenIDs := map[int64]bool{}
		seenUNs := map[string]bool{}
		var unique []Member
		for _, m := range group.members {
			if m.UserID.Valid {
				if !seenIDs[m.UserID.Int64] {
					seenIDs[m.UserID.Int64] = true
					unique = append(unique, m)
				}
			} else if m.Username != "" {
				if !seenUNs[m.Username] {
					seenUNs[m.Username] = true
					unique = append(unique, m)
				}
			}
		}

		if len(unique) == 1 {
			m := unique[0]
			if m.UserID.Valid && !taggedIDs[m.UserID.Int64] {
				taggedIDs[m.UserID.Int64] = true
				normalMentions = append(normalMentions, mentionByID(name, m.UserID.Int64))
			} else if !m.UserID.Valid && m.Username != "" && !taggedUN[m.Username] {
				taggedUN[m.Username] = true
				normalMentions = append(normalMentions, mentionByUsername(name, m.Username))
			}
		} else {
			// چند نفر با یه اسم — پیام مبهم
			var tags []string
			for _, m := range unique {
				if m.UserID.Valid && !taggedIDs[m.UserID.Int64] {
					taggedIDs[m.UserID.Int64] = true
					tags = append(tags, mentionByID(name, m.UserID.Int64))
				} else if !m.UserID.Valid && m.Username != "" && !taggedUN[m.Username] {
					taggedUN[m.Username] = true
					tags = append(tags, mentionByUsername(name, m.Username))
				}
			}
			if len(tags) > 0 {
				body := strings.Join(tags, "\n") +
					"\n\nپشت سر یکیتون دارن غیبت میکنن ولی نمیدونم کدومتون 😏"
				send(botAPI, chatID, body, msg.MessageID)
			}
		}
	}

	if len(normalMentions) == 0 {
		return
	}

	var body string
	if len(normalMentions) == 1 {
		// یه نفر
		body = normalMentions[0] + "\n\nپشت سرت دارن غیبت میکنن 😉"
	} else {
		// چند نفر مختلف
		body = strings.Join(normalMentions, "\n") + "\n\nپشت سرتون دارن غیبت میکنن 😏"
	}
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