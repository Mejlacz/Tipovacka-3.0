// handlers/admin_user_merge.go — Tipovačka 2.0
// Sloučení dvou uživatelů: přenos tipů, odpovědí, nastavení, smazání zdrojového.
package handlers

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"

	"tipovacka/db"
	"tipovacka/middleware"
	"tipovacka/models"
)

// GET /admin/users/merge
func AdminUserMergeForm(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		ctx := context.Background()

		rows, _ := db.Pool.Query(ctx, `SELECT id, username FROM users ORDER BY username`)
		var users []*models.User
		for rows.Next() {
			u := &models.User{}
			_ = rows.Scan(&u.ID, &u.Username)
			users = append(users, u)
		}
		rows.Close()

		flash := middleware.GetFlash(w, r)
		RenderTemplate(w, r, tmpl, "admin/user_merge.html", TemplateData{
			"User":  admin,
			"Users": users,
			"Flash": flash,
		})
	}
}

// POST /admin/users/merge
func AdminUserMerge(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	sourceID, err1 := strconv.Atoi(r.FormValue("source_user_id"))
	targetID, err2 := strconv.Atoi(r.FormValue("target_user_id"))
	if err1 != nil || err2 != nil || sourceID == 0 || targetID == 0 {
		middleware.SetFlash(w, r, "error", "Vyber zdrojového a cílového uživatele.")
		http.Redirect(w, r, "/admin/users/merge", http.StatusSeeOther)
		return
	}
	if sourceID == targetID {
		middleware.SetFlash(w, r, "error", "Zdrojový a cílový uživatel jsou stejní.")
		http.Redirect(w, r, "/admin/users/merge", http.StatusSeeOther)
		return
	}

	ctx := context.Background()

	// Získej jméno zdrojového uživatele pro log
	var sourceUsername, targetUsername string
	_ = db.Pool.QueryRow(ctx, `SELECT username FROM users WHERE id=$1`, sourceID).Scan(&sourceUsername)
	_ = db.Pool.QueryRow(ctx, `SELECT username FROM users WHERE id=$1`, targetID).Scan(&targetUsername)

	if sourceUsername == "" {
		middleware.SetFlash(w, r, "error", "Zdrojový uživatel nenalezen.")
		http.Redirect(w, r, "/admin/users/merge", http.StatusSeeOther)
		return
	}

	// Spusť transakci
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		log.Printf("[user_merge] begin tx error: %v", err)
		middleware.SetFlash(w, r, "error", "Databázová chyba.")
		http.Redirect(w, r, "/admin/users/merge", http.StatusSeeOther)
		return
	}
	defer tx.Rollback(ctx)

	steps := []struct {
		sql  string
		args []interface{}
	}{
		// 1. Přesuň tipy
		{`UPDATE tips SET user_id=$1 WHERE user_id=$2`, []interface{}{targetID, sourceID}},
		// 2. Přesuň extra odpovědi
		{`UPDATE extra_answers SET user_id=$1 WHERE user_id=$2`, []interface{}{targetID, sourceID}},
		// 3. Smaž nastavení notifikací cílového (aby nedošlo ke konfliktu)
		{`DELETE FROM notification_settings WHERE user_id=$1`, []interface{}{targetID}},
		// 4. Přesuň nastavení notifikací zdrojového na cílového
		{`UPDATE notification_settings SET user_id=$1 WHERE user_id=$2`, []interface{}{targetID, sourceID}},
		// 5. Smaž push subscriptions cílového
		{`DELETE FROM push_subscriptions WHERE user_id=$1`, []interface{}{targetID}},
		// 6. Přesuň push subscriptions zdrojového na cílového
		{`UPDATE push_subscriptions SET user_id=$1 WHERE user_id=$2`, []interface{}{targetID, sourceID}},
		// 7. Smaž zdrojového uživatele
		{`DELETE FROM users WHERE id=$1`, []interface{}{sourceID}},
	}

	for i, step := range steps {
		if _, err := tx.Exec(ctx, step.sql, step.args...); err != nil {
			log.Printf("[user_merge] step %d error: %v", i+1, err)
			middleware.SetFlash(w, r, "error", fmt.Sprintf("Chyba při kroku %d: %v", i+1, err))
			http.Redirect(w, r, "/admin/users/merge", http.StatusSeeOther)
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		log.Printf("[user_merge] commit error: %v", err)
		middleware.SetFlash(w, r, "error", "Commit selhal.")
		http.Redirect(w, r, "/admin/users/merge", http.StatusSeeOther)
		return
	}

	// Audit log
	adminID := admin.ID
	LogAction(&adminID, admin.Username, "merge_user", "user", &sourceID,
		fmt.Sprintf("Sloučení uživatele '%s' (id=%d) → '%s' (id=%d)", sourceUsername, sourceID, targetUsername, targetID),
		&sourceUsername, &targetUsername)

	middleware.SetFlash(w, r, "ok", fmt.Sprintf(
		"Uživatel '%s' byl sloučen do '%s' a smazán.", sourceUsername, targetUsername))
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}
