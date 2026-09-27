package handlers

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"

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
	IsHidden bool
}

type CompPaymentSection struct {
	ID     int64
	Name   string
	Unpaid []PaymentUser
	Paid   []PaymentUser
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

		rows, err := db.Pool.Query(ctx, `
			SELECT DISTINCT u.id, u.username, COALESCE(u.is_hidden, false), m.competition_id,
			       COALESCE(cp.paid, false)
			FROM users u
			JOIN tips t ON t.user_id = u.id
			JOIN matches m ON m.id = t.match_id
			LEFT JOIN competition_payments cp
			       ON cp.user_id = u.id AND cp.competition_id = m.competition_id
			WHERE m.competition_id = ANY($1)
			  AND COALESCE(u.is_inactive, false) = false
			ORDER BY u.username, m.competition_id
		`, compIDs)
		if err != nil {
			http.Error(w, "DB error: "+err.Error(), 500)
			return
		}

		// comp_id → { paid: [], unpaid: [] }
		type entry struct{ paid bool; u PaymentUser }
		byComp := map[int64][]entry{}
		for rows.Next() {
			var uid, compID int64
			var uname string
			var isHidden, paid bool
			if err := rows.Scan(&uid, &uname, &isHidden, &compID, &paid); err != nil {
				continue
			}
			// Invisible uživatelé viditelní jen pro vlastníka
			if isHidden && !admin.IsOwner {
				continue
			}
			byComp[compID] = append(byComp[compID], entry{
				paid: paid,
				u:    PaymentUser{UserID: uid, Username: uname, IsHidden: isHidden},
			})
		}
		rows.Close()

		sections := make([]CompPaymentSection, 0, len(paymentCompetitions))
		for _, c := range paymentCompetitions {
			sec := CompPaymentSection{ID: c.ID, Name: c.Name}
			for _, e := range byComp[c.ID] {
				if e.paid {
					sec.Paid = append(sec.Paid, e.u)
				} else {
					sec.Unpaid = append(sec.Unpaid, e.u)
				}
			}
			sections = append(sections, sec)
		}

		RenderTemplate(w, r, tmpl, "admin/payment_overview.html", TemplateData{
			"User":     admin,
			"Sections": sections,
		})
	}
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
