// config/config.go — Tipovačka 2.0
// Centrální konfigurace aplikace. Čte z env proměnných, fallback na defaults.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

var (
	AppName    = "Tipovačka 3.0"
	AppVersion = "3.0.0"

	SecretKey     string
	SessionMaxAge int

	DatabaseURL    string
	NeonBackupURL  string

	AdminUsername string
	AdminPassword string

	MaxUploadBytes          int
	AllowedImageExtensions  = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true}

	PointsExact  = 3
	PointsWinner = 1
	PointsMiss   = 0

	// SMTP
	SMTPHost     string
	SMTPPort     int
	SMTPUser     string
	SMTPPassword string
	SMTPFrom     string
	SMTPEnabled  bool

	AppURL string

	NotifyHoursBefore int

	GithubPAT  string
	GithubRepo string

	FootballAPIKey string
)

func init() {
	// Ignore error — .env nemusí existovat (Koyeb)
	_ = godotenv.Load()

	SecretKey = getEnv("SECRET_KEY", randomHex(32))
	SessionMaxAge = getEnvInt("SESSION_MAX_AGE", 60*60*24*7)

	DatabaseURL = getEnv("DATABASE_URL", "")
	NeonBackupURL = getEnv("NEON_BACKUP_URL", "")

	AdminUsername = getEnv("ADMIN_USERNAME", "admin")
	AdminPassword = getEnv("ADMIN_PASSWORD", "changeme")

	MaxUploadBytes = getEnvInt("MAX_UPLOAD_BYTES", 5*1024*1024)

	SMTPHost = getEnvFirst("MAIL_SERVER", "SMTP_HOST", "")
	SMTPPort = getEnvInt("MAIL_PORT", 587)
	SMTPUser = getEnvFirst("MAIL_USERNAME", "SMTP_USER", "")
	SMTPPassword = getEnvFirst("MAIL_PASSWORD", "SMTP_PASSWORD", "")
	SMTPFrom = getEnvFirst("MAIL_DEFAULT_SENDER", "SMTP_FROM", "")
	SMTPEnabled = SMTPHost != "" && SMTPUser != ""

	AppURL = strings.TrimRight(getEnv("APP_URL", ""), "/")

	NotifyHoursBefore = getEnvInt("NOTIFY_HOURS_BEFORE", 2)

	GithubPAT = getEnv("GITHUB_PAT", "")
	GithubRepo = getEnv("GITHUB_REPO", "Mejlacz/Tipovacka-2.0")

	FootballAPIKey = getEnv("FOOTBALL_API_KEY", "")
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvFirst(key1, key2, fallback string) string {
	if v := os.Getenv(key1); v != "" {
		return v
	}
	if v := os.Getenv(key2); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
