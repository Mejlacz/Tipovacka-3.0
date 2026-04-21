// handlers/admin_rounds.go — Tipovačka 2.0
// Správa kol.
package handlers

import (
	"context"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tipovacka/db"
	"tipovacka/middleware"
	"tipovacka/models"
)

// GET /admin/competitions/{id}/rounds
func AdminRoundsList(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		compID, _ := strconv.Atoi(r.PathValue("competition_id"))
		ctx := context.Background()

		comp := &models.Competition{}
		err := db.Pool.QueryRow(ctx,
			`SELECT id, name, season, is_active, sport, sort_order FROM competitions WHERE id=$1`, compID).
			Scan(&comp.ID, &comp.Name, &comp.Season, &comp.IsActive, &comp.Sport, &comp.SortOrder)
		if err != nil {
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}

		rows, _ := db.Pool.Query(ctx,
			`SELECT id, competition_id, name, deadline, is_active FROM rounds
			  WHERE competition_id=$1 ORDER BY id DESC`, compID)
		var rounds []*models.Round
		for rows.Next() {
			rnd := &models.Round{}
			_ = rows.Scan(&rnd.ID, &rnd.CompetitionID, &rnd.Name, &rnd.Deadline, &rnd.IsActive)
			rounds = append(rounds, rnd)
		}
		rows.Close()

		RenderTemplate(w, r, tmpl, "rounds.html", TemplateData{
			"User":  admin,
			"Comp":  comp,
			"Rounds": rounds,
		})
	}
}

// POST /admin/competitions/{id}/rounds/new
func AdminRoundCreate(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	compID, _ := strconv.Atoi(r.PathValue("competition_id"))
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	deadlineStr := r.FormValue("deadline")
	var deadline *time.Time
	if deadlineStr != "" {
		t, err := time.ParseInLocation("2006-01-02T15:04", deadlineStr, pragueLocation)
		if err == nil {
			deadline = &t
		}
	}
	_, _ = db.Pool.Exec(context.Background(),
		`INSERT INTO rounds (competition_id, name, deadline, is_active) VALUES ($1,$2,$3,true)`,
		compID, name, deadline)
	http.Redirect(w, r, "/admin/competitions/"+strconv.Itoa(compID)+"/rounds", http.StatusSeeOther)
}

// POST /admin/rounds/{id}/edit
func AdminRoundEdit(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	roundID, _ := strconv.Atoi(r.PathValue("round_id"))
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	deadlineStr := r.FormValue("deadline")

	ctx := context.Background()
	var compID int
	_ = db.Pool.QueryRow(ctx, `SELECT competition_id FROM rounds WHERE id=$1`, roundID).Scan(&compID)

	var deadline *time.Time
	if deadlineStr != "" {
		t, err := time.ParseInLocation("2006-01-02T15:04", deadlineStr, pragueLocation)
		if err == nil {
			deadline = &t
		}
	}
	_, _ = db.Pool.Exec(ctx,
		`UPDATE rounds SET name=$1, deadline=$2 WHERE id=$3`, name, deadline, roundID)
	http.Redirect(w, r, "/admin/competitions/"+strconv.Itoa(compID)+"/rounds", http.StatusSeeOther)
}

// POST /admin/rounds/{id}/toggle
func AdminRoundToggle(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	roundID, _ := strconv.Atoi(r.PathValue("round_id"))
	ctx := context.Background()
	var compID int
	var isActive bool
	_ = db.Pool.QueryRow(ctx, `SELECT competition_id, is_active FROM rounds WHERE id=$1`, roundID).
		Scan(&compID, &isActive)
	_, _ = db.Pool.Exec(ctx, `UPDATE rounds SET is_active=$1 WHERE id=$2`, !isActive, roundID)
	http.Redirect(w, r, "/admin/competitions/"+strconv.Itoa(compID)+"/rounds", http.StatusSeeOther)
}

// POST /admin/rounds/{id}/notify-new
func AdminRoundNotifyNew(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	roundID, _ := strconv.Atoi(r.PathValue("round_id"))
	ctx := context.Background()
	var compID int
	_ = db.Pool.QueryRow(ctx, `SELECT competition_id FROM rounds WHERE id=$1`, roundID).Scan(&compID)

	// TODO: implement — email notify odběratelů
	middleware.SetFlash(w, r, "warn", "Email notifikace nejsou v Go verzi ještě implementovány.")
	http.Redirect(w, r, "/admin/competitions/"+strconv.Itoa(compID)+"/rounds", http.StatusSeeOther)
}
