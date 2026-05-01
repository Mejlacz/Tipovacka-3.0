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

		// Load last 300 audit log entries
		auditRows, _ := db.Pool.Query(ctx,
			`SELECT id, timestamp, admin_id, admin_username, action, entity_type, entity_id,
			        description, old_value, new_value, undone
			   FROM audit_log ORDER BY id DESC LIMIT 300`)
		var auditEntries []*models.AuditLog
		for auditRows.Next() {
			e := &models.AuditLog{}
			_ = auditRows.Scan(&e.ID, &e.Timestamp, &e.AdminID, &e.AdminUsername, &e.Action,
				&e.EntityType, &e.EntityID, &e.Description, &e.OldValue, &e.NewValue, &e.Undone)
			auditEntries = append(auditEntries, e)
		}
		auditRows.Close()

		// Collect tip IDs that already have an audit entry (avoid duplicates)
		loggedTipIDs := map[int]bool{}
		tipIDRows, _ := db.Pool.Query(ctx,
			`SELECT entity_id FROM audit_log WHERE action IN ('tip_save','admin_set_tip') AND entity_id IS NOT NULL`)
		for tipIDRows.Next() {
			var tid int
			_ = tipIDRows.Scan(&tid)
			loggedTipIDs[tid] = true
		}
		tipIDRows.Close()

		// Fallback: old tips from Tip table without audit record
		type Event struct {
			TS   *time.Time
			Kind string // "tip" or "admin"
			User string
			Icon string
			Text string
			Sub  string
			Pts  *int
		}

		var events []Event

		tipRows, _ := db.Pool.Query(ctx,
			`SELECT t.id, t.created_at, t.points,
			        u.username,
			        hm.name AS home_name, am.name AS away_name,
			        ro.name AS round_name
			   FROM tips t
			   JOIN users u ON u.id = t.user_id
			   JOIN matches m ON m.id = t.match_id
			   JOIN teams hm ON hm.id = m.home_team_id
			   JOIN teams am ON am.id = m.away_team_id
			   JOIN rounds ro ON ro.id = m.round_id
			   ORDER BY t.created_at DESC LIMIT 200`)
		for tipRows.Next() {
			var tid int
			var ts *time.Time
			var pts *int
			var uname, home, away, roundName string
			_ = tipRows.Scan(&tid, &ts, &pts, &uname, &home, &away, &roundName)
			if loggedTipIDs[tid] {
				continue
			}
			events = append(events, Event{
				TS:   ts,
				Kind: "tip",
				User: uname,
				Icon: "🎯",
				Text: home + " vs " + away,
				Sub:  roundName,
				Pts:  pts,
			})
		}
		tipRows.Close()

		// Audit entries
		userActions := map[string]bool{"tip_save": true, "extra_save": true}
		actionIcons := map[string]string{
			"match_score":          "⚽",
			"round_create":         "📋",
			"round_edit":           "✏️",
			"round_delete":         "🗑️",
			"match_create":         "➕",
			"match_delete":         "🗑️",
			"match_edit":           "✏️",
			"user_create":          "👤",
			"user_role":            "🔑",
			"user_delete":          "🗑️",
			"undo":                 "↩️",
			"ocr_import":           "🤖",
			"backup":               "💾",
			"tip_import":           "📥",
			"admin_set_tip":        "🛠️",
			"admin_set_extra_answer": "🛠️",
			"tip_save":             "🎯",
			"extra_save":           "💬",
		}

		for _, e := range auditEntries {
			icon := actionIcons[e.Action]
			if icon == "" {
				icon = "🔧"
			}
			kind := "admin"
			if userActions[e.Action] {
				kind = "tip"
			}
			uname := e.AdminUsername
			if uname == "" {
				uname = "system"
			}
			ts := e.Timestamp
			events = append(events, Event{
				TS:   ts,
				Kind: kind,
				User: uname,
				Icon: icon,
				Text: e.Description,
				Sub:  e.Action,
				Pts:  nil,
			})
		}

		// Sort events by timestamp descending
		for i := 0; i < len(events)-1; i++ {
			for j := i + 1; j < len(events); j++ {
				ti := events[i].TS
				tj := events[j].TS
				var a, b time.Time
				if ti != nil {
					a = *ti
				}
				if tj != nil {
					b = *tj
				}
				if b.After(a) {
					events[i], events[j] = events[j], events[i]
				}
			}
		}
		if len(events) > 200 {
			events = events[:200]
		}

		// Convert timestamps to Prague time
		for i := range events {
			if events[i].TS != nil {
				prg := events[i].TS.In(pragueLocation)
				events[i].TS = &prg
			}
		}

		RenderTemplate(w, r, tmpl, "history.html", TemplateData{
			"User":   admin,
			"Events": events,
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
		offset := (page - 1) * perPage

		// Non-Owner admins nevidí přesné tipy uživatelů
		whereClause := ""
		if !admin.IsOwner {
			whereClause = ` WHERE action NOT IN ('tip_save','extra_save')`
		}

		// Celkový počet záznamů pro stránkování
		var totalCount int
		_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_log`+whereClause).Scan(&totalCount)
		totalPages := (totalCount + perPage - 1) / perPage
		if totalPages < 1 {
			totalPages = 1
		}

		auditQuery := `SELECT id, timestamp, admin_id, admin_username, action, entity_type, entity_id,
			        description, old_value, new_value, undone
			   FROM audit_log` + whereClause +
			` ORDER BY id DESC LIMIT $1 OFFSET $2`

		rows, _ := db.Pool.Query(ctx, auditQuery, perPage, offset)
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
