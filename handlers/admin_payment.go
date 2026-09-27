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

type CompPaymentCard struct {
	ID     int64
	Name   string
	Paid   int
	Total  int
	Pct    int // 0-100
}

type CompPaymentDetail struct {
	ID     int64
	Name   string
	Unpaid []PaymentUser
	Paid   []PaymentUser
}

// GET /admin/payments  — přehled soutěží s počty
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

		// Počet hráčů v každé soutěži (unikátní tipující)
		type countRow struct{ compID int64; total int }
		totalRows, err := db.Pool.Query(ctx, `
			SELECT m.competition_id, COUNT(DISTINCT u.id)
			FROM users u
			JOIN tips t ON t.user_id = u.id
			JOIN matches m ON m.id = t.match_id
			WHERE m.competition_id = ANY($1)
			  AND COALESCE(u.is_inactive, false) = false
			  AND (NOT $2 OR COALESCE(u.is_hidden, false) = false)
			GROUP BY m.competition_id
		`, compIDs, !admin.IsOwner)
		if err != nil {
			http.Error(w, "DB error: "+err.Error(), 500)
			return
		}
		totalMap := map[int64]int{}
		for totalRows.Next() {
			var cid int64; var n int
			_ = totalRows.Scan(&cid, &n)
			totalMap[cid] = n
		}
		totalRows.Close()

		// Počet zaplacených
		paidRows, _ := db.Pool.Query(ctx,
			`SELECT competition_id, COUNT(*) FROM competition_payments
			 WHERE competition_id = ANY($1) AND paid = true
			 GROUP BY competition_id`, compIDs)
		paidMap := map[int64]int{}
		if paidRows != nil {
			for paidRows.Next() {
				var cid int64; var n int
				_ = paidRows.Scan(&cid, &n)
				paidMap[cid] = n
			}
			paidRows.Close()
		}

		cards := make([]CompPaymentCard, 0, len(paymentCompetitions))
		for _, c := range paymentCompetitions {
			total := totalMap[c.ID]
			paid  := paidMap[c.ID]
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

// GET /admin/payments/{comp_id}  — detail jedné soutěže
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
			SELECT DISTINCT u.id, u.username, COALESCE(u.is_hidden, false),
			       COALESCE(cp.paid, false)
			FROM users u
			JOIN tips t ON t.user_id = u.id
			JOIN matches m ON m.id = t.match_id
			LEFT JOIN competition_payments cp
			       ON cp.user_id = u.id AND cp.competition_id = $1
			WHERE m.competition_id = $1
			  AND COALESCE(u.is_inactive, false) = false
			ORDER BY u.username
		`, compID)
		if err != nil {
			http.Error(w, "DB error: "+err.Error(), 500)
			return
		}

		detail := CompPaymentDetail{ID: compID, Name: compName}
		for rows.Next() {
			var uid int64
			var uname string
			var isHidden, paid bool
			if err := rows.Scan(&uid, &uname, &isHidden, &paid); err != nil {
				continue
			}
			if isHidden && !admin.IsOwner {
				continue
			}
			u := PaymentUser{UserID: uid, Username: uname, IsHidden: isHidden}
			if paid {
				detail.Paid = append(detail.Paid, u)
			} else {
				detail.Unpaid = append(detail.Unpaid, u)
			}
		}
		rows.Close()

		RenderTemplate(w, r, tmpl, "admin/payment_detail.html", TemplateData{
			"User":   admin,
			"Detail": detail,
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
