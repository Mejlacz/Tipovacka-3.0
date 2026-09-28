package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"tipovacka/db"
)

var paymentCompetitions = []struct {
	ID         int64
	Name       string
	SportLabel string
}{
	{1172900132417011713, "MS hokej 2026", "🏒 Hokej"},
	{1181336879584051202, "MS fotbal 2026", "⚽ Fotbal"},
	{1206995494455181314, "LM 2026/27", "⚽ Fotbal"},
}

type PaymentUser struct {
	UserID   int64
	Username string
	Email    string
}

type CompPaymentCard struct {
	ID         int64
	Name       string
	SportLabel string
	Paid       int
	Total      int
	Pct        int
	ArcOffset  float64 // stroke-dashoffset pro SVG arc (circumference=220)
}

type CompPaymentDetail struct {
	ID           int64
	Name         string
	Unpaid       []PaymentUser
	Paid         []PaymentUser
	Excluded     []PaymentUser
	ReminderDate string // YYYY-MM-DD, prázdné = neaktivní
	Settings     PaymentSettings
}

// PaymentSettings obsahuje platební/bankovní údaje soutěže.
type PaymentSettings struct {
	BankAccount    string `json:"bank_account"`
	BankName       string `json:"bank_name"`
	VariableSymbol string `json:"variable_symbol"`
	Amount         string `json:"amount"`
	Note           string `json:"note"`
	EmailText      string `json:"email_text"`
	QR1URL         string `json:"-"`
	QR2URL         string `json:"-"`
}

// defaultEmailText vrátí výchozí text emailu pro danou soutěž.
func defaultEmailText(compName string) string {
	return "Ahoj NICK,\n\npřipomínáme ti, že jsi ještě nezaplatil/a za soutěž " + compName + ".\n\nProsím zašli platbu co nejdříve. Pokud máš dotazy, obrať se na správce tipovačky."
}

// payReminderKey vrátí klíč pro app_config pro datum připomenutí platby.
func payReminderKey(compID int64) string {
	return fmt.Sprintf("pay_reminder_%d", compID)
}

// paySettingsKey vrátí klíč pro app_config pro platební nastavení soutěže.
func paySettingsKey(compID int64) string {
	return fmt.Sprintf("pay_settings_%d", compID)
}

// payQRPath vrátí cestu na disku k QR kódu (num=1 nebo 2).
func payQRPath(compID int64, num int) string {
	return filepath.Join("static", "uploads", "qr", fmt.Sprintf("%d_%d.png", compID, num))
}

// payQRURL vrátí URL cestu k QR kódu (num=1 nebo 2).
func payQRURL(compID int64, num int) string {
	return fmt.Sprintf("/static/uploads/qr/%d_%d.png", compID, num)
}

// loadPaymentSettings načte platební nastavení a QR kódy z app_config + disku.
func loadPaymentSettings(ctx context.Context, compID int64) PaymentSettings {
	var s PaymentSettings
	var raw string
	_ = db.Pool.QueryRow(ctx, `SELECT value FROM app_config WHERE key=$1`, paySettingsKey(compID)).Scan(&raw)
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &s)
	}
	if _, err := os.Stat(payQRPath(compID, 1)); err == nil {
		s.QR1URL = payQRURL(compID, 1)
	}
	if _, err := os.Stat(payQRPath(compID, 2)); err == nil {
		s.QR2URL = payQRURL(compID, 2)
	}
	return s
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

		const arcCircumference = 220.0
		cards := make([]CompPaymentCard, 0, len(paymentCompetitions))
		for _, c := range paymentCompetitions {
			total := totalMap[c.ID]
			paid := paidMap[c.ID]
			pct := 0
			arcOffset := 0.0
			if total > 0 {
				pct = paid * 100 / total
				arcOffset = arcCircumference * float64(total-paid) / float64(total)
			}
			cards = append(cards, CompPaymentCard{
				ID:         c.ID,
				Name:       c.Name,
				SportLabel: c.SportLabel,
				Paid:       paid,
				Total:      total,
				Pct:        pct,
				ArcOffset:  arcOffset,
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

		detail.Settings = loadPaymentSettings(ctx, compID)
		if detail.Settings.EmailText == "" {
			detail.Settings.EmailText = defaultEmailText(compName)
		}

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

// POST /admin/payments/{comp_id}/save-settings — uloží platební nastavení (AJAX)
func AdminPaymentSaveSettings(w http.ResponseWriter, r *http.Request) {
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
	if err := r.ParseForm(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "bad_form"})
		return
	}
	s := PaymentSettings{
		BankAccount:    strings.TrimSpace(r.FormValue("bank_account")),
		BankName:       strings.TrimSpace(r.FormValue("bank_name")),
		VariableSymbol: strings.TrimSpace(r.FormValue("variable_symbol")),
		Amount:         strings.TrimSpace(r.FormValue("amount")),
		Note:           strings.TrimSpace(r.FormValue("note")),
		EmailText:      strings.TrimSpace(r.FormValue("email_text")),
	}
	data, _ := json.Marshal(s)
	ctx := context.Background()
	_, _ = db.Pool.Exec(ctx,
		`INSERT INTO app_config (key, value) VALUES ($1,$2)
		 ON CONFLICT (key) DO UPDATE SET value=$2`,
		paySettingsKey(compID), string(data))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// POST /admin/payments/{comp_id}/upload-qr/{num} — nahraje QR kód (AJAX, multipart)
func AdminPaymentUploadQR(w http.ResponseWriter, r *http.Request) {
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
	num, err := strconv.Atoi(r.PathValue("num"))
	if err != nil || (num != 1 && num != 2) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "bad_num"})
		return
	}

	if err := r.ParseMultipartForm(5 << 20); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "bad_upload"})
		return
	}
	file, _, err := r.FormFile("qr")
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "no_file"})
		return
	}
	defer file.Close()

	destDir := filepath.Join("static", "uploads", "qr")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "mkdir"})
		return
	}

	destPath := payQRPath(compID, num)
	out, err := os.Create(destPath)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "create_file"})
		return
	}
	defer out.Close()
	if _, err = io.Copy(out, file); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "write_file"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": payQRURL(compID, num)})
}

// POST /admin/payments/{comp_id}/delete-qr/{num} — smaže QR kód (AJAX)
func AdminPaymentDeleteQR(w http.ResponseWriter, r *http.Request) {
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
	num, err := strconv.Atoi(r.PathValue("num"))
	if err != nil || (num != 1 && num != 2) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "bad_num"})
		return
	}
	_ = os.Remove(payQRPath(compID, num))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
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

	settings := loadPaymentSettings(ctx, compID)

	sent, skipped := 0, 0
	for uRows.Next() {
		var uid int64
		var uname, email string
		if e := uRows.Scan(&uid, &uname, &email); e != nil {
			continue
		}
		subject := "Tipovačka — připomínka platby za " + compName
		body, imgs := paymentReminderEmailHTML(uname, compName, settings)
		if e := sendEmailHTMLWithImages(email, subject, body, imgs); e != nil {
			skipped++
		} else {
			sent++
		}
	}
	uRows.Close()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "sent": sent, "skipped": skipped})
}

// GET /api/payment-reminder — vrátí soutěže kde aktuální uživatel nezaplatil a běží připomenutí.
// Pouze aktivní soutěže (is_active=true); 2 DB dotazy celkem.
func AdminPaymentReminderAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	empty := func() { json.NewEncoder(w).Encode(map[string]any{"reminders": []struct{}{}}) }

	u := GetCurrentUser(r)
	if u == nil {
		empty()
		return
	}

	ctx := context.Background()
	compIDs := make([]int64, len(paymentCompetitions))
	for i, c := range paymentCompetitions {
		compIDs[i] = c.ID
	}

	// Dotaz 1: aktivní soutěže z našeho seznamu s platným datem připomenutí
	rows1, err := db.Pool.Query(ctx, `
		SELECT c.id, c.name
		FROM competitions c
		JOIN app_config ac ON ac.key = 'pay_reminder_' || c.id::text
		WHERE c.id = ANY($1)
		  AND c.is_active = true
		  AND ac.value != ''
		  AND ac.value::date <= CURRENT_DATE
	`, compIDs)
	if err != nil {
		empty()
		return
	}
	type activeComp struct {
		id   int64
		name string
	}
	var active []activeComp
	for rows1.Next() {
		var ac activeComp
		_ = rows1.Scan(&ac.id, &ac.name)
		active = append(active, ac)
	}
	rows1.Close()

	if len(active) == 0 {
		empty()
		return
	}

	activeIDs := make([]int64, len(active))
	for i, ac := range active {
		activeIDs[i] = ac.id
	}

	// Dotaz 2: stav uživatele (tipoval? zaplatil? vyloučen?) pro všechny aktivní soutěže najednou
	rows2, err := db.Pool.Query(ctx, `
		SELECT DISTINCT m.competition_id,
		       COALESCE(cp.paid, false),
		       COALESCE(cp.excluded, false)
		FROM tips t
		JOIN matches m ON m.id = t.match_id
		LEFT JOIN competition_payments cp
		       ON cp.user_id = t.user_id AND cp.competition_id = m.competition_id
		WHERE t.user_id = $1
		  AND m.competition_id = ANY($2)
	`, u.ID, activeIDs)
	if err != nil {
		empty()
		return
	}
	type userStatus struct{ paid, excluded bool }
	statusMap := map[int64]userStatus{}
	for rows2.Next() {
		var cid int64
		var paid, excluded bool
		_ = rows2.Scan(&cid, &paid, &excluded)
		statusMap[cid] = userStatus{paid: paid, excluded: excluded}
	}
	rows2.Close()

	type reminder struct {
		CompName string `json:"comp_name"`
	}
	var reminders []reminder
	for _, ac := range active {
		st, tipped := statusMap[ac.id]
		if !tipped {
			continue // uživatel v této soutěži netipoval
		}
		if st.paid || st.excluded {
			continue
		}
		reminders = append(reminders, reminder{CompName: ac.name})
	}

	if reminders == nil {
		reminders = []reminder{}
	}
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

func paymentReminderEmailHTML(username, compName string, s PaymentSettings) (string, []EmailInlineImage) {
	// Platební údaje
	var payBlock string
	hasDetails := s.BankAccount != "" || s.VariableSymbol != "" || s.Amount != ""
	if hasDetails {
		rows := ""
		if s.BankAccount != "" {
			rows += `<tr><td style="color:#64748b;padding-right:12px;white-space:nowrap">Číslo účtu:</td><td><strong>` + s.BankAccount + `</strong></td></tr>`
		}
		if s.BankName != "" {
			rows += `<tr><td style="color:#64748b;padding-right:12px">Banka:</td><td>` + s.BankName + `</td></tr>`
		}
		if s.VariableSymbol != "" {
			rows += `<tr><td style="color:#64748b;padding-right:12px;white-space:nowrap">Variabilní symbol:</td><td><strong>` + s.VariableSymbol + `</strong></td></tr>`
		}
		if s.Amount != "" {
			rows += `<tr><td style="color:#64748b;padding-right:12px">Částka:</td><td><strong>` + s.Amount + ` Kč</strong></td></tr>`
		}
		if s.Note != "" {
			note := strings.ReplaceAll(s.Note, "NICK", username)
			rows += `<tr><td style="color:#64748b;padding-right:12px;vertical-align:top">Poznámka:</td><td>` + note + `</td></tr>`
		}
		payBlock = `<div style="background:#f1f5f9;border-left:3px solid #1e40af;padding:12px 16px;border-radius:4px;margin:16px 0">
<div style="font-weight:700;margin-bottom:8px;color:#1e40af">📋 Platební údaje</div>
<table style="border-collapse:collapse;font-size:.92rem">` + rows + `</table></div>`
	}

	// QR kódy — CID inline embedding (funguje ve všech email klientech vč. Gmailu)
	qrLabels := []string{"S příspěvkem na provoz", "Bez příspěvku na provoz"}
	var inlineImages []EmailInlineImage
	type qrEntry struct{ label, cid string }
	var qrEntries []qrEntry
	for i, qrURL := range []string{s.QR1URL, s.QR2URL} {
		if qrURL == "" {
			continue
		}
		diskPath := strings.TrimPrefix(qrURL, "/")
		data, err := os.ReadFile(diskPath)
		if err != nil {
			continue
		}
		cid := fmt.Sprintf("qr%d@tipovacka", i+1)
		inlineImages = append(inlineImages, EmailInlineImage{ContentID: cid, MIMEType: "image/png", Data: data})
		qrEntries = append(qrEntries, qrEntry{label: qrLabels[i], cid: cid})
	}
	var qrBlock string
	if len(qrEntries) > 0 {
		imgs := ""
		for _, q := range qrEntries {
			imgs += `<div style="text-align:center;display:inline-block;margin:8px 16px">` +
				`<div style="font-size:.78rem;color:#64748b;margin-bottom:4px">` + q.label + `</div>` +
				`<img src="cid:` + q.cid + `" style="max-width:180px;height:auto;display:block" alt="` + q.label + `">` +
				`</div>`
		}
		qrBlock = `<div style="margin:16px 0;text-align:center">` +
			`<div style="font-weight:600;margin-bottom:8px;font-size:.9rem">📱 QR kódy pro platbu</div>` + imgs + `</div>`
	}

	// Hlavní text emailu — vlastní nebo výchozí
	emailText := s.EmailText
	if emailText == "" {
		emailText = defaultEmailText(compName)
	}
	emailText = strings.ReplaceAll(emailText, "NICK", username)

	// Převeď odřádkování na HTML odstavce
	paragraphs := ""
	for _, para := range strings.Split(strings.TrimSpace(emailText), "\n\n") {
		para = strings.TrimSpace(para)
		if para != "" {
			line := strings.ReplaceAll(para, "\n", "<br>")
			paragraphs += `<p>` + line + `</p>`
		}
	}

	html := `<!DOCTYPE html><html><body style="font-family:sans-serif;max-width:520px;margin:2rem auto;color:#1e293b">` +
		`<h2 style="color:#1e40af">💰 Připomínka platby — Tipovačka</h2>` +
		paragraphs +
		payBlock +
		qrBlock +
		`<p style="color:#64748b;font-size:.85rem">— Tipovačka 3.0</p>` +
		`</body></html>`
	return html, inlineImages
}
