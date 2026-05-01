// handlers/admin_email.go — Tipovačka 3.0
// Admin stránka pro hromadný email — výběr příjemců, manuální emaily, filtr soutěže.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/smtp"
	"net/url"
	"strings"

	"tipovacka/config"
	"tipovacka/db"
	"tipovacka/middleware"
)

// translateToEN přeloží text z češtiny do angličtiny přes Google Translate (free API).
func translateToEN(text string) string {
	apiURL := "https://translate.googleapis.com/translate_a/single?client=gtx&sl=cs&tl=en&dt=t&q=" + url.QueryEscape(text)
	resp, err := http.Get(apiURL) //nolint:noctx
	if err != nil {
		log.Printf("[translate] chyba: %v", err)
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Odpověď: [[[přeloženýText, originál, ...], ...], null, "cs"]
	var raw [][][]interface{}
	if err := json.Unmarshal(body, &raw); err != nil || len(raw) == 0 {
		// zkus jako obecné pole
		var rawAny []interface{}
		if err2 := json.Unmarshal(body, &rawAny); err2 != nil || len(rawAny) == 0 {
			return ""
		}
		// raw[0] je []interface{} obsahující kusy překladu
		parts, ok := rawAny[0].([]interface{})
		if !ok {
			return ""
		}
		var sb strings.Builder
		for _, p := range parts {
			if arr, ok := p.([]interface{}); ok && len(arr) > 0 {
				if s, ok := arr[0].(string); ok {
					sb.WriteString(s)
				}
			}
		}
		return sb.String()
	}
	var sb strings.Builder
	for _, chunk := range raw[0] {
		if len(chunk) > 0 {
			if s, ok := chunk[0].(string); ok {
				sb.WriteString(s)
			}
		}
	}
	return sb.String()
}

// sendEmail odešle plain-text email přes SMTP.
func sendEmail(to, subject, body string) error {
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

// EmailUser je uživatel s emailem pro výběr příjemců.
type EmailUser struct {
	ID         int
	Username   string
	Email      string
	CompIDs    []int // soutěže kde má tipy (pro JS filtr)
	IsInactive bool
}

// GET /admin/email
func AdminEmailForm(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		ctx := context.Background()

		// Načti uživatele s emailem — vyřadit blokované a neaktivní
		inactiveSel := "false"
		if userCols.IsInactive {
			inactiveSel = "COALESCE(is_inactive, false)"
		}
		blockedWhere := ""
		if userCols.IsBlocked {
			blockedWhere += " AND COALESCE(is_blocked, false) = false"
		}
		if userCols.IsInactive {
			blockedWhere += " AND COALESCE(is_inactive, false) = false"
		}
		uRows, _ := db.Pool.Query(ctx,
			`SELECT id, username, COALESCE(email,''), `+inactiveSel+` FROM users
			  WHERE email IS NOT NULL AND email != '' `+blockedWhere+`
			  ORDER BY username`)
		var users []EmailUser
		for uRows.Next() {
			var u EmailUser
			_ = uRows.Scan(&u.ID, &u.Username, &u.Email, &u.IsInactive)
			u.CompIDs = []int{}
			users = append(users, u)
		}
		uRows.Close()

		// Pro každého uživatele zjisti soutěže kde tipoval
		if len(users) > 0 {
			// index uid → position in slice
			idxByID := map[int]int{}
			for i, u := range users {
				idxByID[u.ID] = i
			}
			cRows, _ := db.Pool.Query(ctx,
				`SELECT DISTINCT t.user_id, r.competition_id
				   FROM tips t
				   JOIN matches m ON m.id = t.match_id
				   JOIN rounds r ON r.id = m.round_id
				   WHERE t.user_id = ANY(
				       SELECT id FROM users WHERE email IS NOT NULL AND email != ''
				   )`)
			for cRows.Next() {
				var uid, cid int
				if err := cRows.Scan(&uid, &cid); err == nil {
					if i, ok := idxByID[uid]; ok {
						users[i].CompIDs = append(users[i].CompIDs, cid)
					}
				}
			}
			cRows.Close()
		}

		// Serialize CompIDs as JSON per user for JS
		type emailUserJS struct {
			ID         int
			Username   string
			Email      string
			CompIDs    template.JS
			IsInactive bool
		}
		var usersJS []emailUserJS
		for _, u := range users {
			compIDsJSON, _ := json.Marshal(u.CompIDs)
			usersJS = append(usersJS, emailUserJS{
				ID:         u.ID,
				Username:   u.Username,
				Email:      u.Email,
				CompIDs:    template.JS(compIDsJSON),
				IsInactive: u.IsInactive,
			})
		}

		// Načti soutěže pro filtr dropdown
		type compOpt struct {
			ID     int
			Name   string
			Season string
		}
		compRows, _ := db.Pool.Query(ctx,
			`SELECT id, name, season FROM competitions ORDER BY is_active DESC, sort_order ASC NULLS LAST, id DESC`)
		var comps []compOpt
		for compRows.Next() {
			var c compOpt
			_ = compRows.Scan(&c.ID, &c.Name, &c.Season)
			comps = append(comps, c)
		}
		compRows.Close()

		flash := middleware.GetFlash(w, r)
		RenderTemplate(w, r, tmpl, "admin/email_compose.html", TemplateData{
			"User":        admin,
			"Users":       usersJS,
			"Competitions": comps,
			"Flash":       flash,
			"SMTPEnabled": config.SMTPEnabled,
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

		// Vybraní příjemci (checkboxy) + manuální emaily
		selected := r.Form["to"]
		manual := r.FormValue("manual_emails")

		seen := map[string]bool{}
		var emails []string
		for _, e := range selected {
			e = strings.TrimSpace(strings.ToLower(e))
			if e != "" && !seen[e] {
				seen[e] = true
				emails = append(emails, e)
			}
		}
		// Manuální emaily oddělené čárkou nebo mezerou
		for _, e := range strings.FieldsFunc(manual, func(r rune) bool {
			return r == ',' || r == ' ' || r == ';' || r == '\n'
		}) {
			e = strings.TrimSpace(strings.ToLower(e))
			if e != "" && !seen[e] {
				seen[e] = true
				emails = append(emails, e)
			}
		}

		if len(emails) == 0 {
			middleware.SetFlash(w, r, "error", "Žádní příjemci — vyber uživatele nebo zadej emaily ručně.")
			http.Redirect(w, r, "/admin/email", http.StatusSeeOther)
			return
		}

		// Přeložit do EN a přidat pod CS text (stejný email pro všechny)
		finalSubject := subject
		finalBody := body
		if r.FormValue("translate_en") == "1" {
			enSubject := translateToEN(subject)
			enBody := translateToEN(body)
			if enSubject != "" || enBody != "" {
				finalSubject = subject + " / " + enSubject
				finalBody = body + "\n\n---\n\n🇬🇧 English translation:\n\n" + enBody
			}
		}

		sent, failed := 0, 0
		for _, email := range emails {
			if err := sendEmail(email, finalSubject, finalBody); err != nil {
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
