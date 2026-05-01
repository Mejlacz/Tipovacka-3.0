// handlers/admin.go — Tipovačka 2.0
// Admin dashboard + správa soutěží a uživatelů.
package handlers

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"html/template"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"tipovacka/db"
	"tipovacka/middleware"
	"tipovacka/models"
)

var sportLabel = map[string]string{
	"football":   "⚽ Fotbal",
	"hockey":     "🏒 Hokej",
	"tennis":     "🎾 Tenis",
	"basketball": "🏀 Basketbal",
}

func getSportLabel(sport string) string {
	if l, ok := sportLabel[sport]; ok {
		return l
	}
	return strings.Title(sport)
}

// ─── GET /admin ───────────────────────────────────────────────────────────────

func AdminDashboard(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}

		ctx := context.Background()
		compRows, _ := db.Pool.Query(ctx,
			`SELECT id, name, season, is_active, sport, sort_order FROM competitions
			  ORDER BY is_active DESC, id DESC`)
		var comps []*models.Competition
		for compRows.Next() {
			c := &models.Competition{}
			_ = compRows.Scan(&c.ID, &c.Name, &c.Season, &c.IsActive, &c.Sport, &c.SortOrder)
			comps = append(comps, c)
		}
		compRows.Close()

		type compData struct {
			Comp        *models.Competition
			SportLabel  string
			LastRound   *models.Round
			RoundsCount int
		}
		var compDataList []compData
		for _, comp := range comps {
			var lastRound *models.Round
			rnd := &models.Round{}
			err := db.Pool.QueryRow(ctx,
				`SELECT id, competition_id, name, deadline, is_active FROM rounds
				  WHERE competition_id = $1 ORDER BY id DESC LIMIT 1`, comp.ID).
				Scan(&rnd.ID, &rnd.CompetitionID, &rnd.Name, &rnd.Deadline, &rnd.IsActive)
			if err == nil {
				lastRound = rnd
			}
			var count int
			_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM rounds WHERE competition_id = $1`, comp.ID).Scan(&count)
			compDataList = append(compDataList, compData{
				Comp:        comp,
				SportLabel:  getSportLabel(comp.Sport),
				LastRound:   lastRound,
				RoundsCount: count,
			})
		}

		flash := middleware.GetFlash(w, r)
		RenderTemplate(w, r, tmpl, "dashboard.html", TemplateData{
			"User":              admin,
			"CompData":          compDataList,
			"Flash":             flash,
			"ShowDeadlineAlert": UserCanSeeDeadline(admin.ID, admin.IsOwner),
		})
	}
}

// ─── GET /admin/competitions ──────────────────────────────────────────────────

func AdminCompetitionsList(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}

		ctx := context.Background()
		rows, _ := db.Pool.Query(ctx,
			`SELECT c.id, c.name, c.season, c.is_active, c.sport, c.sort_order,
			        COUNT(DISTINCT r.id) AS rounds_count,
			        COUNT(DISTINCT m.id) AS matches_count,
			        COUNT(DISTINCT t.id) AS tips_count,
			        COUNT(DISTINCT ct.team_id) AS teams_count
			   FROM competitions c
			   LEFT JOIN rounds r ON r.competition_id = c.id
			   LEFT JOIN matches m ON m.round_id = r.id
			   LEFT JOIN tips t ON t.match_id = m.id
			   LEFT JOIN competition_teams ct ON ct.competition_id = c.id
			  GROUP BY c.id
			  ORDER BY c.is_active DESC, c.sort_order ASC NULLS LAST, c.id DESC`)

		type cData struct {
			Comp         *models.Competition
			SportLabel   string
			RoundsCount  int
			MatchesCount int
			TipsCount    int
			TeamsCount   int
		}
		var compData []cData
		for rows.Next() {
			c := &models.Competition{}
			var rc, mc, tc, tmc int
			_ = rows.Scan(&c.ID, &c.Name, &c.Season, &c.IsActive, &c.Sport, &c.SortOrder,
				&rc, &mc, &tc, &tmc)
			compData = append(compData, cData{
				Comp: c, SportLabel: getSportLabel(c.Sport),
				RoundsCount: rc, MatchesCount: mc, TipsCount: tc, TeamsCount: tmc,
			})
		}
		rows.Close()

		flash := middleware.GetFlash(w, r)
		RenderTemplate(w, r, tmpl, "competitions_list.html", TemplateData{
			"User":      admin,
			"CompData":  compData,
			"Flash":     flash,
		})
	}
}

// ─── GET /admin/competitions/new ─────────────────────────────────────────────

func AdminCompetitionNewForm(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		RenderTemplate(w, r, tmpl, "competition_form.html", TemplateData{
			"User": admin, "Competition": nil,
		})
	}
}

// ─── POST /admin/competitions/new ────────────────────────────────────────────

func AdminCompetitionCreate(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	season := strings.TrimSpace(r.FormValue("season"))
	sport := strings.TrimSpace(r.FormValue("sport"))
	if sport == "" {
		sport = "football"
	}

	var compID int
	_ = db.Pool.QueryRow(context.Background(),
		`INSERT INTO competitions (name, season, sport, is_active) VALUES ($1, $2, $3, true) RETURNING id`,
		name, season, sport).Scan(&compID)
	http.Redirect(w, r, "/admin/competitions/"+strconv.Itoa(compID)+"/teams", http.StatusSeeOther)
}

// ─── GET /admin/competitions/{id}/edit ───────────────────────────────────────

func AdminCompetitionEditForm(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		compID, _ := strconv.Atoi(r.PathValue("competition_id"))
		ctx := context.Background()
		comp := &models.Competition{}
		err := db.Pool.QueryRow(ctx,
			`SELECT id, name, season, is_active, sport, sort_order, COALESCE(fd_code,'') FROM competitions WHERE id = $1`, compID).
			Scan(&comp.ID, &comp.Name, &comp.Season, &comp.IsActive, &comp.Sport, &comp.SortOrder, &comp.FdCode)
		if err != nil {
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}
		RenderTemplate(w, r, tmpl, "competition_form.html", TemplateData{
			"User": admin, "Competition": comp,
		})
	}
}

// ─── POST /admin/competitions/{id}/edit ──────────────────────────────────────

func AdminCompetitionEditSubmit(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	compID, _ := strconv.Atoi(r.PathValue("competition_id"))
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	name     := strings.TrimSpace(r.FormValue("name"))
	season   := strings.TrimSpace(r.FormValue("season"))
	sport    := r.FormValue("sport")
	fdCode   := strings.ToUpper(strings.TrimSpace(r.FormValue("fd_code")))
	isActive := r.FormValue("is_active") == "on"
	_, _ = db.Pool.Exec(context.Background(),
		`UPDATE competitions SET name=$1, season=$2, sport=$3, fd_code=$4, is_active=$5 WHERE id=$6`,
		name, season, sport, fdCode, isActive, compID)
	middleware.SetFlash(w, r, "ok", "Soutěž byla uložena.")
	http.Redirect(w, r, "/admin/competitions", http.StatusSeeOther)
}

// ─── POST /admin/competitions/{id}/toggle ────────────────────────────────────

func AdminCompetitionToggle(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	compID, _ := strconv.Atoi(r.PathValue("competition_id"))
	ctx := context.Background()
	comp := &models.Competition{}
	err := db.Pool.QueryRow(ctx,
		`SELECT id, name, is_active FROM competitions WHERE id = $1`, compID).
		Scan(&comp.ID, &comp.Name, &comp.IsActive)
	if err == nil {
		newState := !comp.IsActive
		_, _ = db.Pool.Exec(ctx, `UPDATE competitions SET is_active=$1 WHERE id=$2`, newState, compID)
		state := "Archivována"
		if newState {
			state = "Aktivována"
		}
		middleware.SetFlash(w, r, "ok", state+": <b>"+comp.Name+"</b>")
	}
	http.Redirect(w, r, "/admin/competitions", http.StatusSeeOther)
}

// ─── POST /admin/competitions/{id}/delete ────────────────────────────────────

func AdminCompetitionDelete(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	compID, _ := strconv.Atoi(r.PathValue("competition_id"))
	ctx := context.Background()
	comp := &models.Competition{}
	err := db.Pool.QueryRow(ctx,
		`SELECT id, name, is_active FROM competitions WHERE id = $1`, compID).
		Scan(&comp.ID, &comp.Name, &comp.IsActive)
	if err == nil {
		if comp.IsActive {
			middleware.SetFlash(w, r, "error", "Aktivní soutěž nelze smazat — nejdřív ji archivuj.")
			http.Redirect(w, r, "/admin/competitions", http.StatusSeeOther)
			return
		}
		_, _ = db.Pool.Exec(ctx, `DELETE FROM notification_settings WHERE competition_id=$1`, compID)
		_, _ = db.Pool.Exec(ctx, `UPDATE teams SET competition_id=NULL WHERE competition_id=$1`, compID)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM competitions WHERE id=$1`, compID)
		middleware.SetFlash(w, r, "ok", "Soutěž <b>"+comp.Name+"</b> byla smazána.")
	}
	http.Redirect(w, r, "/admin/competitions", http.StatusSeeOther)
}

// ─── GET /admin/users ─────────────────────────────────────────────────────────

func AdminUsersList(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}

		ctx := context.Background()
		allUsers := loadAllUsers(ctx)

		var blocked, pending, active, inactive []*models.User
		for _, u := range allUsers {
			switch {
			case u.IsBlocked:
				blocked = append(blocked, u)
			case !u.IsApproved && !u.IsBlocked && !u.IsInactive:
				pending = append(pending, u)
			case !u.IsInactive && u.IsApproved && !u.IsBlocked:
				active = append(active, u)
			case u.IsInactive && !u.IsBlocked:
				inactive = append(inactive, u)
			}
		}

		sortByName := func(users []*models.User) {
			sort.Slice(users, func(i, j int) bool {
				li := strings.ToLower(lastName(users[i])) + strings.ToLower(firstName(users[i])) + strings.ToLower(users[i].Username)
				lj := strings.ToLower(lastName(users[j])) + strings.ToLower(firstName(users[j])) + strings.ToLower(users[j].Username)
				return li < lj
			})
		}
		sort.Slice(active, func(i, j int) bool {
			ri, rj := roleOrder(active[i]), roleOrder(active[j])
			if ri != rj {
				return ri < rj
			}
			return strings.ToLower(active[i].Username) < strings.ToLower(active[j].Username)
		})
		sortByName(blocked)
		sortByName(pending)
		sortByName(inactive)

		flash := middleware.GetFlash(w, r)
		RenderTemplate(w, r, tmpl, "users.html", TemplateData{
			"User":         admin,
			"ActiveUsers":  active,
			"InactiveUsers": inactive,
			"BlockedUsers": blocked,
			"PendingUsers": pending,
			"Flash":        flash,
		})
	}
}

// ─── POST /admin/users/bulk-action ───────────────────────────────────────────

func AdminUsersBulkAction(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	action := r.FormValue("action")
	userIDs := r.Form["user_ids"]
	if len(userIDs) == 0 {
		middleware.SetFlash(w, r, "err", "Nevybral jsi žádné uživatele.")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	ctx := context.Background()

	switch action {
	case "set-role":
		role := strings.ToLower(r.FormValue("role"))
		if role == "owner" && !admin.IsOwner {
			role = "admin"
		}
		isOwner := role == "owner"
		isAdmin := role == "admin" || role == "owner"
		var count int
		for _, idStr := range userIDs {
			uid, _ := strconv.Atoi(idStr)
			if uid == 0 || uid == admin.ID {
				continue
			}
			switch {
			case userCols.IsOwner && userCols.IsAdmin:
				_, _ = db.Pool.Exec(ctx, `UPDATE users SET is_owner=$1, is_admin=$2 WHERE id=$3`, isOwner, isAdmin, uid)
			case userCols.IsAdmin:
				_, _ = db.Pool.Exec(ctx, `UPDATE users SET is_admin=$1 WHERE id=$2`, isAdmin, uid)
			case userCols.IsOwner:
				_, _ = db.Pool.Exec(ctx, `UPDATE users SET is_owner=$1 WHERE id=$2`, isOwner, uid)
			}
			count++
		}
		middleware.SetFlash(w, r, "ok", strconv.Itoa(count)+" uživatelů — role změněna na <b>"+role+"</b>.")

	case "reset-password":
		customPw := strings.TrimSpace(r.FormValue("new_password"))
		var msgs []string
		for _, idStr := range userIDs {
			uid, _ := strconv.Atoi(idStr)
			if uid == 0 {
				continue
			}
			pw := customPw
			if pw == "" {
				pw = genPassword(10)
			}
			hash, _ := HashPassword(pw)
			_, _ = db.Pool.Exec(ctx, `UPDATE users SET password_hash=$1 WHERE id=$2`, hash, uid)
			var username string
			_ = db.Pool.QueryRow(ctx, `SELECT username FROM users WHERE id=$1`, uid).Scan(&username)
			msgs = append(msgs, "<b>"+username+"</b>: <code>"+pw+"</code>")
		}
		if len(msgs) > 0 {
			middleware.SetFlash(w, r, "info", "Nová hesla: "+strings.Join(msgs, " · "))
		}

	case "send-welcome":
		var sent, failed int
		for _, idStr := range userIDs {
			uid, _ := strconv.Atoi(idStr)
			if uid == 0 {
				continue
			}
			var username, email string
			_ = db.Pool.QueryRow(ctx, `SELECT username, COALESCE(email,'') FROM users WHERE id=$1`, uid).Scan(&username, &email)
			if email == "" {
				failed++
				continue
			}
			subject := "Vítej v Tipovačce!"
			body := "Ahoj " + username + ",\n\nvítej v Tipovačce!\nPřihlásit se můžeš zde: https://tipovacka-3.fly.dev/login\n\nPokud máš dotazy, odpověz na tento email.\n\nHodně štěstí!\nTipovačka"
			if err := sendEmail(email, subject, body); err != nil {
				failed++
			} else {
				sent++
			}
		}
		msg := "Welcome email odeslán: <b>" + strconv.Itoa(sent) + "</b>"
		if failed > 0 {
			msg += ", selhalo: " + strconv.Itoa(failed)
		}
		middleware.SetFlash(w, r, "ok", msg)

	case "approve":
		if !userCols.IsApproved {
			middleware.SetFlash(w, r, "err", "Sloupec is_approved neexistuje v DB.")
			break
		}
		var count int
		for _, idStr := range userIDs {
			uid, _ := strconv.Atoi(idStr)
			if uid == 0 {
				continue
			}
			_, _ = db.Pool.Exec(ctx, `UPDATE users SET is_approved=true WHERE id=$1`, uid)
			var username string
			_ = db.Pool.QueryRow(ctx, `SELECT username FROM users WHERE id=$1`, uid).Scan(&username)
			LogAction(&admin.ID, admin.Username, "user_approve", "user", &uid,
				"Uživatel "+username+" schválen (bulk)", nil, nil)
			count++
		}
		middleware.SetFlash(w, r, "ok", strconv.Itoa(count)+" uživatelů schváleno.")

	case "delete":
		var count int
		for _, idStr := range userIDs {
			uid, _ := strconv.Atoi(idStr)
			if uid == 0 || uid == admin.ID {
				continue
			}
			_, _ = db.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, uid)
			count++
		}
		middleware.SetFlash(w, r, "ok", strconv.Itoa(count)+" uživatelů smazáno.")

	default:
		middleware.SetFlash(w, r, "err", "Neznámá akce.")
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

func lastName(u *models.User) string {
	if u.LastName != nil {
		return *u.LastName
	}
	return ""
}

func firstName(u *models.User) string {
	if u.FirstName != nil {
		return *u.FirstName
	}
	return ""
}

func roleOrder(u *models.User) int {
	if u.IsOwner {
		return 0
	}
	if u.IsAdmin {
		return 1
	}
	return 2
}

// ─── POST /admin/users/{id}/approve ──────────────────────────────────────────

func AdminUserApprove(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	userID, _ := strconv.Atoi(r.PathValue("user_id"))
	ctx := context.Background()
	var username string
	err := db.Pool.QueryRow(ctx, `SELECT username FROM users WHERE id=$1`, userID).Scan(&username)
	if err == nil {
		if userCols.IsApproved {
			_, _ = db.Pool.Exec(ctx, `UPDATE users SET is_approved=true WHERE id=$1`, userID)
		}
		newVal := `{"is_approved":true}`
		LogAction(&admin.ID, admin.Username, "user_approve", "user", &userID, "Uživatel "+username+" schválen", nil, &newVal)
		middleware.SetFlash(w, r, "ok", "Uživatel <b>"+username+"</b> byl schválen.")
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// ─── POST /admin/users/{id}/block ────────────────────────────────────────────

func AdminUserBlock(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	userID, _ := strconv.Atoi(r.PathValue("user_id"))
	if userCols.IsBlocked {
		_, _ = db.Pool.Exec(context.Background(),
			`UPDATE users SET is_blocked=true WHERE id=$1`, userID)
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// ─── POST /admin/users/{id}/unblock ──────────────────────────────────────────

func AdminUserUnblock(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	userID, _ := strconv.Atoi(r.PathValue("user_id"))
	ctx := context.Background()
	var username string
	_ = db.Pool.QueryRow(ctx, `SELECT username FROM users WHERE id=$1`, userID).Scan(&username)
	if userCols.IsBlocked {
		_, _ = db.Pool.Exec(ctx, `UPDATE users SET is_blocked=false WHERE id=$1`, userID)
	}
	middleware.SetFlash(w, r, "ok", "Uživatel <b>"+username+"</b> byl aktivován.")
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// ─── POST /admin/users/{id}/toggle-admin ─────────────────────────────────────

func AdminUserToggleAdmin(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	userID, _ := strconv.Atoi(r.PathValue("user_id"))
	if userID == admin.ID {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	ctx := context.Background()
	if !userCols.IsAdmin {
		// Sloupec is_admin neexistuje — nelze přepínat roli
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	var isAdmin, isOwner bool
	if userCols.IsOwner {
		_ = db.Pool.QueryRow(ctx, `SELECT is_admin, is_owner FROM users WHERE id=$1`, userID).Scan(&isAdmin, &isOwner)
	} else {
		_ = db.Pool.QueryRow(ctx, `SELECT is_admin FROM users WHERE id=$1`, userID).Scan(&isAdmin)
	}
	if isOwner && !admin.IsOwner {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	newAdmin := !isAdmin
	if userCols.IsOwner {
		newOwner := isOwner && newAdmin
		_, _ = db.Pool.Exec(ctx, `UPDATE users SET is_admin=$1, is_owner=$2 WHERE id=$3`, newAdmin, newOwner, userID)
	} else {
		_, _ = db.Pool.Exec(ctx, `UPDATE users SET is_admin=$1 WHERE id=$2`, newAdmin, userID)
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// ─── POST /admin/users/{id}/toggle-owner ─────────────────────────────────────

func AdminUserToggleOwner(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	if !admin.IsOwner {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	userID, _ := strconv.Atoi(r.PathValue("user_id"))
	if userID == admin.ID {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	ctx := context.Background()
	if !userCols.IsOwner {
		// Sloupec is_owner neexistuje — nelze přepínat vlastnictví
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	var isOwner bool
	_ = db.Pool.QueryRow(ctx, `SELECT is_owner FROM users WHERE id=$1`, userID).Scan(&isOwner)
	newOwner := !isOwner
	newAdmin := true // owner je vždy admin
	if userCols.IsAdmin {
		_, _ = db.Pool.Exec(ctx, `UPDATE users SET is_owner=$1, is_admin=$2 WHERE id=$3`, newOwner, newAdmin, userID)
	} else {
		_, _ = db.Pool.Exec(ctx, `UPDATE users SET is_owner=$1 WHERE id=$2`, newOwner, userID)
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// ─── POST /admin/users/{id}/toggle-inactive ──────────────────────────────────

func AdminUserToggleInactive(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	userID, _ := strconv.Atoi(r.PathValue("user_id"))
	if userID == admin.ID {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	if userCols.IsInactive {
		var cur bool
		_ = db.Pool.QueryRow(context.Background(), `SELECT is_inactive FROM users WHERE id=$1`, userID).Scan(&cur)
		_, _ = db.Pool.Exec(context.Background(), `UPDATE users SET is_inactive=$1 WHERE id=$2`, !cur, userID)
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// ─── POST /admin/users/{id}/toggle-hidden ────────────────────────────────────

func AdminUserToggleHidden(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	if !admin.IsOwner {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	userID, _ := strconv.Atoi(r.PathValue("user_id"))
	if userID == admin.ID {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	ctx := context.Background()
	if userCols.IsHidden {
		var current bool
		_ = db.Pool.QueryRow(ctx, `SELECT COALESCE(is_hidden, false) FROM users WHERE id=$1`, userID).Scan(&current)
		_, _ = db.Pool.Exec(ctx, `UPDATE users SET is_hidden=$1 WHERE id=$2`, !current, userID)
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// ─── POST /admin/users/{id}/delete ───────────────────────────────────────────

func AdminUserDelete(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	userID, _ := strconv.Atoi(r.PathValue("user_id"))
	if userID == admin.ID {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	ctx := context.Background()
	var username string
	var isOwner bool
	if userCols.IsOwner {
		_ = db.Pool.QueryRow(ctx, `SELECT username, is_owner FROM users WHERE id=$1`, userID).
			Scan(&username, &isOwner)
	} else {
		_ = db.Pool.QueryRow(ctx, `SELECT username FROM users WHERE id=$1`, userID).Scan(&username)
	}
	if isOwner && !admin.IsOwner {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	oldVal := `{"username":"` + username + `"}`
	LogAction(&admin.ID, admin.Username, "user_delete", "user", &userID, "Smazán uživatel: "+username, &oldVal, nil)
	_, _ = db.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, userID)
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// ─── POST /admin/users/{id}/reset-password ───────────────────────────────────

func AdminUserResetPassword(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	userID, _ := strconv.Atoi(r.PathValue("user_id"))
	ctx := context.Background()
	var username string
	var isOwner bool
	if userCols.IsOwner {
		_ = db.Pool.QueryRow(ctx, `SELECT username, is_owner FROM users WHERE id=$1`, userID).
			Scan(&username, &isOwner)
	} else {
		_ = db.Pool.QueryRow(ctx, `SELECT username FROM users WHERE id=$1`, userID).Scan(&username)
	}
	if isOwner && !admin.IsOwner {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	newPw := genPassword(10)
	hash, _ := HashPassword(newPw)
	_, _ = db.Pool.Exec(ctx, `UPDATE users SET password_hash=$1 WHERE id=$2`, hash, userID)
	middleware.SetFlash(w, r, "info", "Nové heslo pro <b>"+username+"</b>: <code>"+newPw+"</code>")
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// ─── POST /admin/users/{id}/set-role ─────────────────────────────────────────

func AdminUserSetRole(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	userID, _ := strconv.Atoi(r.PathValue("user_id"))
	if userID == admin.ID {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	role := strings.ToLower(r.FormValue("role"))
	if role == "owner" && !admin.IsOwner {
		role = "admin"
	}
	isOwner := role == "owner"
	isAdmin := role == "admin" || role == "owner"

	ctx := context.Background()
	var username string
	var oldIsOwner, oldIsAdmin bool
	// Čti jen existující sloupce
	switch {
	case userCols.IsOwner && userCols.IsAdmin:
		_ = db.Pool.QueryRow(ctx, `SELECT username, is_owner, is_admin FROM users WHERE id=$1`, userID).
			Scan(&username, &oldIsOwner, &oldIsAdmin)
	case userCols.IsAdmin:
		_ = db.Pool.QueryRow(ctx, `SELECT username, is_admin FROM users WHERE id=$1`, userID).
			Scan(&username, &oldIsAdmin)
	case userCols.IsOwner:
		_ = db.Pool.QueryRow(ctx, `SELECT username, is_owner FROM users WHERE id=$1`, userID).
			Scan(&username, &oldIsOwner)
	default:
		_ = db.Pool.QueryRow(ctx, `SELECT username FROM users WHERE id=$1`, userID).Scan(&username)
	}

	oldRole := "user"
	if oldIsOwner {
		oldRole = "owner"
	} else if oldIsAdmin {
		oldRole = "admin"
	}

	if oldRole != role {
		oldVal := `{"role":"` + oldRole + `"}`
		newVal := `{"role":"` + role + `"}`
		LogAction(&admin.ID, admin.Username, "user_role", "user", &userID,
			"Role "+username+": "+oldRole+" → "+role, &oldVal, &newVal)
	}
	// Aktualizuj jen existující sloupce
	switch {
	case userCols.IsOwner && userCols.IsAdmin:
		_, _ = db.Pool.Exec(ctx, `UPDATE users SET is_owner=$1, is_admin=$2 WHERE id=$3`, isOwner, isAdmin, userID)
	case userCols.IsAdmin:
		_, _ = db.Pool.Exec(ctx, `UPDATE users SET is_admin=$1 WHERE id=$2`, isAdmin, userID)
	case userCols.IsOwner:
		_, _ = db.Pool.Exec(ctx, `UPDATE users SET is_owner=$1 WHERE id=$2`, isOwner, userID)
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// ─── GET /admin/users/new ─────────────────────────────────────────────────────

func AdminUserNewForm(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		RenderTemplate(w, r, tmpl, "user_new.html", TemplateData{"User": admin})
	}
}

// ─── POST /admin/users/new ────────────────────────────────────────────────────

func AdminUserNewSubmit(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		username := strings.TrimSpace(r.FormValue("username"))
		email := strings.TrimSpace(strings.ToLower(r.FormValue("email")))
		firstName := strings.TrimSpace(r.FormValue("first_name"))
		lastName := strings.TrimSpace(r.FormValue("last_name"))
		role := strings.ToLower(r.FormValue("role"))
		password := r.FormValue("password")

		ctx := context.Background()
		var errors []string
		if username == "" {
			errors = append(errors, "Nick je povinný.")
		} else {
			var count int
			_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE username=$1`, username).Scan(&count)
			if count > 0 {
				errors = append(errors, "Nick '"+username+"' už existuje.")
			}
		}
		if email == "" {
			errors = append(errors, "E-mail je povinný.")
		} else {
			var count int
			_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE email=$1`, email).Scan(&count)
			if count > 0 {
				errors = append(errors, "E-mail '"+email+"' už existuje.")
			}
		}

		if len(errors) > 0 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			RenderTemplate(w, r, tmpl, "user_new.html", TemplateData{
				"User": admin, "Errors": errors,
				"Form": map[string]string{
					"first_name": firstName, "last_name": lastName,
					"username": username, "email": email, "role": role,
				},
			})
			return
		}

		generatedPw := strings.TrimSpace(password)
		autoGen := generatedPw == ""
		if autoGen {
			generatedPw = genPassword(10)
		}
		if role == "owner" && !admin.IsOwner {
			role = "admin"
		}
		isAdmin := role == "admin" || role == "owner"
		isOwner := role == "owner"
		hash, _ := HashPassword(generatedPw)

		_ = firstName // first_name, last_name not in Neon backup schema — suppress unused
		_ = lastName
		// Build INSERT dynamically — jen existující sloupce
		var newUserID int
		isql, ivals := buildUserInsertSQL([]UserInsertField{
			{Col: "username", Val: username, Include: true},
			{Col: "password_hash", Val: hash, Include: true},
			{Col: "email", Val: PtrStr(email), Include: userCols.Email},
			{Col: "is_admin", Val: isAdmin, Include: userCols.IsAdmin},
			{Col: "is_owner", Val: isOwner, Include: userCols.IsOwner},
			{Col: "lang", Val: "cs", Include: userCols.Lang},
			{Col: "created_at", Val: time.Now().UTC(), Include: userCols.CreatedAt},
		})
		_ = db.Pool.QueryRow(ctx, isql, ivals...).Scan(&newUserID)

		newVal, _ := json.Marshal(map[string]string{"username": username, "email": email, "role": role})
		newValStr := string(newVal)
		LogAction(&admin.ID, admin.Username, "user_create", "user", &newUserID,
			"Nový uživatel: "+username+" ("+role+")", nil, &newValStr)

		msg := "Uživatel <b>" + username + "</b> vytvořen."
		if autoGen {
			msg += " Vygenerované heslo: <code>" + generatedPw + "</code>"
		}
		middleware.SetFlash(w, r, "ok", msg)
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
	}
}

// ─── GET /admin/users/{id}/edit ───────────────────────────────────────────────

func AdminUserEditForm(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		userID, _ := strconv.Atoi(r.PathValue("user_id"))
		ctx := context.Background()
		u := &models.User{}
		cols, _ := buildUserSelect()
		err := scanUser(u, db.Pool.QueryRow(ctx, "SELECT "+cols+" FROM users WHERE id=$1", userID))
		if err != nil {
			http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
			return
		}
		if u.IsOwner && !admin.IsOwner {
			http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
			return
		}
		RenderTemplate(w, r, tmpl, "user_edit.html", TemplateData{
			"User": admin, "Edited": u,
		})
	}
}

// ─── POST /admin/users/{id}/edit ─────────────────────────────────────────────

func AdminUserEditSubmit(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		userID, _ := strconv.Atoi(r.PathValue("user_id"))
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		username := strings.TrimSpace(r.FormValue("username"))
		email := strings.TrimSpace(strings.ToLower(r.FormValue("email")))
		firstName := strings.TrimSpace(r.FormValue("first_name"))
		lastName := strings.TrimSpace(r.FormValue("last_name"))
		role := strings.ToLower(r.FormValue("role"))
		newPw := strings.TrimSpace(r.FormValue("new_password"))

		ctx := context.Background()
		// Unikátnost username
		var count int
		_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE username=$1 AND id!=$2`, username, userID).Scan(&count)
		if count > 0 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			// Jednoduše přesměruj zpět s chybou
			http.Redirect(w, r, "/admin/users/"+strconv.Itoa(userID)+"/edit", http.StatusSeeOther)
			return
		}

		if role == "owner" && !admin.IsOwner {
			role = "admin"
		}
		isOwner := role == "owner"
		isAdmin := role == "admin" || role == "owner"
		isHidden   := r.FormValue("is_hidden") == "on"
		isApproved := r.FormValue("is_approved") == "on"
		isBlocked  := r.FormValue("is_blocked") == "on"

		lang := r.FormValue("lang")
		if lang != "cs" && lang != "en" {
			lang = "cs"
		}
		// Build UPDATE dynamically — jen existující sloupce
		usql, uvals := buildUserUpdateSQL(userID, []UserUpdateField{
			{Col: "username",    Val: username,          Include: true},
			{Col: "email",       Val: PtrStr(email),     Include: userCols.Email},
			{Col: "is_owner",    Val: isOwner,           Include: userCols.IsOwner},
			{Col: "is_admin",    Val: isAdmin,           Include: userCols.IsAdmin},
			{Col: "is_hidden",   Val: isHidden,          Include: userCols.IsHidden && admin.IsOwner},
			{Col: "is_approved", Val: isApproved,        Include: userCols.IsApproved},
			{Col: "is_blocked",  Val: isBlocked,         Include: userCols.IsBlocked},
			{Col: "lang",        Val: lang,              Include: userCols.Lang},
			{Col: "first_name",  Val: PtrStr(firstName), Include: userCols.FirstName},
			{Col: "last_name",   Val: PtrStr(lastName),  Include: userCols.LastName},
		})
		_, _ = db.Pool.Exec(ctx, usql, uvals...)

		if newPw != "" {
			hash, _ := HashPassword(newPw)
			_, _ = db.Pool.Exec(ctx, `UPDATE users SET password_hash=$1 WHERE id=$2`, hash, userID)
		}
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
	}
}

// ─── GET /admin/manual ────────────────────────────────────────────────────────

func AdminManual(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		var content string
		_ = db.Pool.QueryRow(context.Background(),
			`SELECT value FROM site_config WHERE key='page_pravidla'`).Scan(&content)
		RenderTemplate(w, r, tmpl, "manual.html", TemplateData{
			"User":    admin,
			"Content": template.HTML(content),
		})
	}
}

// ─── GET /admin/quick-add-match ───────────────────────────────────────────────

func AdminQuickAddMatch(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	ctx := context.Background()
	var compID int
	err := db.Pool.QueryRow(ctx,
		`SELECT id FROM competitions WHERE is_active=true ORDER BY id DESC LIMIT 1`).Scan(&compID)
	if err == nil {
		http.Redirect(w, r, "/admin/competitions/"+strconv.Itoa(compID)+"/rounds", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// ─── POST /admin/competitions/{id}/sort-order (AJAX) ─────────────────────────

func AdminCompetitionSortOrder(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
		return
	}
	compID, _ := strconv.Atoi(r.PathValue("competition_id"))
	if err := r.ParseForm(); err != nil {
		return
	}
	sortVal := strings.TrimSpace(r.FormValue("sort_order"))
	var sortOrder *int
	if sortVal != "" {
		n, err := strconv.Atoi(sortVal)
		if err == nil {
			sortOrder = &n
		}
	}
	_, _ = db.Pool.Exec(context.Background(),
		`UPDATE competitions SET sort_order=$1 WHERE id=$2`, sortOrder, compID)
	w.Header().Set("Content-Type", "application/json")
	sortStr := "null"
	if sortOrder != nil {
		sortStr = strconv.Itoa(*sortOrder)
	}
	_, _ = w.Write([]byte(`{"ok":true,"sort_order":` + sortStr + `}`))
}

// ─── GET /admin/io ────────────────────────────────────────────────────────────

func AdminIO(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		ctx := context.Background()
		compRows, _ := db.Pool.Query(ctx,
			`SELECT id, name, season, is_active, sport, sort_order FROM competitions ORDER BY id DESC`)
		var competitions []*models.Competition
		var activeCompetitions []*models.Competition
		for compRows.Next() {
			c := &models.Competition{}
			_ = compRows.Scan(&c.ID, &c.Name, &c.Season, &c.IsActive, &c.Sport, &c.SortOrder)
			competitions = append(competitions, c)
			if c.IsActive {
				activeCompetitions = append(activeCompetitions, c)
			}
		}
		compRows.Close()

		RenderTemplate(w, r, tmpl, "admin/io.html", TemplateData{
			"User":               admin,
			"Competitions":       competitions,
			"ActiveCompetitions": activeCompetitions,
		})
	}
}

// ─── POST /admin/users/{id}/send-welcome ─────────────────────────────────────

func AdminUserSendWelcome(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	userID, _ := strconv.Atoi(r.PathValue("user_id"))
	ctx := context.Background()
	var username, email string
	_ = db.Pool.QueryRow(ctx, `SELECT username, COALESCE(email,'') FROM users WHERE id=$1`, userID).
		Scan(&username, &email)
	if email == "" {
		middleware.SetFlash(w, r, "err", "Uživatel <b>"+username+"</b> nemá email.")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	subject := "Vítej v Tipovačce!"
	body := "Ahoj " + username + ",\n\nvítej v Tipovačce!\nPřihlásit se můžeš zde: https://tipovacka-3.fly.dev/login\n\nPokud máš dotazy, odpověz na tento email.\n\nHodně štěstí!\nTipovačka"
	if err := sendEmail(email, subject, body); err != nil {
		middleware.SetFlash(w, r, "err", "Chyba při odesílání emailu: "+err.Error())
	} else {
		middleware.SetFlash(w, r, "ok", "Welcome email odeslán na <b>"+email+"</b>.")
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func genPassword(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, length)
	for {
		for i := range b {
			n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
			b[i] = charset[n.Int64()]
		}
		s := string(b)
		hasUpper := false
		hasLower := false
		hasDigit := false
		for _, c := range s {
			if c >= 'A' && c <= 'Z' {
				hasUpper = true
			}
			if c >= 'a' && c <= 'z' {
				hasLower = true
			}
			if c >= '0' && c <= '9' {
				hasDigit = true
			}
		}
		if hasUpper && hasLower && hasDigit {
			return s
		}
	}
}
