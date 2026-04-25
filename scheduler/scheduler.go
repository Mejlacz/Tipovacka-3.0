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
	// Začni za 2 minuty, pak každých 30 minut
	time.Sleep(2 * time.Minute)

	ticker := time.NewTicker(30 * time.Minute)
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

// notifyTime vrátí čas kdy má odejít upozornění pro daný zápas.
//   - Noční zápas (22:00–08:00): den předem v 20:00
//   - Normální zápas: NOTIFY_HOURS_BEFORE hodin předem
func notifyTime(matchDate time.Time) time.Time {
	h := matchDate.Hour()
	if h >= 22 {
		// Zápas večer po 22:00 — upozornění ve 20:00 stejný den
		return time.Date(matchDate.Year(), matchDate.Month(), matchDate.Day(),
			20, 0, 0, 0, matchDate.Location())
	}
	if h < 8 {
		// Zápas brzy ráno (0–8h) — upozornění ve 20:00 předchozí den
		prev := matchDate.AddDate(0, 0, -1)
		return time.Date(prev.Year(), prev.Month(), prev.Day(),
			20, 0, 0, 0, prev.Location())
	}
	// Normální zápas — upozornění X hodin předem
	return matchDate.Add(-time.Duration(config.NotifyHoursBefore) * time.Hour)
}

// shouldNotifyNow vrátí true pokud má scheduler v tomto běhu odeslat upozornění.
// Okno je 30 minut (interval scheduleru), takže notifyTime musí padnout do (now-30min, now].
func shouldNotifyNow(matchDate, now time.Time) bool {
	nt := notifyTime(matchDate)
	return !nt.After(now) && nt.After(now.Add(-30*time.Minute))
}

func sendMatchNotifications(ctx context.Context, pool *pgxpool.Pool) {
	if !config.SMTPEnabled {
		return
	}

	loc, _ := time.LoadLocation("Europe/Prague")
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)

	// Načteme zápasy v okně 24h (pokryje i noční, jejichž notifyTime je dnes v 20:00
	// ale zápas je až zítra ráno)
	windowEnd := now.Add(24 * time.Hour)

	// Zápasy, které ještě nezačaly, nejsou oznámeny, soutěž aktivní
	rows, err := pool.Query(ctx, `
		SELECT m.id, m.match_date,
		       ht.name AS home, at.name AS away,
		       c.name  AS comp, c.id AS comp_id
		FROM matches m
		JOIN rounds r       ON r.id  = m.round_id
		JOIN competitions c ON c.id  = r.competition_id
		JOIN teams ht       ON ht.id = m.home_team_id
		JOIN teams at       ON at.id = m.away_team_id
		WHERE m.is_finished = false
		  AND m.match_date IS NOT NULL
		  AND m.match_date > $1
		  AND m.match_date <= $2
		  AND (m.notify_sent IS NULL OR m.notify_sent = false)
		  AND c.is_active = true
	`, now, windowEnd)
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
		CompID    int
	}
	var matches []matchInfo
	for rows.Next() {
		var m matchInfo
		_ = rows.Scan(&m.ID, &m.MatchDate, &m.Home, &m.Away, &m.Comp, &m.CompID)
		matches = append(matches, m)
	}
	rows.Close()

	if len(matches) == 0 {
		return
	}

	log.Printf("[scheduler] Notifikace: nalezeno %d zápasů v okně, filtruji na aktuální", len(matches))

	appURL := config.AppURL
	tipsURL := appURL + "/"

	for _, m := range matches {
		// Zkontroluj jestli je teď správný čas odeslat upozornění pro tento zápas
		matchDateLocal := m.MatchDate.In(loc)
		if !shouldNotifyNow(matchDateLocal, now) {
			continue
		}
		// Uživatelé s opt-in pro tuto soutěž (ne blokovaní, ne neaktivní, mají email)
		uRows, err := pool.Query(ctx, `
			SELECT u.id, u.email, u.username
			FROM users u
			JOIN notification_settings ns ON ns.user_id = u.id
			WHERE ns.competition_id = $1
			  AND u.email IS NOT NULL AND u.email != ''
			  AND COALESCE(u.is_blocked,  false) = false
			  AND COALESCE(u.is_inactive, false) = false
			  AND COALESCE(u.is_approved, true)  = true
		`, m.CompID)
		if err != nil {
			log.Printf("[scheduler] recipients query error (match %d): %v", m.ID, err)
			// Označ stejně jako odeslaný, aby se to neopakovalo
			_, _ = pool.Exec(ctx, `UPDATE matches SET notify_sent=true WHERE id=$1`, m.ID)
			continue
		}
		type recipient struct {
			ID       int
			Email    string
			Username string
		}
		var opted []recipient
		for uRows.Next() {
			var rec recipient
			_ = uRows.Scan(&rec.ID, &rec.Email, &rec.Username)
			opted = append(opted, rec)
		}
		uRows.Close()

		if len(opted) == 0 {
			_, _ = pool.Exec(ctx, `UPDATE matches SET notify_sent=true WHERE id=$1`, m.ID)
			continue
		}

		// Kdo už tipoval tento zápas?
		tRows, _ := pool.Query(ctx,
			`SELECT user_id FROM tips WHERE match_id=$1`, m.ID)
		tipped := map[int]bool{}
		for tRows.Next() {
			var uid int
			_ = tRows.Scan(&uid)
			tipped[uid] = true
		}
		tRows.Close()

		// Jen netipovaní
		var untipped []recipient
		for _, rec := range opted {
			if !tipped[rec.ID] {
				untipped = append(untipped, rec)
			}
		}

		if len(untipped) > 0 {
			matchTime := matchDateLocal.Format("02.01. 15:04")
			isNight := matchDateLocal.Hour() >= 22 || matchDateLocal.Hour() < 8
			subject := fmt.Sprintf("⏰ Ještě nemáš tip — %s vs %s", m.Home, m.Away)
			bodyHTML := buildNotifyEmailHTML(m.Home, m.Away, m.Comp, matchTime, isNight, tipsURL, appURL)

			for _, rec := range untipped {
				if err := schedulerSendEmail(rec.Email, subject, bodyHTML); err != nil {
					log.Printf("[scheduler] email chyba → %s: %v", rec.Email, err)
				}
			}
			log.Printf("[scheduler] %s vs %s: %d bez tipu, odesláno (noční=%v)", m.Home, m.Away, len(untipped), isNight)
		}

		_, _ = pool.Exec(ctx, `UPDATE matches SET notify_sent=true WHERE id=$1`, m.ID)
	}
}

// buildNotifyEmailHTML sestaví HTML tělo notifikačního emailu.
// isNight=true → zápas je noční (22:00–08:00), upozornění jde den předem.
func buildNotifyEmailHTML(home, away, comp, matchTime string, isNight bool, tipsURL, appURL string) string {
	var whenMsg string
	if isNight {
		whenMsg = "Noční zápas začíná <strong>zítra</strong> a ty ještě nemáš tip."
	} else {
		whenMsg = fmt.Sprintf("Zápas začíná za méně než <strong>%d&nbsp;hodin</strong> a ty ještě nemáš tip.", config.NotifyHoursBefore)
	}
	return fmt.Sprintf(
		`<html><body style="font-family:sans-serif;max-width:500px;margin:auto;padding:1rem;background:#f0f4f8">`+
			`<div style="background:#131f2e;color:#fff;padding:1rem 1.5rem;border-radius:8px 8px 0 0">`+
			`<h2 style="margin:0;font-size:1.1rem">⏰ Nezapomeň tipovat!</h2>`+
			`</div>`+
			`<div style="background:#fff;padding:1.5rem;border-radius:0 0 8px 8px;border:1px solid #dde3ea;border-top:none">`+
			`<p style="margin-top:0">%s</p>`+
			`<div style="background:#f8fafc;border:1px solid #e2e8f0;border-radius:8px;padding:1rem;text-align:center;margin:1.2rem 0">`+
			`<div style="font-size:1.15rem;font-weight:700;color:#0f172a">%s <span style="color:#94a3b8">vs</span> %s</div>`+
			`<div style="color:#64748b;margin-top:.3rem">🏆 %s &nbsp;·&nbsp; 🕐 %s</div>`+
			`</div>`+
			`<div style="text-align:center;margin:1.5rem 0">`+
			`<a href="%s" style="background:#10b981;color:#fff;text-decoration:none;padding:.65rem 1.8rem;border-radius:6px;font-weight:700;font-size:.95rem">Tipovat teď →</a>`+
			`</div>`+
			`<p style="color:#94a3b8;font-size:.78rem;text-align:center;margin-bottom:0">`+
			`Nastavení upozornění: <a href="%s/profile" style="color:#64748b">/profile</a>`+
			`</p>`+
			`</div>`+
			`</body></html>`,
		whenMsg,
		home, away, comp, matchTime,
		tipsURL,
		appURL,
	)
}

// schedulerSendEmail odešle HTML email z plánovače.
func schedulerSendEmail(to, subject, bodyHTML string) error {
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
		"Content-Type: text/html; charset=UTF-8\r\n" +
		"\r\n" +
		bodyHTML

	addr := fmt.Sprintf("%s:%d", config.SMTPHost, config.SMTPPort)
	auth := smtp.PlainAuth("", config.SMTPUser, config.SMTPPassword, config.SMTPHost)

	return smtp.SendMail(addr, auth, from, []string{to}, []byte(msg))
}
