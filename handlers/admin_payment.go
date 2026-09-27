package handlers

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"

	"tipovacka/db"
)

// Soutěže zobrazované v přehledu plateb — ID + zobrazovaný název
var paymentCompetitions = []struct {
	ID   int64
	Name string
}{
	{1172900132417011713, "MS hokej 2026"},
	{1181336879584051202, "MS fotbal 2026"},
	{1206995494455181314, "LM 2026/27"},
}

type PaymentRow struct {
	UserID   int64
	Username string
	Tipped   map[int64]bool // competition_id → tipoval?
	Paid     bool
}

// GET /admin/payments
func AdminPaymentOverview(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		ctx := context.Background()

		// Sestav seznam comp IDs pro dotaz
		compIDs := make([]int64, len(paymentCompetitions))
		for i, c := range paymentCompetitions {
			compIDs[i] = c.ID
		}

		// Kdo tipoval v těchto soutěžích
		rows, err := db.Pool.Query(ctx, `
			SELECT DISTINCT u.id, u.username, m.competition_id
			FROM users u
			JOIN tips t ON t.user_id = u.id
			JOIN matches m ON m.id = t.match_id
			WHERE m.competition_id = ANY($1)
			  AND COALESCE(u.is_inactive, false) = false
			ORDER BY u.username, m.competition_id
		`, compIDs)
		if err != nil {
			http.Error(w, "DB error: "+err.Error(), 500)
			return
		}

		userMap := map[int64]*PaymentRow{}
		var userOrder []int64
		for rows.Next() {
			var uid, compID int64
			var uname string
			if err := rows.Scan(&uid, &uname, &compID); err != nil {
				continue
			}
			if _, ok := userMap[uid]; !ok {
				userMap[uid] = &PaymentRow{
					UserID:   uid,
					Username: uname,
					Tipped:   map[int64]bool{},
				}
				userOrder = append(userOrder, uid)
			}
			userMap[uid].Tipped[compID] = true
		}
		rows.Close()

		// Načti stav zaplaceno
		pRows, _ := db.Pool.Query(ctx,
			`SELECT user_id, paid FROM competition_payments WHERE paid = true`)
		if pRows != nil {
			for pRows.Next() {
				var uid int64
				var paid bool
				_ = pRows.Scan(&uid, &paid)
				if pr, ok := userMap[uid]; ok {
					pr.Paid = paid
				}
			}
			pRows.Close()
		}

		result := make([]*PaymentRow, 0, len(userOrder))
		for _, uid := range userOrder {
			result = append(result, userMap[uid])
		}

		RenderTemplate(w, r, tmpl, "admin/payment_overview.html", TemplateData{
			"User":         admin,
			"Rows":         result,
			"Competitions": paymentCompetitions,
		})
	}
}

// POST /admin/payments/{user_id}/toggle-paid  (AJAX)
func AdminPaymentToggle(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "forbidden"})
		return
	}
	uid, err := strconv.ParseInt(r.PathValue("user_id"), 10, 64)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "bad_id"})
		return
	}

	ctx := context.Background()
	// Toggle: pokud existuje a paid=true → false, jinak true
	var cur bool
	_ = db.Pool.QueryRow(ctx,
		`SELECT paid FROM competition_payments WHERE user_id=$1 LIMIT 1`, uid).Scan(&cur)

	newVal := !cur
	_, _ = db.Pool.Exec(ctx, `
		INSERT INTO competition_payments (user_id, competition_id, paid, updated_at)
		VALUES ($1, 0, $2, now())
		ON CONFLICT (user_id, competition_id) DO UPDATE SET paid=$2, updated_at=now()
	`, uid, newVal)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "paid": newVal})
}
