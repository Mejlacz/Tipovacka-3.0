// scheduler/scheduler.go — Tipovačka 2.0
// Pozadí goroutiny: automatická Neon záloha + email notifikace o nadcházejících zápasech.
package scheduler

import (
	"context"
	"fmt"
	"log"
	"net/smtp"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"tipovacka/config"
	"tipovacka/handlers"
)

// Start spustí všechny pravidelné goroutiny.
// Voláno z main() po db.Init().
func Start(ctx context.Context, pool *pgxpool.Pool) {
	go runNeonBackupLoop(ctx, pool)
	go runEmailNotifyLoop(ctx, pool)
	log.Println("[scheduler] spuštěn")
}

// ─── Neon backup loop ─────────────────────────────────────────────────────────

func runNeonBackupLoop(ctx context.Context, pool *pgxpool.Pool) {
	// Začni za 5 minut, pak každou hodinu
	time.Sleep(5 * time.Minute)

	ticker := time.NewTicker(60 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkAndRunNeonBackup(ctx, pool)
		}
	}
}

func checkAndRunNeonBackup(ctx context.Context, pool *pgxpool.Pool) {
	var enabled bool
	var autoHour int
	err := pool.QueryRow(ctx,
		`SELECT enabled, auto_hour FROM neon_sync_config WHERE id=1`,
	).Scan(&enabled, &autoHour)
	if err != nil {
		// Tabulka možná neexistuje
		return
	}
	if !enabled {
		return
	}

	now := time.Now()
	if now.Hour() != autoHour {
		return
	}

	log.Printf("[scheduler] Spouštím automatickou Neon zálohu (hodina %d)", autoHour)
	total, err := handlers.RunNeonSync("auto")
	if err != nil {
		log.Printf("[scheduler] Neon záloha selhala: %v", err)
	} else {
		log.Printf("[scheduler] Neon záloha dokončena: %d řádků", total)
	}
}

// ─── Email notification loop ──────────────────────────────────────────────────

func runEmailNotifyLoop(ctx context.Context, pool *pgxpool.Pool) {
	// Začni za 2 minuty, pak každou hodinu
	time.Sleep(2 * time.Minute)

	ticker := time.NewTicker(60 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sendMatchNotifications(ctx, pool)
		}
	}
}

// notifyRecipient je příjemce notifikace.
type notifyRecipient struct {
	Email    string
	Username string
}

func sendMatchNotifications(ctx context.Context, pool *pgxpool.Pool) {
	if !config.SMTPEnabled {
		return
	}

	notifyBefore := time.Duration(config.NotifyHoursBefore) * time.Hour
	now := time.Now()
	windowStart := now
	windowEnd := now.Add(notifyBefore)

	// Najdi zápasy začínající v okně a ještě neoznámené
	rows, err := pool.Query(ctx, `
		SELECT m.id, m.match_date,
		       ht.name AS home, at.name AS away,
		       c.name AS comp
		FROM matches m
		JOIN rounds r ON r.id = m.round_id
		JOIN competitions c ON c.id = r.competition_id
		JOIN teams ht ON ht.id = m.home_team_id
		JOIN teams at ON at.id = m.away_team_id
		WHERE m.is_finished = false
		  AND m.match_date IS NOT NULL
		  AND m.match_date >= $1
		  AND m.match_date <= $2
		  AND (m.notify_sent IS NULL OR m.notify_sent = false)
	`, windowStart, windowEnd)
	if err != nil {
		log.Printf("[scheduler] notify query error: %v", err)
		return
	}
	type matchInfo struct {
		ID        int
		MatchDate time.Time
		Home      string
		Away      string
		Comp      string
	}
	var matches []matchInfo
	for rows.Next() {
		var m matchInfo
		_ = rows.Scan(&m.ID, &m.MatchDate, &m.Home, &m.Away, &m.Comp)
		matches = append(matches, m)
	}
	rows.Close()

	if len(matches) == 0 {
		return
	}

	log.Printf("[scheduler] Odesílám notifikace pro %d zápasů", len(matches))

	// Načti příjemce (uživatelé s emailem a zapnutými notifikacemi)
	uRows, err := pool.Query(ctx, `
		SELECT DISTINCT u.email, u.username
		FROM users u
		JOIN notification_settings ns ON ns.user_id = u.id
		WHERE u.email IS NOT NULL AND u.email != ''
		  AND ns.email_before_match = true
	`)
	if err != nil {
		// Tabulka notification_settings možná nemá sloupec email_before_match
		// Fallback: všichni s emailem
		uRows, err = pool.Query(ctx, `
			SELECT email, username FROM users
			WHERE email IS NOT NULL AND email != ''
		`)
		if err != nil {
			log.Printf("[scheduler] recipients query error: %v", err)
			return
		}
	}
	var recipients []notifyRecipient
	for uRows.Next() {
		var rec notifyRecipient
		_ = uRows.Scan(&rec.Email, &rec.Username)
		recipients = append(recipients, rec)
	}
	uRows.Close()

	if len(recipients) == 0 {
		return
	}

	for _, m := range matches {
		subject := fmt.Sprintf("Tipovačka — nadcházející zápas: %s vs %s", m.Home, m.Away)
		body := fmt.Sprintf(
			"Ahoj!\n\nZa %d hodin začíná zápas:\n\n%s vs %s\nSoutěž: %s\nZačátek: %s\n\nNezapomeň tipovat!\n\n%s\n",
			config.NotifyHoursBefore,
			m.Home, m.Away,
			m.Comp,
			m.MatchDate.Format("02.01.2006 15:04"),
			config.AppURL,
		)

		for _, rec := range recipients {
			if err := schedulerSendEmail(rec.Email, subject, body); err != nil {
				log.Printf("[scheduler] email error → %s: %v", rec.Email, err)
			}
		}

		// Označ jako odeslaný
		_, _ = pool.Exec(ctx, `UPDATE matches SET notify_sent=true WHERE id=$1`, m.ID)
	}
}

// schedulerSendEmail odešle email z plánovače.
// Duplikuje logiku z handlers.sendEmail (which is unexported).
func schedulerSendEmail(to, subject, body string) error {
	if !config.SMTPEnabled {
		return fmt.Errorf("SMTP není nakonfigurováno")
	}

	from := config.SMTPFrom
	if from == "" {
		from = config.SMTPUser
	}

	msg := "From: " + from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"\r\n" +
		body

	addr := fmt.Sprintf("%s:%d", config.SMTPHost, config.SMTPPort)
	auth := smtp.PlainAuth("", config.SMTPUser, config.SMTPPassword, config.SMTPHost)

	return smtp.SendMail(addr, auth, from, []string{to}, []byte(msg))
}
