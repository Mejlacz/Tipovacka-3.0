// handlers/notify.go — Tipovačka 2.0
// Sdílená logika pro okamžité odeslání emailové notifikace pro jeden zápas.
// Volá se z AdminMatchNotifyNow; scheduler volá vlastní sendMatchNotifications.
package handlers

import (
	"context"
	"fmt"
	"log"
	"net/smtp"
	"time"

	"tipovacka/config"
	"tipovacka/db"
)

// sendMatchNotificationForID okamžitě odešle email upozornění na zápas matchID
// uživatelům, kteří mají opt-in pro danou soutěž a ještě netipovali.
// Ignoruje shouldNotifyNow timing — admin spustil ručně.
func sendMatchNotificationForID(matchID int) {
	if !config.SMTPEnabled {
		log.Printf("[notify-now] SMTP není nakonfigurováno")
		return
	}

	ctx := context.Background()
	loc, _ := time.LoadLocation("Europe/Prague")
	if loc == nil {
		loc = time.UTC
	}

	// Načti info o zápase
	type matchInfo struct {
		Home      string
		Away      string
		Comp      string
		CompID    int
		MatchDate *time.Time
	}
	var m matchInfo
	err := db.Pool.QueryRow(ctx, `
		SELECT ht.name, at.name, c.name, c.id, m.match_date
		FROM matches m
		JOIN competitions c ON c.id  = m.competition_id
		JOIN teams ht       ON ht.id = m.home_team_id
		JOIN teams at       ON at.id = m.away_team_id
		WHERE m.id = $1 AND m.is_finished = false
	`, matchID).Scan(&m.Home, &m.Away, &m.Comp, &m.CompID, &m.MatchDate)
	if err != nil {
		log.Printf("[notify-now] zápas %d nenalezen nebo již dokončen: %v", matchID, err)
		return
	}

	// Všichni aktivní tipéři v soutěži — tj. mají aspoň jeden tip v soutěži,
	// mají email, nejsou blokovaní/neaktivní. Bez opt-in filtru — ruční zvoneček
	// jde záměrně všem kdo v soutěži hraje.
	uRows, err := db.Pool.Query(ctx, `
		SELECT DISTINCT u.id, u.email, u.username
		FROM users u
		JOIN tips t   ON t.user_id  = u.id
		JOIN matches m2 ON m2.id    = t.match_id
		WHERE m2.competition_id = $1
		  AND u.email IS NOT NULL AND u.email != ''
		  AND COALESCE(u.is_blocked,  false) = false
		  AND COALESCE(u.is_inactive, false) = false
		  AND COALESCE(u.is_approved, true)  = true
	`, m.CompID)
	if err != nil {
		log.Printf("[notify-now] recipients query error: %v", err)
		return
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
		log.Printf("[notify-now] %s vs %s: žádní příjemci s opt-in", m.Home, m.Away)
		_, _ = db.Pool.Exec(ctx, `UPDATE matches SET notify_sent=true WHERE id=$1`, matchID)
		return
	}

	// Kdo už tipoval?
	tRows, _ := db.Pool.Query(ctx, `SELECT user_id FROM tips WHERE match_id=$1`, matchID)
	tipped := map[int]bool{}
	for tRows.Next() {
		var uid int
		_ = tRows.Scan(&uid)
		tipped[uid] = true
	}
	tRows.Close()

	var untipped []recipient
	for _, rec := range opted {
		if !tipped[rec.ID] {
			untipped = append(untipped, rec)
		}
	}

	if len(untipped) == 0 {
		log.Printf("[notify-now] %s vs %s: všichni již tipovali", m.Home, m.Away)
		_, _ = db.Pool.Exec(ctx, `UPDATE matches SET notify_sent=true WHERE id=$1`, matchID)
		return
	}

	matchTime := "?"
	isNight := false
	if m.MatchDate != nil {
		// m.MatchDate is Prague wall-clock labeled as UTC (pgx v5 TIMESTAMP WITHOUT TIME ZONE).
		// Rebuild as real Prague time.
		md := time.Date(m.MatchDate.Year(), m.MatchDate.Month(), m.MatchDate.Day(),
			m.MatchDate.Hour(), m.MatchDate.Minute(), m.MatchDate.Second(), m.MatchDate.Nanosecond(), loc)
		matchTime = md.Format("02.01. 15:04")
		isNight = md.Hour() >= 22 || md.Hour() < 8
	}

	appURL := config.AppURL
	tipsURL := appURL + "/"
	subject := fmt.Sprintf("⏰ Ještě nemáš tip — %s vs %s", m.Home, m.Away)
	bodyHTML := notifyBuildEmailHTML(m.Home, m.Away, m.Comp, matchTime, isNight, tipsURL, appURL)

	sent := 0
	for _, rec := range untipped {
		if err := notifySendEmail(rec.Email, subject, bodyHTML); err != nil {
			log.Printf("[notify-now] email chyba → %s: %v", rec.Email, err)
		} else {
			sent++
		}
	}
	log.Printf("[notify-now] %s vs %s: odesláno %d/%d emailů", m.Home, m.Away, sent, len(untipped))

	_, _ = db.Pool.Exec(ctx, `UPDATE matches SET notify_sent=true WHERE id=$1`, matchID)
}

// notifyBuildEmailHTML — HTML tělo notifikačního emailu.
func notifyBuildEmailHTML(home, away, comp, matchTime string, isNight bool, tipsURL, appURL string) string {
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

// notifySendEmail odešle HTML email.
func notifySendEmail(to, subject, bodyHTML string) error {
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
