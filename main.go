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
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS members (
			id         SERIAL PRIMARY KEY,
			group_id   BIGINT       NOT NULL,
			name       VARCHAR(100) NOT NULL,
			user_id    BIGINT,
			username   VARCHAR(100),
			added_by   BIGINT,
			created_at TIMESTAMP DEFAULT NOW()
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_members_resolved
			ON members(group_id, name, user_id) WHERE user_id IS NOT NULL;
		CREATE UNIQUE INDEX IF NOT EXISTS idx_members_unresolved
			ON members(group_id, name, username) WHERE user_id IS NULL;
		CREATE TABLE IF NOT EXISTS group_settings (
			group_id             BIGINT PRIMARY KEY,
			allow_admin_register BOOLEAN NOT NULL DEFAULT FALSE,
			updated_at           TIMESTAMP DEFAULT NOW()
		);
	`)
	return err
}

func getMembers(groupID int64) ([]Member, error) {
	rows, err := db.Query(
		`SELECT name, user_id, COALESCE(username,'')
		 FROM members WHERE group_id=$1
		 ORDER BY COALESCE(user_id,0), id`,
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

func addMemberWithID(groupID, userID, addedBy int64, name, username string) error {
	db.Exec(`DELETE FROM members WHERE group_id=$1 AND name=$2 AND user_id IS NULL`, groupID, name)
	_, err := db.Exec(`
		INSERT INTO members (group_id, name, user_id, username, added_by)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (group_id, name, user_id) WHERE user_id IS NOT NULL
		DO UPDATE SET username=EXCLUDED.username
	`, groupID, name, userID, nullStr(username), addedBy)
	return err
}

func addMemberUsernameOnly(groupID, addedBy int64, name, username string) error {
	_, err := db.Exec(`
		INSERT INTO members (group_id, name, user_id, username, added_by)
		VALUES ($1,$2,NULL,$3,$4)
		ON CONFLICT (group_id, name, username) WHERE user_id IS NULL
		DO UPDATE SET username=EXCLUDED.username
	`, groupID, name, username, addedBy)
	return err
}

func autoResolve(groupID int64, username string, realUserID int64) {
	if username == "" {
		return
	}
	db.Exec(`
		DELETE FROM members
		WHERE group_id=$1 AND username=$2 AND user_id IS NULL
		AND EXISTS (
			SELECT 1 FROM members m2
			WHERE m2.group_id=$1 AND m2.name=members.name AND m2.user_id=$3
		)
	`, groupID, username, realUserID)
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

func getUserIDsByName(groupID int64, name string) ([]sql.NullInt64, error) {
	rows, err := db.Query(
		`SELECT DISTINCT user_id FROM members WHERE group_id=$1 AND name=$2`, groupID, name,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []sql.NullInt64
	for rows.Next() {
		var id sql.NullInt64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids, nil
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
	err := db.QueryRow(
		`SELECT allow_admin_register FROM group_settings WHERE group_id=$1`, groupID,
	).Scan(&allow)
	return err == nil && allow
}

func setAllowAdmin(groupID int64, allow bool) error {
	_, err := db.Exec(`
		INSERT INTO group_settings (group_id, allow_admin_register)
		VALUES ($1,$2)
		ON CONFLICT (group_id)
		DO UPDATE SET allow_admin_register=EXCLUDED.allow_admin_register, updated_at=NOW()
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
	return mentionByUsername(m.Name, m.Username)
}

func send(botAPI *tgbotapi.BotAPI, chatID int64, text string, replyTo int) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	if replyTo != 0 {
		msg.ReplyToMessageID = replyTo
	}
	botAPI.Send(msg)
}

// parseIdentifier: بدون در نظر گرفتن ترتیب RTL/LTR، شناسه و اسم رو جدا میکنه
func parseNameAndID(parts []string) (name, identifier string) {
	var nameParts []string
	for _, p := range parts {
		hasAt := strings.Contains(p, "@")
		_, numErr := strconv.ParseInt(strings.Trim(p, "@"), 10, 64)
		if (hasAt || numErr == nil) && identifier == "" {
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

	// ── روش ۱: reply (بهترین روش)
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
		if err := addMemberWithID(chatID, int64(target.ID), int64(msg.From.ID), name, target.UserName); err != nil {
			log.Println("addMemberWithID:", err)
			send(botAPI, chatID, "❌ خطا در ثبت.", msgID)
			return
		}
		send(botAPI, chatID,
			fmt.Sprintf("✅ %s با اسم <b>%s</b> ثبت شد! 🔒",
				mentionByID(name, int64(target.ID)), name), msgID)
		return
	}

	// ── روش ۲ و ۳: بدون reply
	parts := strings.Fields(afterCmd)
	if len(parts) < 2 {
		send(botAPI, chatID,
			"❌ سه روش ثبت:\n\n"+
				"۱. <b>reply</b> روی پیام + <code>ثبت فرهاد</code> ✅ بهترین\n"+
				"۲. <code>ثبت فرهاد 123456789</code> آیدی عددی\n"+
				"۳. <code>ثبت فرهاد @farhad</code> یوزرنیم", msgID)
		return
	}

	// جدا کردن اسم و شناسه بدون توجه به ترتیب RTL/LTR
	name, identifier := parseNameAndID(parts)

	if name == "" || identifier == "" {
		send(botAPI, chatID, "❌ اسم و شناسه رو هر دو بنویس.", msgID)
		return
	}

	if strings.Contains(identifier, "@") {
		// روش ۳: @username
		username := strings.Trim(identifier, "@")
		if err := addMemberUsernameOnly(chatID, int64(msg.From.ID), name, username); err != nil {
			log.Println("addMemberUsernameOnly:", err)
			send(botAPI, chatID, "❌ خطا در ثبت.", msgID)
			return
		}
		send(botAPI, chatID,
			fmt.Sprintf("✅ <b>%s</b> (@%s) ثبت شد!\n⏳ آیدی عددی دفعه‌ای که پیام بده خودکار ذخیره میشه.",
				name, username), msgID)
		return
	}

	// روش ۲: آیدی عددی
	userID, err := strconv.ParseInt(strings.Trim(identifier, "@"), 10, 64)
	if err != nil || userID <= 0 {
		send(botAPI, chatID, "❌ شناسه معتبر نیست!\nمثال: <code>ثبت فرهاد 123456789</code>", msgID)
		return
	}
	if err := addMemberWithID(chatID, userID, int64(msg.From.ID), name, ""); err != nil {
		log.Println("addMemberWithID:", err)
		send(botAPI, chatID, "❌ خطا در ثبت.", msgID)
		return
	}
	send(botAPI, chatID,
		fmt.Sprintf("✅ %s با اسم <b>%s</b> ثبت شد! 🔒",
			mentionByID(name, userID), name), msgID)
}

// handleAlias: لقب‌جدید را به همان user_id اسم‌اصلی متصل میکند
func handleAlias(botAPI *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	msgID := msg.MessageID

	if !canRegister(botAPI, chatID, msg.From.ID) {
		return
	}

	// فرمت: لقب فری فرهاد
	afterCmd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(msg.Text), "لقب"))
	parts := strings.Fields(afterCmd)
	if len(parts) != 2 {
		send(botAPI, chatID,
			"❌ فرمت: <code>لقب فری فرهاد</code>\n(لقب جدید + اسم اصلی که قبلاً ثبت شده)", msgID)
		return
	}
	aliasName := parts[0]
	mainName := parts[1]

	ids, err := getUserIDsByName(chatID, mainName)
	if err != nil || len(ids) == 0 {
		send(botAPI, chatID, fmt.Sprintf("❌ «%s» در لیست پیدا نشد. اول ثبتش کن.", mainName), msgID)
		return
	}

	var resolvedIDs []int64
	for _, id := range ids {
		if id.Valid {
			resolvedIDs = append(resolvedIDs, id.Int64)
		}
	}
	if len(resolvedIDs) == 0 {
		send(botAPI, chatID,
			fmt.Sprintf("❌ «%s» هنوز آیدی عددی نداره.\nصبر کن یه پیام توی گروه بده.", mainName), msgID)
		return
	}
	if len(resolvedIDs) > 1 {
		send(botAPI, chatID, fmt.Sprintf("❌ چند نفر با اسم «%s» هستن.", mainName), msgID)
		return
	}

	var username string
	db.QueryRow(
		`SELECT COALESCE(username,'') FROM members WHERE group_id=$1 AND user_id=$2 LIMIT 1`,
		chatID, resolvedIDs[0],
	).Scan(&username)

	if err := addMemberWithID(chatID, resolvedIDs[0], int64(msg.From.ID), aliasName, username); err != nil {
		log.Println("addMember alias:", err)
		send(botAPI, chatID, "❌ خطا در ثبت لقب.", msgID)
		return
	}
	send(botAPI, chatID,
		fmt.Sprintf("✅ «%s» به عنوان لقب %s ثبت شد!", aliasName, mentionByID(mainName, resolvedIDs[0])), msgID)
}

func handleRemove(botAPI *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	msgID := msg.MessageID

	if !canRegister(botAPI, chatID, msg.From.ID) {
		return
	}

	afterCmd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(msg.Text), "حذف"))

	// حذف کل — فقط مالک
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
			"❌ اسم رو بنویس!\n<code>حذف فرهاد</code> ← یه اسم\n<code>حذف کل</code> ← همه اسم‌ها (فقط مالک)", msgID)
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

	type Key struct {
		UserID   int64
		Username string
	}
	type PersonInfo struct {
		Names   []string
		MainRef Member
	}
	personMap := make(map[Key]*PersonInfo)
	var order []Key

	for _, m := range members {
		var k Key
		if m.UserID.Valid {
			k = Key{UserID: m.UserID.Int64}
		} else {
			k = Key{Username: m.Username}
		}
		if _, exists := personMap[k]; !exists {
			personMap[k] = &PersonInfo{MainRef: m}
			order = append(order, k)
		}
		personMap[k].Names = append(personMap[k].Names, m.Name)
	}

	lines := []string{"📋 <b>لیست اعضای ثبت‌شده:</b>\n"}
	for i, k := range order {
		p := personMap[k]
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

	type MatchGroup struct{ Members []Member }
	nameMatches := make(map[string]*MatchGroup)
	for _, m := range members {
		if !strings.Contains(text, m.Name) {
			continue
		}
		if _, ok := nameMatches[m.Name]; !ok {
			nameMatches[m.Name] = &MatchGroup{}
		}
		nameMatches[m.Name].Members = append(nameMatches[m.Name].Members, m)
	}
	if len(nameMatches) == 0 {
		return
	}

	senderID := int64(msg.From.ID)
	taggedIDs := map[int64]bool{senderID: true}
	taggedUsernames := map[string]bool{}
	var normalMentions []string

	for name, group := range nameMatches {
		var filtered []Member
		for _, m := range group.Members {
			if m.UserID.Valid && m.UserID.Int64 == senderID {
				continue
			}
			filtered = append(filtered, m)
		}
		if len(filtered) == 0 {
			continue
		}

		if len(filtered) == 1 {
			m := filtered[0]
			if m.UserID.Valid {
				if !taggedIDs[m.UserID.Int64] {
					taggedIDs[m.UserID.Int64] = true
					normalMentions = append(normalMentions, mentionByID(name, m.UserID.Int64))
				}
			} else {
				if !taggedUsernames[m.Username] {
					taggedUsernames[m.Username] = true
					normalMentions = append(normalMentions, mentionByUsername(name, m.Username))
				}
			}
		} else {
			var tags []string
			for _, m := range filtered {
				if m.UserID.Valid {
					if !taggedIDs[m.UserID.Int64] {
						taggedIDs[m.UserID.Int64] = true
						tags = append(tags, mentionByID(name, m.UserID.Int64))
					}
				} else {
					if !taggedUsernames[m.Username] {
						taggedUsernames[m.Username] = true
						tags = append(tags, mentionByUsername(name, m.Username))
					}
				}
			}
			if len(tags) > 0 {
				body := strings.Join(tags, "\n") +
					"\n\nپشت سر یکی از شماها دارن غیبت میکنن ولی نمیدونم کدومتون 😏"
				send(botAPI, chatID, body, msg.MessageID)
			}
		}
	}

	if len(normalMentions) > 0 {
		body := strings.Join(normalMentions, "\n") + "\n\nپشت سرت دارن غیبت میکنن 😉"
		send(botAPI, chatID, body, msg.MessageID)
	}
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