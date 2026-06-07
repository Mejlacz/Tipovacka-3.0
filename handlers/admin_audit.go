// handlers/admin_audit.go — Tipovačka 2.0
// Audit log + Historia + Undo posledních akcí.
package handlers

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"tipovacka/db"
	"tipovacka/middleware"
	"tipovacka/models"
)

// UNDOABLE_ACTIONS are action types that support undo.
var UNDOABLE_ACTIONS = map[string]bool{
	"match_score": true,
	"user_create": true,
	"user_role":   true,
	"admin_set_tip": true,
}

// historyCategory vrátí kategorii pro danou akci.
func historyCategory(action string) string {
	switch action {
	case "tip_save", "extra_save", "admin_set_tip", "admin_set_extra_answer":
		return "tipy"
	case "match_score", "match_score_clear", "match_create", "match_add_quick",
		"match_edit", "match_delete", "match_date_change", "auto_fetch_results", "api_update_results":
		return "zapasy"
	case "round_create", "round_edit", "round_delete", "round_toggle":
		return "kola"
	case "user_create", "user_role", "user_delete", "user_approve", "user_block",
		"user_unblock", "user_toggle_admin", "user_toggle_owner", "user_inactive",
		"user_import", "merge_user":
		return "uzivatele"
	case "team_merge", "team_delete":
		return "tymy"
	default:
		return "system"
	}
}

var histCategoryLabel = map[string]string{
	"tipy":      "🎯 Tipy",
	"zapasy":    "⚽ Zápasy",
	"kola":      "📋 Kola",
	"uzivatele": "👤 Uživatelé",
	"tymy":      "🛡 Týmy",
	"system":    "⚙️ Systém",
}

var histActionIcon = map[string]string{
	"tip_save":               "🎯",
	"extra_save":             "💬",
	"admin_set_tip":          "🛠️",
	"admin_set_extra_answer": "🛠️",
	"match_score":            "⚽",
	"match_score_clear":      "🧹",
	"match_create":           "➕",
	"match_add_quick":        "➕",
	"match_edit":             "✏️",
	"match_delete":           "🗑️",
	"match_date_change":      "🗓️",
	"auto_fetch_results":     "🤖",
	"api_update_results":     "🔄",
	"round_create":           "📋",
	"round_edit":             "✏️",
	"round_delete":           "🗑️",
	"round_toggle":           "🔀",
	"user_create":            "👤",
	"user_role":              "🔑",
	"user_delete":            "🗑️",
	"user_approve":           "✅",
	"user_block":             "🚫",
	"user_unblock":           "✅",
	"merge_user":             "🔀",
	"user_import":            "📥",
	"team_merge":             "🔀",
	"team_delete":            "🗑️",
	"undo":                   "↩️",
	"ocr_import":             "🤖",
	"tip_import":             "📥",
	"backup":                 "💾",
	"extra_import":           "📥",
	"tz_override_set":        "🕐",
	"tz_override_clear":      "🕐",
	"scheduler":              "⏰",
}

// histCatActions mapuje kategorii → slice akcí (sdíleno mezi History a AuditLog).
var histCatActions = map[string][]string{
	"tipy":      {"tip_save", "extra_save", "admin_set_tip", "admin_set_extra_answer"},
	"zapasy":    {"match_score", "match_score_clear", "match_create", "match_add_quick", "match_edit", "match_delete", "match_date_change", "auto_fetch_results", "api_update_results"},
	"kola":      {"round_create", "round_edit", "round_delete", "round_toggle"},
	"uzivatele": {"user_create", "user_role", "user_delete", "user_approve", "user_block", "user_unblock", "user_toggle_admin", "user_toggle_owner", "user_inactive", "user_import", "merge_user"},
	"tymy":      {"team_merge", "team_delete"},
}

// GET /admin/history
func AdminHistory(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		if !admin.IsOwner {
			http.Error(w, "403 Forbidden — jen pro Owner", http.StatusForbidden)
			return
		}
		ctx := context.Background()

		const perPage = 100
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		cat := r.URL.Query().Get("cat") // "", "tipy", "zapasy", "kola", "uzivatele", "tymy", "system"

		// ── Počty per kategorie (z audit_log) ──────────────────────────────
		type CatCount struct {
			Cat   string
			Label string
			Count int
		}
		catRows, _ := db.Pool.Query(ctx, `SELECT action, COUNT(*) FROM audit_log GROUP BY action`)
		rawCounts := map[string]int{}
		for catRows.Next() {
			var act string
			var cnt int
			_ = catRows.Scan(&act, &cnt)
			rawCounts[act] += cnt
		}
		catRows.Close()
		catTotals := map[string]int{}
		totalAll := 0
		for act, cnt := range rawCounts {
			c := historyCategory(act)
			catTotals[c] += cnt
			totalAll += cnt
		}
		catOrder := []string{"tipy", "zapasy", "kola", "uzivatele", "tymy", "system"}
		var catCounts []CatCount
		for _, c := range catOrder {
			catCounts = append(catCounts, CatCount{Cat: c, Label: histCategoryLabel[c], Count: catTotals[c]})
		}

		// ── Filtr pro SQL ──────────────────────────────────────────────────
		var whereSQL string
		var queryArgs []interface{}
		if cat != "" {
			if actions, ok := histCatActions[cat]; ok {
				whereSQL = ` WHERE action = ANY($1)`
				queryArgs = append(queryArgs, actions)
			} else if cat == "system" {
				// system = vše co není v ostatních kategoriích
				var allKnown []string
				for _, acts := range histCatActions {
					allKnown = append(allKnown, acts...)
				}
				whereSQL = ` WHERE action != ALL($1)`
				queryArgs = append(queryArgs, allKnown)
			}
		}

		// ── Celkový počet pro stránkování ──────────────────────────────────
		countQuery := `SELECT COUNT(*) FROM audit_log` + whereSQL
		var totalCount int
		if len(queryArgs) > 0 {
			_ = db.Pool.QueryRow(ctx, countQuery, queryArgs...).Scan(&totalCount)
		} else {
			_ = db.Pool.QueryRow(ctx, countQuery).Scan(&totalCount)
		}
		totalPages := (totalCount + perPage - 1) / perPage
		if totalPages < 1 {
			totalPages = 1
		}
		offset := (page - 1) * perPage

		// ── Záznamy ────────────────────────────────────────────────────────
		type HistEntry struct {
			ID          int
			TS          *time.Time
			User        string
			Action      string
			Icon        string
			Category    string
			CatLabel    string
			Description string
			OldValue    *string
			NewValue    *string
			Undone      bool
		}

		mainQuery := `SELECT id, timestamp, admin_username, action, description, old_value, new_value, undone
		               FROM audit_log` + whereSQL + ` ORDER BY id DESC LIMIT $` +
			strconv.Itoa(len(queryArgs)+1) + ` OFFSET $` + strconv.Itoa(len(queryArgs)+2)
		queryArgs = append(queryArgs, perPage, offset)

		rows, _ := db.Pool.Query(ctx, mainQuery, queryArgs...)
		var entries []HistEntry
		for rows.Next() {
			var e HistEntry
			_ = rows.Scan(&e.ID, &e.TS, &e.User, &e.Action, &e.Description, &e.OldValue, &e.NewValue, &e.Undone)
			if e.User == "" {
				e.User = "system"
			}
			e.Icon = histActionIcon[e.Action]
			if e.Icon == "" {
				e.Icon = "🔧"
			}
			e.Category = historyCategory(e.Action)
			e.CatLabel = histCategoryLabel[e.Category]
			if e.TS != nil {
				prg := e.TS.In(pragueLocation)
				e.TS = &prg
			}
			entries = append(entries, e)
		}
		rows.Close()

		RenderTemplate(w, r, tmpl, "history.html", TemplateData{
			"User":        admin,
			"Entries":     entries,
			"CatCounts":   catCounts,
			"TotalAll":    totalAll,
			"SelectedCat": cat,
			"Page":        page,
			"TotalPages":  totalPages,
			"TotalCount":  totalCount,
		})
	}
}

// GET /admin/audit
func AdminAuditLog(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		ctx := context.Background()

		const perPage = 100
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		cat := r.URL.Query().Get("cat")

		// ── Počty per kategorie ────────────────────────────────────────────
		type CatCount struct {
			Cat   string
			Label string
			Count int
		}
		catRows, _ := db.Pool.Query(ctx, `SELECT action, COUNT(*) FROM audit_log GROUP BY action`)
		rawCounts := map[string]int{}
		for catRows.Next() {
			var act string
			var cnt int
			_ = catRows.Scan(&act, &cnt)
			rawCounts[act] += cnt
		}
		catRows.Close()
		catTotals := map[string]int{}
		totalAll := 0
		for act, cnt := range rawCounts {
			c := historyCategory(act)
			catTotals[c] += cnt
			totalAll += cnt
		}
		catOrder := []string{"tipy", "zapasy", "kola", "uzivatele", "tymy", "system"}
		var catCounts []CatCount
		for _, c := range catOrder {
			catCounts = append(catCounts, CatCount{Cat: c, Label: histCategoryLabel[c], Count: catTotals[c]})
		}

		// ── WHERE clause: role + kategorie ────────────────────────────────
		var conditions []string
		var queryArgs []interface{}

		// Non-Owner admins nevidí přesné tipy uživatelů
		if !admin.IsOwner {
			conditions = append(conditions, `action NOT IN ('tip_save','extra_save')`)
		}

		if cat != "" {
			if actions, ok := histCatActions[cat]; ok {
				queryArgs = append(queryArgs, actions)
				conditions = append(conditions, `action = ANY($`+strconv.Itoa(len(queryArgs))+`)`)
			} else if cat == "system" {
				var allKnown []string
				for _, acts := range histCatActions {
					allKnown = append(allKnown, acts...)
				}
				queryArgs = append(queryArgs, allKnown)
				conditions = append(conditions, `action != ALL($`+strconv.Itoa(len(queryArgs))+`)`)
			}
		}

		whereClause := ""
		for i, c := range conditions {
			if i == 0 {
				whereClause = " WHERE " + c
			} else {
				whereClause += " AND " + c
			}
		}

		// Celkový počet pro stránkování
		var totalCount int
		countArgs := make([]interface{}, len(queryArgs))
		copy(countArgs, queryArgs)
		_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_log`+whereClause, countArgs...).Scan(&totalCount)
		totalPages := (totalCount + perPage - 1) / perPage
		if totalPages < 1 {
			totalPages = 1
		}
		offset := (page - 1) * perPage

		// Hlavní dotaz
		mainArgs := make([]interface{}, len(queryArgs))
		copy(mainArgs, queryArgs)
		mainArgs = append(mainArgs, perPage, offset)
		auditQuery := `SELECT id, timestamp, admin_id, admin_username, action, entity_type, entity_id,
			        description, old_value, new_value, undone
			   FROM audit_log` + whereClause +
			` ORDER BY id DESC LIMIT $` + strconv.Itoa(len(mainArgs)-1) +
			` OFFSET $` + strconv.Itoa(len(mainArgs))

		rows, _ := db.Pool.Query(ctx, auditQuery, mainArgs...)
		var entries []*models.AuditLog
		for rows.Next() {
			e := &models.AuditLog{}
			_ = rows.Scan(&e.ID, &e.Timestamp, &e.AdminID, &e.AdminUsername, &e.Action,
				&e.EntityType, &e.EntityID, &e.Description, &e.OldValue, &e.NewValue, &e.Undone)
			entries = append(entries, e)
		}
		rows.Close()

		// Undoable: last 3 not-yet-undone undoable actions
		undoRows, _ := db.Pool.Query(ctx,
			`SELECT id FROM audit_log
			  WHERE undone = FALSE AND action = ANY($1)
			  ORDER BY id DESC LIMIT 3`,
			[]string{"match_score", "user_create", "user_role", "admin_set_tip"})
		undoableIDs := map[int]bool{}
		for undoRows.Next() {
			var id int
			_ = undoRows.Scan(&id)
			undoableIDs[id] = true
		}
		undoRows.Close()

		flash := middleware.GetFlash(w, r)

		RenderTemplate(w, r, tmpl, "audit_log.html", TemplateData{
			"User":            admin,
			"Entries":         entries,
			"UndoableIDs":     undoableIDs,
			"UndoableActions": UNDOABLE_ACTIONS,
			"Flash":           flash,
			"Page":            page,
			"TotalPages":      totalPages,
			"TotalCount":      totalCount,
			"TotalAll":        totalAll,
			"CatCounts":       catCounts,
			"SelectedCat":     cat,
		})
	}
}

// POST /admin/audit/{entry_id}/undo
func AdminAuditUndo(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	if !admin.IsOwner {
		http.Error(w, "403 Forbidden — jen pro Owner", http.StatusForbidden)
		return
	}
	entryID, _ := strconv.Atoi(r.PathValue("entry_id"))
	ctx := context.Background()

	// Load the entry
	e := &models.AuditLog{}
	err := db.Pool.QueryRow(ctx,
		`SELECT id, action, entity_id, description, old_value, new_value, undone
		   FROM audit_log WHERE id=$1`, entryID).
		Scan(&e.ID, &e.Action, &e.EntityID, &e.Description, &e.OldValue, &e.NewValue, &e.Undone)
	if err != nil || e.Undone || !UNDOABLE_ACTIONS[e.Action] {
		middleware.SetFlash(w, r, "warn", "Tuto akci nelze vrátit.")
		http.Redirect(w, r, "/admin/audit", http.StatusSeeOther)
		return
	}

	// Verify it's within last 3 undoable
	undoRows, _ := db.Pool.Query(ctx,
		`SELECT id FROM audit_log
		  WHERE undone = FALSE AND action = ANY($1)
		  ORDER BY id DESC LIMIT 3`,
		[]string{"match_score", "user_create", "user_role", "admin_set_tip"})
	undoableIDs := map[int]bool{}
	for undoRows.Next() {
		var id int
		_ = undoRows.Scan(&id)
		undoableIDs[id] = true
	}
	undoRows.Close()

	if !undoableIDs[entryID] {
		middleware.SetFlash(w, r, "warn", "Lze vrátit jen posledních 3 akce.")
		http.Redirect(w, r, "/admin/audit", http.StatusSeeOther)
		return
	}

	var oldMap map[string]interface{}
	if e.OldValue != nil {
		_ = json.Unmarshal([]byte(*e.OldValue), &oldMap)
	}

	successMsg := "Akce '" + e.Description + "' vrácena."
	entityID := 0
	if e.EntityID != nil {
		entityID = *e.EntityID
	}

	switch e.Action {
	case "match_score":
		if entityID == 0 {
			middleware.SetFlash(w, r, "warn", "Zápas neexistuje.")
			http.Redirect(w, r, "/admin/audit", http.StatusSeeOther)
			return
		}
		var oldHomeScore, oldAwayScore *int
		var oldIsFinished bool
		if v, ok := oldMap["home_score"]; ok && v != nil {
			n := int(v.(float64))
			oldHomeScore = &n
		}
		if v, ok := oldMap["away_score"]; ok && v != nil {
			n := int(v.(float64))
			oldAwayScore = &n
		}
		if v, ok := oldMap["is_finished"]; ok {
			oldIsFinished, _ = v.(bool)
		}
		_, _ = db.Pool.Exec(ctx,
			`UPDATE matches SET home_score=$1, away_score=$2, is_finished=$3 WHERE id=$4`,
			oldHomeScore, oldAwayScore, oldIsFinished, entityID)
		// Recalculate tips
		if oldHomeScore != nil && oldAwayScore != nil {
			RecalculateTips(ctx, entityID, *oldHomeScore, *oldAwayScore)
		} else {
			_, _ = db.Pool.Exec(ctx, `UPDATE tips SET points=NULL WHERE match_id=$1`, entityID)
		}
		eid := entityID
		LogAction(&admin.ID, admin.Username, "undo", "match", &eid,
			"Undo: "+e.Description, nil, nil)

	case "user_create":
		if entityID == 0 {
			middleware.SetFlash(w, r, "warn", "Uživatel neexistuje.")
			http.Redirect(w, r, "/admin/audit", http.StatusSeeOther)
			return
		}
		var tipCount int
		_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM tips WHERE user_id=$1`, entityID).Scan(&tipCount)
		if tipCount > 0 {
			middleware.SetFlash(w, r, "warn", "Uživatel má tipy — nelze smazat.")
			http.Redirect(w, r, "/admin/audit", http.StatusSeeOther)
			return
		}
		var uname string
		_ = db.Pool.QueryRow(ctx, `SELECT username FROM users WHERE id=$1`, entityID).Scan(&uname)
		eid2 := entityID
		LogAction(&admin.ID, admin.Username, "undo", "user", &eid2,
			"Undo: smazán uživatel "+uname, nil, nil)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, entityID)

	case "admin_set_tip":
		if entityID == 0 {
			middleware.SetFlash(w, r, "warn", "Tip neexistuje.")
			http.Redirect(w, r, "/admin/audit", http.StatusSeeOther)
			return
		}
		wasNew := false
		if v, ok := oldMap["was_new"]; ok {
			wasNew, _ = v.(bool)
		}
		if wasNew {
			_, _ = db.Pool.Exec(ctx, `DELETE FROM tips WHERE id=$1`, entityID)
			successMsg = "Tip smazán (vráceno): " + e.Description
		} else {
			var oldHome, oldAway *int
			var oldPts *int
			if v, ok := oldMap["home_score"]; ok && v != nil {
				n := int(v.(float64))
				oldHome = &n
			}
			if v, ok := oldMap["away_score"]; ok && v != nil {
				n := int(v.(float64))
				oldAway = &n
			}
			if v, ok := oldMap["points"]; ok && v != nil {
				n := int(v.(float64))
				oldPts = &n
			}
			_, _ = db.Pool.Exec(ctx,
				`UPDATE tips SET home_score=$1, away_score=$2, points=$3 WHERE id=$4`,
				oldHome, oldAway, oldPts, entityID)
			successMsg = "Tip obnoven: " + e.Description
		}
		eid3 := entityID
		LogAction(&admin.ID, admin.Username, "undo", "tip", &eid3,
			"Undo: "+e.Description, nil, nil)

	case "user_role":
		if entityID == 0 {
			middleware.SetFlash(w, r, "warn", "Uživatel neexistuje.")
			http.Redirect(w, r, "/admin/audit", http.StatusSeeOther)
			return
		}
		oldRole := "user"
		if v, ok := oldMap["role"]; ok {
			oldRole, _ = v.(string)
		}
		isOwner := oldRole == "owner"
		isAdmin := oldRole == "admin" || oldRole == "owner"
		// Aktualizuj jen existující sloupce
		switch {
		case userCols.IsOwner && userCols.IsAdmin:
			_, _ = db.Pool.Exec(ctx,
				`UPDATE users SET is_owner=$1, is_admin=$2 WHERE id=$3`, isOwner, isAdmin, entityID)
		case userCols.IsAdmin:
			_, _ = db.Pool.Exec(ctx, `UPDATE users SET is_admin=$1 WHERE id=$2`, isAdmin, entityID)
		case userCols.IsOwner:
			_, _ = db.Pool.Exec(ctx, `UPDATE users SET is_owner=$1 WHERE id=$2`, isOwner, entityID)
		}
		var uname string
		_ = db.Pool.QueryRow(ctx, `SELECT username FROM users WHERE id=$1`, entityID).Scan(&uname)
		eid4 := entityID
		LogAction(&admin.ID, admin.Username, "undo", "user", &eid4,
			"Undo: role "+uname+" → "+oldRole, nil, nil)
	}

	// Mark entry as undone
	_, _ = db.Pool.Exec(ctx, `UPDATE audit_log SET undone=TRUE WHERE id=$1`, entryID)

	middleware.SetFlash(w, r, "ok", successMsg)
	http.Redirect(w, r, "/admin/audit", http.StatusSeeOther)
}
