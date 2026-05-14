// handlers/tips.go — Tipovačka 2.0
// Zobrazení zápasů a zadávání tipů.
package handlers

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"tipovacka/db"
	"tipovacka/models"
)

// ─── GET / ────────────────────────────────────────────────────────────────────

// IndexMatchCtx drží data pro jedno tipovatelné utkání na hlavní stránce.
type IndexMatchCtx struct {
	Match     *models.Match
	Tip       *models.Tip
	CompName  string
	CompID    int
	RoundName string
}

func Index(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := RequireLogin(w, r)
		if u == nil {
			return
		}

		ctx := context.Background()
		rows, err := db.Pool.Query(ctx,
			`SELECT id, name, season, is_active, sport, sort_order
			   FROM competitions WHERE is_active = true ORDER BY COALESCE(sort_order,9999) ASC, id DESC`)
		if err != nil {
			http.Error(w, "DB error", http.StatusInternalServerError)
			return
		}
		var comps []*models.Competition
		for rows.Next() {
			c := &models.Competition{}
			_ = rows.Scan(&c.ID, &c.Name, &c.Season, &c.IsActive, &c.Sport, &c.SortOrder)
			comps = append(comps, c)
		}
		rows.Close()

		if len(comps) == 1 {
			http.Redirect(w, r, "/competition/"+strconv.Itoa(comps[0].ID), http.StatusSeeOther)
			return
		}

		// Načti tipovatelné zápasy ze všech aktivních soutěží
		var openMatches []IndexMatchCtx
		if len(comps) > 0 {
			compIDs := make([]int, len(comps))
			compByID := map[int]*models.Competition{}
			for i, c := range comps {
				compIDs[i] = c.ID
				compByID[c.ID] = c
			}

			// Aktivní kola
			type roundRow struct {
				ID       int
				CompID   int
				Name     string
				Deadline *time.Time
			}
			rndByID := map[int]roundRow{}
			rndRows, _ := db.Pool.Query(ctx,
				`SELECT id, competition_id, name, deadline FROM rounds
				  WHERE competition_id = ANY($1) AND is_active = true`, compIDs)
			for rndRows.Next() {
				var rr roundRow
				_ = rndRows.Scan(&rr.ID, &rr.CompID, &rr.Name, &rr.Deadline)
				rndByID[rr.ID] = rr
			}
			rndRows.Close()

			if len(rndByID) > 0 {
				rndIDs := make([]int, 0, len(rndByID))
				for id := range rndByID {
					rndIDs = append(rndIDs, id)
				}

				// Otevřené zápasy
				mRows, _ := db.Pool.Query(ctx,
					`SELECT m.id, m.round_id, m.home_team_id, m.away_team_id,
					        m.home_score, m.away_score, m.match_date, m.is_finished,
					        ht.id, ht.name, ht.display_name,
					        at.id, at.name, at.display_name
					   FROM matches m
					   JOIN teams ht ON ht.id = m.home_team_id
					   JOIN teams at ON at.id = m.away_team_id
					  WHERE m.round_id = ANY($1) AND m.is_finished = false
					  ORDER BY m.match_date ASC NULLS LAST`, rndIDs)

				var matchIDs []int
				var pendingMatches []*models.Match
				for mRows.Next() {
					m := &models.Match{HomeTeam: &models.Team{}, AwayTeam: &models.Team{}}
					_ = mRows.Scan(
						&m.ID, &m.RoundID, &m.HomeTeamID, &m.AwayTeamID,
						&m.HomeScore, &m.AwayScore, &m.MatchDate, &m.IsFinished,
						&m.HomeTeam.ID, &m.HomeTeam.Name, &m.HomeTeam.DisplayName,
						&m.AwayTeam.ID, &m.AwayTeam.Name, &m.AwayTeam.DisplayName)
					rr := rndByID[m.RoundID]
					// Filtruj: musí mít otevřenou uzávěrku
					rndModel := &models.Round{ID: rr.ID, CompetitionID: rr.CompID, Name: rr.Name, Deadline: rr.Deadline}
					if IsBeforeDeadline(rndModel, m) {
						pendingMatches = append(pendingMatches, m)
						matchIDs = append(matchIDs, m.ID)
					}
				}
				mRows.Close()

				// Tipy uživatele
				tipMap := map[int]*models.Tip{}
				if len(matchIDs) > 0 {
					tRows, _ := db.Pool.Query(ctx,
						`SELECT id, user_id, match_id, home_score, away_score, points, created_at
						   FROM tips WHERE user_id = $1 AND match_id = ANY($2)`,
						u.ID, matchIDs)
					for tRows.Next() {
						t := &models.Tip{}
						_ = tRows.Scan(&t.ID, &t.UserID, &t.MatchID, &t.HomeScore, &t.AwayScore, &t.Points, &t.CreatedAt)
						tipMap[t.MatchID] = t
					}
					tRows.Close()
				}

				for _, m := range pendingMatches {
					rr := rndByID[m.RoundID]
					comp := compByID[rr.CompID]
					ctx2 := IndexMatchCtx{
						Match:     m,
						Tip:       tipMap[m.ID],
						RoundName: rr.Name,
					}
					if comp != nil {
						ctx2.CompName = comp.Name
						ctx2.CompID = comp.ID
					}
					openMatches = append(openMatches, ctx2)
				}
			}
		}

		RenderTemplate(w, r, tmpl, "index.html", TemplateData{
			"User":         u,
			"Competitions": comps,
			"OpenMatches":  openMatches,
		})
	}
}

// ─── GET /competition/{id} ────────────────────────────────────────────────────

func CompetitionDetail(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := RequireLogin(w, r)
		if u == nil {
			return
		}

		compID, err := strconv.Atoi(r.PathValue("competition_id"))
		if err != nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		ctx := context.Background()

		// Načti soutěž
		comp := &models.Competition{}
		err = db.Pool.QueryRow(ctx,
			`SELECT id, name, season, is_active, sport, sort_order FROM competitions WHERE id = $1`, compID).
			Scan(&comp.ID, &comp.Name, &comp.Season, &comp.IsActive, &comp.Sport, &comp.SortOrder)
		if err != nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		// Aktivní kola
		roundRows, _ := db.Pool.Query(ctx,
			`SELECT id, competition_id, name, deadline, is_active FROM rounds
			  WHERE competition_id = $1 AND is_active = true ORDER BY id`, compID)
		roundsByID := map[int]*models.Round{}
		for roundRows.Next() {
			rnd := &models.Round{}
			_ = roundRows.Scan(&rnd.ID, &rnd.CompetitionID, &rnd.Name, &rnd.Deadline, &rnd.IsActive)
			roundsByID[rnd.ID] = rnd
		}
		roundRows.Close()

		if len(roundsByID) == 0 {
			RenderTemplate(w, r, tmpl, "competition.html", TemplateData{
				"User": u, "Comp": comp, "MatchContext": nil, "AllLocked": false,
			})
			return
		}

		roundIDs := make([]int, 0, len(roundsByID))
		for id := range roundsByID {
			roundIDs = append(roundIDs, id)
		}

		// Nevyhodnocené zápasy v aktivních kolech
		matchRows, _ := db.Pool.Query(ctx,
			`SELECT m.id, m.round_id, m.home_team_id, m.away_team_id,
			        m.home_score, m.away_score, m.match_date, m.is_finished,
			        ht.id, ht.name, ht.display_name,
			        at.id, at.name, at.display_name
			   FROM matches m
			   JOIN teams ht ON ht.id = m.home_team_id
			   JOIN teams at ON at.id = m.away_team_id
			  WHERE m.round_id = ANY($1) AND m.is_finished = false
			  ORDER BY m.match_date ASC NULLS LAST`, roundIDs)

		type matchCtxRow struct {
			Match     *models.Match
			Tip       *models.Tip
			CanTip    bool
			RoundName string
		}

		var matchContextAll []matchCtxRow
		var matchIDs []int
		for matchRows.Next() {
			m := &models.Match{HomeTeam: &models.Team{}, AwayTeam: &models.Team{}}
			_ = matchRows.Scan(
				&m.ID, &m.RoundID, &m.HomeTeamID, &m.AwayTeamID,
				&m.HomeScore, &m.AwayScore, &m.MatchDate, &m.IsFinished,
				&m.HomeTeam.ID, &m.HomeTeam.Name, &m.HomeTeam.DisplayName,
				&m.AwayTeam.ID, &m.AwayTeam.Name, &m.AwayTeam.DisplayName)
			rnd := roundsByID[m.RoundID]
			canTip := IsBeforeDeadline(rnd, m)
			matchContextAll = append(matchContextAll, matchCtxRow{
				Match: m, CanTip: canTip, RoundName: rnd.Name,
			})
			matchIDs = append(matchIDs, m.ID)
		}
		matchRows.Close()

		// Tipy uživatele
		tipsByMatch := map[int]*models.Tip{}
		if len(matchIDs) > 0 {
			tipRows, _ := db.Pool.Query(ctx,
				`SELECT id, user_id, match_id, home_score, away_score, points, created_at
				   FROM tips WHERE user_id = $1 AND match_id = ANY($2)`,
				u.ID, matchIDs)
			for tipRows.Next() {
				t := &models.Tip{}
				_ = tipRows.Scan(&t.ID, &t.UserID, &t.MatchID, &t.HomeScore, &t.AwayScore, &t.Points, &t.CreatedAt)
				tipsByMatch[t.MatchID] = t
			}
			tipRows.Close()
		}

		var tippable []matchCtxRow
		for i := range matchContextAll {
			row := matchContextAll[i]
			row.Tip = tipsByMatch[row.Match.ID]
			if row.CanTip {
				tippable = append(tippable, row)
			}
		}

		allLocked := len(matchContextAll) > 0 && len(tippable) == 0

		// Convert to interface{} slice for template
		tippableIface := make([]interface{}, len(tippable))
		for i, v := range tippable {
			tippableIface[i] = v
		}

		// Extra otázky — zjisti jestli jsou otevřené (deadline ještě nepřešel)
		extraOpen := false
		hasExtra := false
		{
			var extraDeadline *time.Time
			_ = db.Pool.QueryRow(ctx,
				`SELECT extra_deadline FROM competitions WHERE id=$1`, compID).Scan(&extraDeadline)

			var effectiveDeadline *time.Time
			if extraDeadline != nil {
				effectiveDeadline = extraDeadline
			} else {
				var firstMatch time.Time
				err2 := db.Pool.QueryRow(ctx,
					`SELECT MIN(m.match_date) FROM matches m
					   JOIN rounds r ON r.id = m.round_id
					  WHERE r.competition_id = $1 AND m.match_date IS NOT NULL`, compID).Scan(&firstMatch)
				if err2 == nil && !firstMatch.IsZero() {
					effectiveDeadline = &firstMatch
				}
			}

			// Má vůbec soutěž nějaké extra otázky?
			var extraCount int
			_ = db.Pool.QueryRow(ctx,
				`SELECT COUNT(*) FROM extra_questions WHERE competition_id=$1`, compID).Scan(&extraCount)
			hasExtra = extraCount > 0

			if hasExtra {
				now := NowPrague()
				if effectiveDeadline == nil || now.Before(*effectiveDeadline) {
					extraOpen = true
				}
			}
		}

		RenderTemplate(w, r, tmpl, "competition.html", TemplateData{
			"User":         u,
			"Comp":         comp,
			"MatchContext": tippableIface,
			"AllLocked":    allLocked,
			"ExtraOpen":    extraOpen,
		})
	}
}

// ─── GET /round/{id} (redirect) ──────────────────────────────────────────────

func RoundRedirect(w http.ResponseWriter, r *http.Request) {
	roundID, err := strconv.Atoi(r.PathValue("round_id"))
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	ctx := context.Background()
	var compID int
	err = db.Pool.QueryRow(ctx, `SELECT competition_id FROM rounds WHERE id = $1`, roundID).Scan(&compID)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/competition/"+strconv.Itoa(compID), http.StatusMovedPermanently)
}

// ─── POST /tips/submit ────────────────────────────────────────────────────────

func SubmitTip(w http.ResponseWriter, r *http.Request) {
	u := RequireLogin(w, r)
	if u == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	matchID, _ := strconv.Atoi(r.FormValue("match_id"))
	homeScore, _ := strconv.Atoi(r.FormValue("home_score"))
	awayScore, _ := strconv.Atoi(r.FormValue("away_score"))

	ctx := context.Background()

	// Načti zápas
	m := &models.Match{}
	var roundID int
	err := db.Pool.QueryRow(ctx,
		`SELECT id, round_id, match_date, is_finished FROM matches WHERE id = $1`, matchID).
		Scan(&m.ID, &roundID, &m.MatchDate, &m.IsFinished)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	m.RoundID = roundID

	// Načti kolo
	rnd := &models.Round{}
	err = db.Pool.QueryRow(ctx,
		`SELECT id, competition_id, deadline FROM rounds WHERE id = $1`, roundID).
		Scan(&rnd.ID, &rnd.CompetitionID, &rnd.Deadline)
	if err != nil || !IsBeforeDeadline(rnd, m) {
		http.Redirect(w, r, "/competition/"+strconv.Itoa(rnd.CompetitionID), http.StatusSeeOther)
		return
	}

	// Upsert tipu
	var existingID int
	var oldHome, oldAway int
	wasNew := true
	err = db.Pool.QueryRow(ctx,
		`SELECT id, home_score, away_score FROM tips WHERE user_id = $1 AND match_id = $2`,
		u.ID, matchID).Scan(&existingID, &oldHome, &oldAway)
	if err == nil {
		wasNew = false
		_, _ = db.Pool.Exec(ctx,
			`UPDATE tips SET home_score = $1, away_score = $2, points = NULL WHERE id = $3`,
			homeScore, awayScore, existingID)
	} else {
		err = db.Pool.QueryRow(ctx,
			`INSERT INTO tips (user_id, match_id, home_score, away_score, created_at)
			 VALUES ($1, $2, $3, $4, NOW()) RETURNING id`,
			u.ID, matchID, homeScore, awayScore).Scan(&existingID)
		if err != nil {
			http.Redirect(w, r, "/competition/"+strconv.Itoa(rnd.CompetitionID), http.StatusSeeOther)
			return
		}
	}

	// Audit log
	var desc string
	if wasNew {
		desc = "Tip: " + strconv.Itoa(homeScore) + ":" + strconv.Itoa(awayScore)
	} else {
		desc = "Změna tipu: " + strconv.Itoa(oldHome) + ":" + strconv.Itoa(oldAway) + " → " + strconv.Itoa(homeScore) + ":" + strconv.Itoa(awayScore)
	}
	newVal := `{"home_score":` + strconv.Itoa(homeScore) + `,"away_score":` + strconv.Itoa(awayScore) + `}`
	LogAction(&u.ID, u.Username, "tip_save", "tip", &existingID, desc, nil, &newVal)

	http.Redirect(w, r, "/competition/"+strconv.Itoa(rnd.CompetitionID), http.StatusSeeOther)
}

// ─── POST /tips/submit-ajax ───────────────────────────────────────────────────

func SubmitTipAjax(w http.ResponseWriter, r *http.Request) {
	u := GetCurrentUser(r)
	if u == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"error":"not_logged_in"}`))
		return
	}
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	matchID, _ := strconv.Atoi(r.FormValue("match_id"))
	homeScore, _ := strconv.Atoi(r.FormValue("home_score"))
	awayScore, _ := strconv.Atoi(r.FormValue("away_score"))

	ctx := context.Background()
	m := &models.Match{}
	var roundID int
	err := db.Pool.QueryRow(ctx,
		`SELECT id, round_id, match_date, is_finished FROM matches WHERE id = $1`, matchID).
		Scan(&m.ID, &roundID, &m.MatchDate, &m.IsFinished)
	if err != nil {
		jsonError(w, "match_not_found", http.StatusNotFound)
		return
	}
	m.RoundID = roundID

	rnd := &models.Round{}
	_ = db.Pool.QueryRow(ctx, `SELECT id, competition_id, deadline FROM rounds WHERE id = $1`, roundID).
		Scan(&rnd.ID, &rnd.CompetitionID, &rnd.Deadline)

	if !IsBeforeDeadline(rnd, m) {
		jsonError(w, "deadline_passed", http.StatusForbidden)
		return
	}

	var existingID int
	var oldHome, oldAway int
	wasNew := true
	err = db.Pool.QueryRow(ctx,
		`SELECT id, home_score, away_score FROM tips WHERE user_id = $1 AND match_id = $2`,
		u.ID, matchID).Scan(&existingID, &oldHome, &oldAway)
	if err == nil {
		wasNew = false
		_, _ = db.Pool.Exec(ctx,
			`UPDATE tips SET home_score = $1, away_score = $2, points = NULL WHERE id = $3`,
			homeScore, awayScore, existingID)
	} else {
		_ = db.Pool.QueryRow(ctx,
			`INSERT INTO tips (user_id, match_id, home_score, away_score, created_at)
			 VALUES ($1, $2, $3, $4, NOW()) RETURNING id`,
			u.ID, matchID, homeScore, awayScore).Scan(&existingID)
	}

	var desc string
	if wasNew {
		desc = "Tip: " + strconv.Itoa(homeScore) + ":" + strconv.Itoa(awayScore)
	} else {
		desc = "Změna tipu: " + strconv.Itoa(oldHome) + ":" + strconv.Itoa(oldAway) + " → " + strconv.Itoa(homeScore) + ":" + strconv.Itoa(awayScore)
	}
	newVal := `{"home_score":` + strconv.Itoa(homeScore) + `,"away_score":` + strconv.Itoa(awayScore) + `}`
	LogAction(&u.ID, u.Username, "tip_save", "tip", &existingID, desc, nil, &newVal)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok": true, "home": homeScore, "away": awayScore,
	})
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`{"ok":false,"error":"` + msg + `"}`))
}

// Suppress unused import warning
var _ = time.Now
