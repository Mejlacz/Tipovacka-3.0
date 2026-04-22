// handlers/admin_email.go — Tipovačka 2.0
// Admin stránka pro hromadný email všem aktivním uživatelům.
package handlers

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/smtp"
	"strings"

	"tipovacka/config"
	"tipovacka/db"
	"tipovacka/middleware"
)

// sendEmail odešle plain-text email přes SMTP.
// Používá config.SMTP* proměnné.
func sendEmail(to, subject, body string) error {
	if !config.SMTPEnabled {
		return fmt.Errorf("SMTP není nakonfigurováno")
	}

	from := config.SMTPFrom
	if from == "" {
		from = config.SMTPUser
	}

	// Sestavení zprávy (RFC 2822)
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

// loadActiveEmailRecipients vrátí emaily všech aktivních uživatelů.
// Pokud compID > 0, vrátí jen uživatele kteří mají tip v dané soutěži.
func loadActiveEmailRecipients(ctx context.Context, compID int) []string {
	var rows interface{ Next() bool; Scan(...interface{}) error; Close() }
	var err error

	if compID > 0 {
		rows, err = db.Pool.Query(ctx, `
			SELECT DISTINCT u.email FROM users u
			JOIN tips t ON t.user_id = u.id
			JOIN matches m ON m.id = t.match_id
			JOIN rounds r ON r.id = m.round_id
			WHERE r.competition_id = $1
			  AND u.email IS NOT NULL AND u.email != ''
			ORDER BY u.email
		`, compID)
	} else {
		rows, err = db.Pool.Query(ctx, `
			SELECT email FROM users
			WHERE email IS NOT NULL AND email != ''
			ORDER BY email
		`)
	}
	if err != nil {
		log.Printf("[email] query error: %v", err)
		return nil
	}
	defer rows.Close()

	var emails []string
	for rows.Next() {
		var email string
		_ = rows.Scan(&email)
		if email != "" {
			emails = append(emails, email)
		}
	}
	return emails
}

// GET /admin/email
func AdminEmailForm(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		ctx := context.Background()

		// Načti soutěže pro filtr
		compRows, _ := db.Pool.Query(ctx,
			`SELECT id, name, season FROM competitions ORDER BY is_active DESC, id DESC`)
		type compOpt struct {
			ID     int
			Name   string
			Season string
		}
		var comps []compOpt
		for compRows.Next() {
			var c compOpt
			_ = compRows.Scan(&c.ID, &c.Name, &c.Season)
			comps = append(comps, c)
		}
		compRows.Close()

		// Počet příjemců (všichni s emailem)
		emails := loadActiveEmailRecipients(ctx, 0)
		flash := middleware.GetFlash(w, r)

		RenderTemplate(w, r, tmpl, "admin/email_compose.html", TemplateData{
			"User":           admin,
			"Competitions":   comps,
			"RecipientCount": len(emails),
			"Flash":          flash,
			"SMTPEnabled":    config.SMTPEnabled,
		})
	}
}

// POST /admin/email/send
func AdminEmailSend(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}

		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}

		subject := strings.TrimSpace(r.FormValue("subject"))
		body := strings.TrimSpace(r.FormValue("body"))
		compIDStr := r.FormValue("competition_id")
		compID := 0
		if compIDStr != "" {
			fmt.Sscanf(compIDStr, "%d", &compID)
		}

		if subject == "" || body == "" {
			middleware.SetFlash(w, r, "error", "Předmět a tělo zprávy jsou povinné.")
			http.Redirect(w, r, "/admin/email", http.StatusSeeOther)
			return
		}

		if !config.SMTPEnabled {
			middleware.SetFlash(w, r, "error", "SMTP není nakonfigurováno — nelze odeslat email.")
			http.Redirect(w, r, "/admin/email", http.StatusSeeOther)
			return
		}

		ctx := context.Background()
		emails := loadActiveEmailRecipients(ctx, compID)
		if len(emails) == 0 {
			middleware.SetFlash(w, r, "error", "Žádní příjemci nenalezeni.")
			http.Redirect(w, r, "/admin/email", http.StatusSeeOther)
			return
		}

		sent := 0
		failed := 0
		for _, email := range emails {
			if err := sendEmail(email, subject, body); err != nil {
				log.Printf("[email] chyba při odesílání na %s: %v", email, err)
				failed++
			} else {
				sent++
			}
		}

		msg := fmt.Sprintf("Odesláno: %d, selhalo: %d (z %d příjemců).", sent, failed, len(emails))
		flashType := "ok"
		if failed > 0 && sent == 0 {
			flashType = "error"
		}
		middleware.SetFlash(w, r, flashType, msg)
		http.Redirect(w, r, "/admin/email", http.StatusSeeOther)
	}
}
