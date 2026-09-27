package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"tipovacka/config"
	"tipovacka/db"
)

var paymentCompetitions = []struct {
	ID   int64
	Name string
}{
	{1172900132417011713, "MS hokej 2026"},
	{1181336879584051202, "MS fotbal 2026"},
	{1206995494455181314, "LM 2026/27"},
}

type PaymentUser struct {
	UserID   int64
	Username string
	Email    string
}

type CompPaymentCard struct {
	ID    int64
	Name  string
	Paid  int
	Total int
	Pct   int
}

type CompPaymentDetail struct {
	ID           int64
	Name         string
	Unpaid       []PaymentUser
	Paid         []PaymentUser
	Excluded     []PaymentUser
	ReminderDate string // YYYY-MM-DD, prázdné = neaktivní
}

// payReminderKey vrátí klíč pro app_config pro datum připomenutí platby.
func payReminderKey(compID int64) string {
	return fmt.Sprintf("pay_reminder_%d", compID)
}

// GET /admin/payments
func AdminPaymentOverview(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		ctx := context.Background()

		compIDs := make([]int64, len(paymentCompetitions))
		for i, c := range paymentCompetitions {
			compIDs[i] = c.ID
		}

		totalRows, err := db.Pool.Query(ctx, `
			SELECT m.competition_id, COUNT(DISTINCT u.id)
			FROM users u
			JOIN tips t ON t.user_id = u.id
			JOIN matches m ON m.id = t.match_id
			LEFT JOIN competition_payments cp ON cp.user_id = u.id AND cp.competition_id = m.competition_id
			WHERE m.competition_id = ANY($1)
			  AND COALESCE(u.is_inactive, false) = false
			  AND COALESCE(u.is_hidden, false) = false
			  AND COALESCE(cp.excluded, false) = false
			GROUP BY m.competition_id
		`, compIDs)
		if err != nil {
			http.Error(w, "DB error: "+err.Error(), 500)
			return
		}
		totalMap := map[int64]int{}
		for totalRows.Next() {
			var cid int64
			var n int
			_ = totalRows.Scan(&cid, &n)
			totalMap[cid] = n
		}
		totalRows.Close()

		paidRows, _ := db.Pool.Query(ctx,
			`SELECT competition_id, COUNT(*) FROM competition_payments
			 WHERE competition_id = ANY($1) AND paid = true AND excluded = false
			 GROUP BY competition_id`, compIDs)
		paidMap := map[int64]int{}
		if paidRows != nil {
			for paidRows.Next() {
				var cid int64
				var n int
				_ = paidRows.Scan(&cid, &n)
				paidMap[cid] = n
			}
			paidRows.Close()
		}

		cards := make([]CompPaymentCard, 0, len(paymentCompetitions))
		for _, c := range paymentCompetitions {
			total := totalMap[c.ID]
			paid := paidMap[c.ID]
			pct := 0
			if total > 0 {
				pct = paid * 100 / total
			}
			cards = append(cards, CompPaymentCard{
				ID:    c.ID,
				Name:  c.Name,
				Paid:  paid,
				Total: total,
				Pct:   pct,
			})
		}

		RenderTemplate(w, r, tmpl, "admin/payment_overview.html", TemplateData{
			"User":  admin,
			"Cards": cards,
		})
	}
}

// GET /admin/payments/{comp_id}
func AdminPaymentDetail(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		compID, err := strconv.ParseInt(r.PathValue("comp_id"), 10, 64)
		if err != nil {
			http.Error(w, "bad comp_id", 400)
			return
		}

		var compName string
		for _, c := range paymentCompetitions {
			if c.ID == compID {
				compName = c.Name
				break
			}
		}
		if compName == "" {
			http.Error(w, "unknown competition", 404)
			return
		}

		ctx := context.Background()
		rows, err := db.Pool.Query(ctx, `
			SELECT DISTINCT u.id, u.username, COALESCE(u.email,''),
			       COALESCE(cp.paid, false), COALESCE(cp.excluded, false)
			FROM users u
			JOIN tips t ON t.user_id = u.id
			JOIN matches m ON m.id = t.match_id
			LEFT JOIN competition_payments cp
			       ON cp.user_id = u.id AND cp.competition_id = $1
			WHERE m.competition_id = $1
			  AND COALESCE(u.is_inactive, false) = false
			  AND COALESCE(u.is_hidden, false) = false
			ORDER BY u.username
		`, compID)
		if err != nil {
			http.Error(w, "DB error: "+err.Error(), 500)
			return
		}

		detail := CompPaymentDetail{ID: compID, Name: compName}
		for rows.Next() {
			var uid int64
			var uname, email string
			var paid, excluded bool
			if err := rows.Scan(&uid, &uname, &email, &paid, &excluded); err != nil {
				continue
			}
			u := PaymentUser{UserID: uid, Username: uname, Email: email}
			switch {
			case excluded:
				detail.Excluded = append(detail.Excluded, u)
			case paid:
				detail.Paid = append(detail.Paid, u)
			default:
				detail.Unpaid = append(detail.Unpaid, u)
			}
		}
		rows.Close()

		// Načti datum připomenutí z app_config
		_ = db.Pool.QueryRow(ctx,
			`SELECT value FROM app_config WHERE key=$1`, payReminderKey(compID)).Scan(&detail.ReminderDate)

		RenderTemplate(w, r, tmpl, "admin/payment_detail.html", TemplateData{
			"User":   admin,
			"Detail": detail,
		})
	}
}

// POST /admin/payments/{comp_id}/set-reminder-date — uloží datum připomenutí
func AdminPaymentSetReminderDate(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	compID, err := strconv.ParseInt(r.PathValue("comp_id"), 10, 64)
	if err != nil {
		http.Error(w, "bad comp_id", 400)
		return
	}
	date := strings.TrimSpace(r.FormValue("reminder_date"))
	ctx := context.Background()
	key := payReminderKey(compID)
	if date == "" {
		_, _ = db.Pool.Exec(ctx, `DELETE FROM app_config WHERE key=$1`, key)
	} else {
		_, _ = db.Pool.Exec(ctx,
			`INSERT INTO app_config (key, value) VALUES ($1,$2)
			 ON CONFLICT (key) DO UPDATE SET value=$2`, key, date)
	}
	http.Redirect(w, r, "/admin/payments/"+strconv.FormatInt(compID, 10), http.StatusSeeOther)
}

// POST /admin/payments/{comp_id}/send-reminder — pošle email vybraným hráčům
func AdminPaymentSendReminder(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "forbidden"})
		return
	}
	compID, err := strconv.ParseInt(r.PathValue("comp_id"), 10, 64)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "bad_id"})
		return
	}
	var compName string
	for _, c := range paymentCompetitions {
		if c.ID == compID {
			compName = c.Name
			break
		}
	}

	if err := r.ParseForm(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "bad_form"})
		return
	}
	userIDStrs := r.Form["user_ids[]"]
	if len(userIDStrs) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "no_users"})
		return
	}

	// Načti emaily vybraných hráčů
	ctx := context.Background()
	userIDs := make([]int64, 0, len(userIDStrs))
	for _, s := range userIDStrs {
		id, e := strconv.ParseInt(s, 10, 64)
		if e == nil {
			userIDs = append(userIDs, id)
		}
	}

	uRows, err := db.Pool.Query(ctx,
		`SELECT id, username, email FROM users WHERE id = ANY($1) AND email IS NOT NULL AND email != ''`,
		userIDs)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "db_error"})
		return
	}

	sent, skipped := 0, 0
	for uRows.Next() {
		var uid int64
		var uname, email string
		if e := uRows.Scan(&uid, &uname, &email); e != nil {
			continue
		}
		subject := "Tipovačka — připomínka platby za " + compName
		body := paymentReminderEmailHTML(uname, compName)
		if e := sendEmailHTML(email, subject, body); e != nil {
			skipped++
		} else {
			sent++
		}
	}
	uRows.Close()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "sent": sent, "skipped": skipped})
}

// GET /api/payment-reminder — vrátí seznam soutěží, kde aktuální uživatel nezaplatil a je aktivní připomenutí
func AdminPaymentReminderAPI(w http.ResponseWriter, r *http.Request) {
	u := GetCurrentUser(r)
	if u == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"reminders": []string{}})
		return
	}

	ctx := context.Background()
	type reminder struct {
		CompName string `json:"comp_name"`
	}
	var reminders []reminder

	for _, c := range paymentCompetitions {
		// Zkontroluj, zda je aktivní datum připomenutí
		var dateStr string
		if err := db.Pool.QueryRow(ctx,
			`SELECT value FROM app_config WHERE key=$1`, payReminderKey(c.ID)).Scan(&dateStr); err != nil || dateStr == "" {
			continue
		}
		// Datum musí být v minulosti nebo dnes
		if db.Pool.QueryRow(ctx, `SELECT $1::date <= CURRENT_DATE`, dateStr).Scan(new(bool)) != nil {
			continue
		}
		var isPast bool
		if err := db.Pool.QueryRow(ctx, `SELECT $1::date <= CURRENT_DATE`, dateStr).Scan(&isPast); err != nil || !isPast {
			continue
		}
		// Zkontroluj zda uživatel tipoval v soutěži
		var tipped bool
		_ = db.Pool.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM tips t JOIN matches m ON m.id=t.match_id
				WHERE t.user_id=$1 AND m.competition_id=$2
			)`, u.ID, c.ID).Scan(&tipped)
		if !tipped {
			continue
		}
		// Zkontroluj zda zaplatil nebo je vyloučen
		var paid, excluded bool
		_ = db.Pool.QueryRow(ctx,
			`SELECT COALESCE(paid,false), COALESCE(excluded,false)
			 FROM competition_payments WHERE user_id=$1 AND competition_id=$2`,
			u.ID, c.ID).Scan(&paid, &excluded)
		if paid || excluded {
			continue
		}
		reminders = append(reminders, reminder{CompName: c.Name})
	}

	if reminders == nil {
		reminders = []reminder{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"reminders": reminders})
}

// POST /admin/payments/{comp_id}/{user_id}/toggle-exclude  (AJAX)
func AdminPaymentExclude(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "forbidden"})
		return
	}
	compID, err1 := strconv.ParseInt(r.PathValue("comp_id"), 10, 64)
	uid, err2 := strconv.ParseInt(r.PathValue("user_id"), 10, 64)
	if err1 != nil || err2 != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "bad_id"})
		return
	}

	ctx := context.Background()
	var cur bool
	_ = db.Pool.QueryRow(ctx,
		`SELECT excluded FROM competition_payments WHERE user_id=$1 AND competition_id=$2 LIMIT 1`,
		uid, compID).Scan(&cur)

	newVal := !cur
	_, _ = db.Pool.Exec(ctx, `
		INSERT INTO competition_payments (user_id, competition_id, excluded, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (user_id, competition_id) DO UPDATE SET excluded=$3, updated_at=now()
	`, uid, compID, newVal)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "excluded": newVal})
}

// POST /admin/payments/{comp_id}/{user_id}/toggle-paid  (AJAX)
func AdminPaymentToggle(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "forbidden"})
		return
	}
	compID, err1 := strconv.ParseInt(r.PathValue("comp_id"), 10, 64)
	uid, err2 := strconv.ParseInt(r.PathValue("user_id"), 10, 64)
	if err1 != nil || err2 != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "bad_id"})
		return
	}

	ctx := context.Background()
	var cur bool
	_ = db.Pool.QueryRow(ctx,
		`SELECT paid FROM competition_payments WHERE user_id=$1 AND competition_id=$2 LIMIT 1`,
		uid, compID).Scan(&cur)

	newVal := !cur
	_, _ = db.Pool.Exec(ctx, `
		INSERT INTO competition_payments (user_id, competition_id, paid, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (user_id, competition_id) DO UPDATE SET paid=$3, updated_at=now()
	`, uid, compID, newVal)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "paid": newVal})
}

func paymentReminderEmailHTML(username, compName string) string {
	_ = config.SMTPEnabled // ensure import used
	return `<!DOCTYPE html><html><body style="font-family:sans-serif;max-width:520px;margin:2rem auto;color:#1e293b">
<h2 style="color:#1e40af">💰 Připomínka platby — Tipovačka</h2>
<p>Ahoj <strong>` + username + `</strong>,</p>
<p>připomínáme ti, že jsi ještě nezaplatil za soutěž <strong>` + compName + `</strong>.</p>
<p>Prosím zašli platbu svému správci tipovačky co nejdříve.</p>
<p style="color:#64748b;font-size:.85rem">— Tipovačka 3.0</p>
</body></html>`
}
