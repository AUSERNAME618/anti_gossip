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