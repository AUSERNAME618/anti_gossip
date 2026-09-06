package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

var db *sql.DB
var groqToken string

type Member struct {
	Name     string
	UserID   sql.NullInt64
	Username string
	LastSeen sql.NullTime
}

// ── Persian text normalization ──────────────────────────────────────────────
// حروف عربی/فارسی که ظاهرشون یکیه ولی کد یونیکدشون فرق داره رو یکسان میکنه
// مثلاً "ي" عربی vs "ی" فارسی، یا "ك" عربی vs "ک" فارسی
var persianNormalizer = strings.NewReplacer(
	"ي", "ی",
	"ك", "ک",
	"ة", "ه",
	"ۀ", "ه",
	"أ", "ا",
	"إ", "ا",
	"ؤ", "و",
	"ئ", "ی",
	"\u200c", "", // نیم‌فاصله (ZWNJ) — حذف میشه تا کلمات متصل یکسان مقایسه بشن
	"\u00a0", " ", // space غیرشکن -> space معمولی
)

func normalizePersian(s string) string {
	return persianNormalizer.Replace(s)
}

// ── Database ──────────────────────────────────────────────────────────────────

func initDB() error {
	var err error
	db, err = sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	// فقط یک کانکشن همزمان — از تصادم درخواست‌ها روی Neon جلوگیری میکنه
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)
	if err = db.Ping(); err != nil {
		return fmt.Errorf("ping: %w", err)
	}

	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS members (
		id         SERIAL PRIMARY KEY,
		group_id   BIGINT       NOT NULL,
		name       VARCHAR(100) NOT NULL,
		user_id    BIGINT,
		username   VARCHAR(100),
		last_seen  TIMESTAMP,
		added_by   BIGINT,
		created_at TIMESTAMP DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("create members: %w", err)
	}
	db.Exec(`ALTER TABLE members ADD COLUMN IF NOT EXISTS last_seen TIMESTAMP`)
	db.Exec(`ALTER TABLE members DROP CONSTRAINT IF EXISTS members_group_id_name_key`)

	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS group_settings (
		group_id             BIGINT PRIMARY KEY,
		allow_admin_register BOOLEAN NOT NULL DEFAULT FALSE,
		updated_at           TIMESTAMP DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("create settings: %w", err)
	}

	return nil
}

func getMembers(groupID int64) ([]Member, error) {
	rows, err := db.Query(
		`SELECT name, user_id, COALESCE(username,''), last_seen
		 FROM members WHERE group_id=$1 ORDER BY id`,
		groupID,
	)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var list []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.Name, &m.UserID, &m.Username, &m.LastSeen); err != nil {
			log.Println("scan error:", err)
			continue
		}
		list = append(list, m)
	}
	return list, rows.Err()
}

func insertMember(groupID, addedBy int64, name string, userID sql.NullInt64, username string) error {
	name = normalizePersian(name)
	if userID.Valid {
		var count int
		db.QueryRow(
			`SELECT COUNT(*) FROM members WHERE group_id=$1 AND name=$2 AND user_id=$3`,
			groupID, name, userID.Int64,
		).Scan(&count)
		if count > 0 {
			_, err := db.Exec(
				`UPDATE members SET username=$1 WHERE group_id=$2 AND name=$3 AND user_id=$4`,
				nullStr(username), groupID, name, userID.Int64,
			)
			return err
		}
	}
	_, err := db.Exec(
		`INSERT INTO members (group_id, name, user_id, username, added_by)
		 VALUES ($1,$2,$3,$4,$5)`,
		groupID, name, userID, nullStr(username), addedBy,
	)
	return err
}

// updateActivity: last_seen رو آپدیت میکنه و username رو به آیدی عددی resolve میکنه
// عمداً به صورت synchronous (بدون go) صدا زده میشه تا با query های دیگه تصادم نکنه
func updateActivity(groupID int64, userID int64, username string) {
	if _, err := db.Exec(`UPDATE members SET last_seen=NOW() WHERE group_id=$1 AND user_id=$2`,
		groupID, userID); err != nil {
		log.Println("updateActivity last_seen error:", err)
	}

	if username != "" {
		result, err := db.Exec(`
			UPDATE members SET user_id=$1, last_seen=NOW()
			WHERE group_id=$2 AND username=$3 AND user_id IS NULL
		`, userID, groupID, username)
		if err != nil {
			log.Println("updateActivity resolve error:", err)
		} else if n, _ := result.RowsAffected(); n > 0 {
			log.Printf("✅ auto-resolved @%s → %d", username, userID)
		}
	}
}

func deleteMember(groupID int64, name string) (bool, error) {
	name = normalizePersian(name)
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

// cleanWord: کاراکترهای نامرئی و علائم نگارشی رو از یه کلمه حذف میکنه
func cleanWord(w string) string {
	return strings.TrimFunc(w, func(r rune) bool {
		switch {
		case r <= 0x20:
			return true
		case r >= 0x200B && r <= 0x200F:
			return true
		case r >= 0x202A && r <= 0x202E:
			return true
		case r == 0xFEFF:
			return true
		}
		return strings.ContainsRune("!?.,،؟؛:\"'()-_…«»", r)
	})
}

// tokenizeWords: متن رو به کلمات تمیزشده تبدیل میکنه
func tokenizeWords(text string) []string {
	raw := strings.Fields(text)
	out := make([]string, 0, len(raw))
	for _, w := range raw {
		w = cleanWord(w)
		if w != "" {
			out = append(out, w)
		}
	}
	return out
}

// hasSuffixMatch: چک میکنه کلمه با پسوند رایج فارسی به اسم چسبیده باشه (مثل علیو، رضارو)
func hasSuffixMatch(word, base string) bool {
	if word == base {
		return true
	}
	for _, s := range []string{"ی", "و", "رو", "را", "ام", "ات", "اش", "هم"} {
		if word == base+s {
			return true
		}
	}
	return false
}

// isNamePresent: اسم (یک یا چند کلمه‌ای) رو با رعایت مرز کلمه در متن پیدا میکنه
// "محمد رضا" فقط وقتی matches میشه که دقیقاً همون دو کلمه پشت‌سرهم بیان — نه فقط "محمد"
func isNamePresent(text, name string) bool {
	text = normalizePersian(text)
	name = normalizePersian(name)

	nameWords := tokenizeWords(name)
	if len(nameWords) == 0 {
		return false
	}
	textWords := tokenizeWords(text)

	for i := 0; i+len(nameWords) <= len(textWords); i++ {
		matched := true
		for j := 0; j < len(nameWords); j++ {
			tw := textWords[i+j]
			nw := nameWords[j]
			if j == len(nameWords)-1 {
				// فقط آخرین کلمه اجازه پسوند داره
				if !hasSuffixMatch(tw, nw) {
					matched = false
					break
				}
			} else if tw != nw {
				matched = false
				break
			}
		}
		if matched {
			return true
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
		identifier = "@" + afterCmd[idx+1:end]
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
		send(botAPI, chatID, "❌ فرمت: <code>لقب فری فرهاد</code>", msgID)
		return
	}
	aliasName := normalizePersian(parts[0])
	mainName := normalizePersian(parts[1])

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
			fmt.Sprintf("❌ «%s» هنوز آیدی عددی نداره.", mainName), msgID)
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
		log.Println("getMembers error in list:", err)
		send(botAPI, chatID, fmt.Sprintf("❌ خطا: %v", err), 0)
		return
	}
	if len(members) == 0 {
		send(botAPI, chatID, "📋 هنوز کسی در این گروه ثبت نشده.", 0)
		return
	}

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

const chunkMaxSec = 25   // هر تکه حداکثر ۲۵ ثانیه — کاملاً زیر بازه بهینه ۳۰ ثانیه‌ای Whisper
const chunkOverlapSec = 5 // همپوشانی بین تکه‌های پشت‌سرهم — تضمین میکنه کلمه‌ای روی خط برش گم نشه
const chunkStepSec = chunkMaxSec - chunkOverlapSec

// buildPromptFromNames: لیست اسم‌های ثبت‌شده گروه رو به یه prompt برای Whisper تبدیل میکنه
// این کار دقت تشخیص این اسم‌های خاص رو به شدت بالا میبره — حتی وقتی فقط یه بار گفته بشن
func buildPromptFromNames(groupID int64) string {
	members, err := getMembers(groupID)
	if err != nil || len(members) == 0 {
		return ""
	}
	seen := map[string]bool{}
	var names []string
	for _, m := range members {
		if !seen[m.Name] {
			seen[m.Name] = true
			names = append(names, m.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "اسم‌های افراد این مکالمه: " + strings.Join(names, "، ")
}

// padAudioEnd: به انتهای فایل صوتی کمی سکوت اضافه میکنه
// چون Whisper وقتی صدا بدون مکث قطع بشه، معمولاً آخرین کلمه رو گم میکنه یا اشتباه میفهمه
func padAudioEnd(inputPath string) (string, error) {
	outPath := inputPath + ".padded.ogg"
	cmd := exec.Command("ffmpeg", "-y", "-i", inputPath, "-af", "apad=pad_dur=1.5", outPath)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg pad: %w", err)
	}
	return outPath, nil
}

// downloadToTemp: فایل صوتی رو از تلگرام دانلود و توی یه فایل موقت ذخیره میکنه
func downloadToTemp(fileURL string) (string, error) {
	resp, err := http.Get(fileURL)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	tmpFile, err := os.CreateTemp("", "voice-*.ogg")
	if err != nil {
		return "", fmt.Errorf("temp file: %w", err)
	}
	defer tmpFile.Close()

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		return "", fmt.Errorf("write temp: %w", err)
	}
	return tmpFile.Name(), nil
}

// getAudioDuration: با ffprobe مدت زمان فایل صوتی رو به ثانیه برمیگردونه
func getAudioDuration(path string) (float64, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe: %w", err)
	}
	dur, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0, fmt.Errorf("parse duration: %w", err)
	}
	return dur, nil
}

// splitAudioChunks: فایل صوتی طولانی رو با همپوشانی به تکه‌های کوچیک‌تر تقسیم میکنه
// اگه فایل کوتاه‌تر از حد مجاز باشه، همون فایل اصلی رو برمیگردونه (بدون هزینه اضافه)
func splitAudioChunks(inputPath string, duration float64) ([]string, error) {
	if duration <= chunkMaxSec {
		return []string{inputPath}, nil
	}

	var chunks []string
	start := 0.0
	i := 0
	for start < duration {
		chunkDur := chunkMaxSec
		if start+float64(chunkMaxSec) > duration {
			chunkDur = int(duration - start)
		}
		if chunkDur <= 0 {
			break
		}
		outPath := fmt.Sprintf("%s.chunk%d.ogg", inputPath, i)
		cmd := exec.Command("ffmpeg", "-y", "-i", inputPath,
			"-ss", fmt.Sprintf("%.2f", start),
			"-t", fmt.Sprintf("%d", chunkDur),
			"-c", "copy", outPath)
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("ffmpeg chunk %d: %w", i, err)
		}
		chunks = append(chunks, outPath)
		start += chunkStepSec
		i++
	}
	return chunks, nil
}

// transcribeFile: یه فایل صوتی (یا تکه‌ای از اون) رو با Groq Whisper به متن تبدیل میکنه
// prompt: لیست اسم‌های محتمل که به مدل کمک میکنه دقیق‌تر تشخیص بده
func transcribeFile(path string, prompt string) (string, error) {
	audioBytes, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read audio: %w", err)
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	part, err := writer.CreateFormFile("file", "voice.ogg")
	if err != nil {
		return "", err
	}
	if _, err := part.Write(audioBytes); err != nil {
		return "", err
	}
	writer.WriteField("model", "whisper-large-v3")
	writer.WriteField("language", "fa") // راهنمایی به مدل که صدا فارسیه — دقت رو بالاتر میبره
	writer.WriteField("response_format", "json")
	writer.WriteField("temperature", "0") // خروجی قطعی‌تر و کمتر تصادفی
	if prompt != "" {
		writer.WriteField("prompt", prompt)
	}
	writer.Close()

	req, err := http.NewRequest("POST", "https://api.groq.com/openai/v1/audio/transcriptions", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+groqToken)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{Timeout: 30 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("groq request: %w", err)
	}
	defer res.Body.Close()

	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		return "", fmt.Errorf("groq error %d: %s", res.StatusCode, string(body))
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	return result.Text, nil
}

// transcribeWithChunking: در صورت طولانی بودن صدا، تکه‌تکه (با همپوشانی) تبدیل میکنه
// و در نهایت متن کامل رو برمیگردونه
func transcribeWithChunking(path string, prompt string) (string, error) {
	duration, err := getAudioDuration(path)
	if err != nil {
		log.Println("getAudioDuration failed, trying whole file:", err)
		return transcribeFile(path, prompt)
	}

	chunks, err := splitAudioChunks(path, duration)
	if err != nil {
		log.Println("splitAudioChunks failed, trying whole file:", err)
		return transcribeFile(path, prompt)
	}

	var texts []string
	for _, chunkPath := range chunks {
		if chunkPath != path {
			defer os.Remove(chunkPath)
		}
		t, err := transcribeFile(chunkPath, prompt)
		if err != nil {
			log.Println("transcribe chunk error:", err)
			continue
		}
		texts = append(texts, t)
	}
	return strings.Join(texts, " "), nil
}

// handleVoice: پیام صوتی رو به متن تبدیل و همون منطق تشخیص اسم رو روش اجرا میکنه
// عمداً synchronous (بدون go) — تا با DB تصادم نکنه
func handleVoice(botAPI *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	if msg.Voice == nil || groqToken == "" {
		return
	}

	file, err := botAPI.GetFile(tgbotapi.FileConfig{FileID: msg.Voice.FileID})
	if err != nil {
		log.Println("GetFile error:", err)
		return
	}
	fileURL := file.Link(botAPI.Token)

	localPath, err := downloadToTemp(fileURL)
	if err != nil {
		log.Println("downloadToTemp error:", err)
		return
	}
	defer os.Remove(localPath)

	// یه‌ذره سکوت به انتها اضافه میکنیم — تا آخرین کلمه (دقیقاً جایی که صدا قطع میشه) گم نشه
	processPath := localPath
	if padded, perr := padAudioEnd(localPath); perr == nil {
		processPath = padded
		defer os.Remove(padded)
	} else {
		log.Println("padAudioEnd failed, using unpadded file:", perr)
	}

	text, err := transcribeWithChunking(processPath, buildPromptFromNames(msg.Chat.ID))
	if err != nil {
		log.Println("transcribeWithChunking error:", err)
		return
	}
	if text == "" {
		return
	}

	checkAndTagNames(botAPI, msg, text)
}

// checkAndTagNames: اسم‌های ثبت‌شده رو توی متن (از پیام یا از ویس تبدیل‌شده) پیدا و تگ میکنه
func checkAndTagNames(botAPI *tgbotapi.BotAPI, msg *tgbotapi.Message, text string) {
	chatID := msg.Chat.ID
	if text == "" {
		return
	}

	members, err := getMembers(chatID)
	if err != nil {
		log.Println("getMembers error in handleMessage:", err)
		return
	}
	if len(members) == 0 {
		return
	}

	senderID := int64(msg.From.ID)
	taggedIDs := map[int64]bool{senderID: true}
	taggedUN := map[string]bool{}

	type nameGroup struct{ members []Member }
	nameGroups := make(map[string]*nameGroup)

	for _, m := range members {
		if m.UserID.Valid && m.UserID.Int64 == senderID {
			continue
		}
		if !isNamePresent(text, m.Name) {
			continue
		}
		// اگه ۵ دقیقه اخیر توی گروه پیام داده، تگ نکن
		if m.LastSeen.Valid && time.Since(m.LastSeen.Time) < 5*time.Minute {
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
		body = normalMentions[0] + "\n\nپشت سرت دارن غیبت میکنن 😉"
	} else {
		body = strings.Join(normalMentions, "\n") + "\n\nپشت سرتون دارن غیبت میکنن 😏"
	}
	send(botAPI, chatID, body, msg.MessageID)
}

// handleMessage: پیام متنی معمولی — همون منطق checkAndTagNames رو با msg.Text صدا میزنه
func handleMessage(botAPI *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	checkAndTagNames(botAPI, msg, msg.Text)
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

	groqToken = os.Getenv("GROQ_API_TOKEN")
	if groqToken == "" {
		log.Println("⚠️ GROQ_API_TOKEN not set — voice message detection disabled")
	} else {
		log.Println("✅ Voice transcription enabled (Groq Whisper)")
	}

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

		// synchronous — بدون "go" — تا با query های دیگه همزمان تصادم نکنه
		updateActivity(msg.Chat.ID, int64(msg.From.ID), msg.From.UserName)

		// پیام صوتی (ویس) — تبدیل به متن و بررسی اسم‌ها
		if msg.Voice != nil {
			handleVoice(botAPI, msg)
			continue
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
