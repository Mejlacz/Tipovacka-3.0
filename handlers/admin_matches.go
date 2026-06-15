// handlers/admin_matches.go — Tipovačka 3.0
// Správa zápasů + výsledky + přepočet bodů.
package handlers

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"tipovacka/db"
	"tipovacka/middleware"
	"tipovacka/models"
)

// GET /admin/competitions/{competition_id}/matches
func AdminMatchesList(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		compID, _ := strconv.Atoi(r.PathValue("competition_id"))
		ctx := context.Background()

		comp := &models.Competition{}
		err := db.Pool.QueryRow(ctx,
			`SELECT id, name, season, is_active, sport, sort_order, deadline FROM competitions WHERE id=$1`, compID).
			Scan(&comp.ID, &comp.Name, &comp.Season, &comp.IsActive, &comp.Sport, &comp.SortOrder, &comp.Deadline)
		if err != nil {
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}

		matchRows, _ := db.Pool.Query(ctx,
			`SELECT m.id, m.competition_id, m.home_team_id, m.away_team_id,
			        m.home_score, m.away_score, m.match_date, m.is_finished,
			        ht.id, ht.name, ht.display_name,
			        at.id, at.name, at.display_name
			   FROM matches m
			   JOIN teams ht ON ht.id = m.home_team_id
			   JOIN teams at ON at.id = m.away_team_id
			  WHERE m.competition_id=$1
			  ORDER BY m.is_finished ASC, m.match_date ASC NULLS LAST`, compID)
		var matches []*models.Match
		for matchRows.Next() {
			m := &models.Match{HomeTeam: &models.Team{}, AwayTeam: &models.Team{}}
			_ = matchRows.Scan(
				&m.ID, &m.CompetitionID, &m.HomeTeamID, &m.AwayTeamID,
				&m.HomeScore, &m.AwayScore, &m.MatchDate, &m.IsFinished,
				&m.HomeTeam.ID, &m.HomeTeam.Name, &m.HomeTeam.DisplayName,
				&m.AwayTeam.ID, &m.AwayTeam.Name, &m.AwayTeam.DisplayName)
			matches = append(matches, m)
		}
		matchRows.Close()

		// Týmy pro danou soutěž
		teamRows, _ := db.Pool.Query(ctx,
			`SELECT t.id, t.name, t.sport, t.alias, t.display_name, t.logo_url, t.category, t.competition_id
			   FROM teams t
			   JOIN competition_teams ct ON ct.team_id = t.id
			  WHERE ct.competition_id=$1
			  ORDER BY t.name`, compID)
		var teams []*models.Team
		for teamRows.Next() {
			t := &models.Team{}
			_ = teamRows.Scan(&t.ID, &t.Name, &t.Sport, &t.Alias, &t.DisplayName, &t.LogoURL, &t.Category, &t.CompetitionID)
			teams = append(teams, t)
		}
		teamRows.Close()

		// Varování o duplicitním zápasu (přichází přes query params z POST handleru)
		var dupWarn map[string]interface{}
		if dupHomeStr := r.URL.Query().Get("dup_home"); dupHomeStr != "" {
			homeID, _ := strconv.Atoi(dupHomeStr)
			awayID, _ := strconv.Atoi(r.URL.Query().Get("dup_away"))
			var homeName, awayName string
			_ = db.Pool.QueryRow(ctx, `SELECT COALESCE(display_name, name) FROM teams WHERE id=$1`, homeID).Scan(&homeName)
			_ = db.Pool.QueryRow(ctx, `SELECT COALESCE(display_name, name) FROM teams WHERE id=$1`, awayID).Scan(&awayName)
			tipCount, _ := strconv.Atoi(r.URL.Query().Get("dup_tips"))
			dupWarn = map[string]interface{}{
				"HomeID":   homeID,
				"AwayID":   awayID,
				"DateStr":  r.URL.Query().Get("dup_date"),
				"Tips":     tipCount,
				"HomeName": homeName,
				"AwayName": awayName,
			}
		}

		RenderTemplate(w, r, tmpl, "matches.html", TemplateData{
			"User":    admin,
			"Comp":    comp,
			"Matches": matches,
			"Teams":   teams,
			"Flash":   middleware.GetFlash(w, r),
			"DupWarn": dupWarn,
		})
	}
}

// POST /admin/competitions/{competition_id}/matches/new
func AdminMatchCreate(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	compID, _ := strconv.Atoi(r.PathValue("competition_id"))
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	homeTeamID, _ := strconv.Atoi(r.FormValue("home_team_id"))
	awayTeamID, _ := strconv.Atoi(r.FormValue("away_team_id"))
	matchDateStr := r.FormValue("match_date")

	if homeTeamID == 0 || awayTeamID == 0 {
		middleware.SetFlash(w, r, "error", "Chyba: nevybrán tým (domácí="+strconv.Itoa(homeTeamID)+", hosté="+strconv.Itoa(awayTeamID)+"). Jsou týmy přiřazeny k soutěži?")
		http.Redirect(w, r, "/admin/competitions/"+strconv.Itoa(compID)+"/matches", http.StatusSeeOther)
		return
	}

	var matchDate *time.Time
	if matchDateStr != "" {
		t, err := time.ParseInLocation("2006-01-02T15:04", matchDateStr, pragueLocation)
		if err == nil {
			matchDate = &t
		}
	}

	ctx := context.Background()

	// Kontrola duplicitního zápasu (stejné týmy, stejný den, stejná soutěž)
	if r.FormValue("force") != "1" {
		var dupID, tipCount int
		var dupErr error
		if matchDate != nil {
			dupErr = db.Pool.QueryRow(ctx,
				`SELECT m.id, COUNT(t.id)
				   FROM matches m
				   LEFT JOIN tips t ON t.match_id = m.id
				  WHERE m.competition_id=$1 AND m.home_team_id=$2 AND m.away_team_id=$3
				    AND m.match_date::date = $4::date
				  GROUP BY m.id LIMIT 1`,
				compID, homeTeamID, awayTeamID, matchDate).Scan(&dupID, &tipCount)
		} else {
			dupErr = db.Pool.QueryRow(ctx,
				`SELECT m.id, COUNT(t.id)
				   FROM matches m
				   LEFT JOIN tips t ON t.match_id = m.id
				  WHERE m.competition_id=$1 AND m.home_team_id=$2 AND m.away_team_id=$3
				    AND m.match_date IS NULL
				  GROUP BY m.id LIMIT 1`,
				compID, homeTeamID, awayTeamID).Scan(&dupID, &tipCount)
		}
		if dupErr == nil && dupID > 0 {
			q := url.Values{}
			q.Set("dup_home", strconv.Itoa(homeTeamID))
			q.Set("dup_away", strconv.Itoa(awayTeamID))
			q.Set("dup_date", matchDateStr)
			q.Set("dup_tips", strconv.Itoa(tipCount))
			q.Set("dup_existing", strconv.Itoa(dupID))
			http.Redirect(w, r, "/admin/competitions/"+strconv.Itoa(compID)+"/matches?"+q.Encode(), http.StatusSeeOther)
			return
		}
	}

	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO matches (competition_id, home_team_id, away_team_id, match_date, is_finished)
		 VALUES ($1,$2,$3,$4,false)`,
		compID, homeTeamID, awayTeamID, matchDate); err != nil {
		middleware.SetFlash(w, r, "error", "Chyba při ukládání zápasu: "+err.Error())
	} else {
		middleware.SetFlash(w, r, "ok", "Zápas přidán.")
	}
	http.Redirect(w, r, "/admin/competitions/"+strconv.Itoa(compID)+"/matches", http.StatusSeeOther)
}

// POST /admin/matches/{id}/edit
func AdminMatchEdit(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	matchID, _ := strconv.Atoi(r.PathValue("match_id"))
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	homeTeamID, _ := strconv.Atoi(r.FormValue("home_team_id"))
	awayTeamID, _ := strconv.Atoi(r.FormValue("away_team_id"))
	matchDateStr := r.FormValue("match_date")

	ctx := context.Background()
	// Zjisti competition_id zápasu
	var compID int
	_ = db.Pool.QueryRow(ctx, `SELECT competition_id FROM matches WHERE id=$1`, matchID).Scan(&compID)

	if matchDateStr != "" {
		var matchDate *time.Time
		if t, err := time.ParseInLocation("2006-01-02T15:04", matchDateStr, pragueLocation); err == nil {
			matchDate = &t
		}
		_, _ = db.Pool.Exec(ctx,
			`UPDATE matches SET home_team_id=$1, away_team_id=$2, match_date=$3 WHERE id=$4`,
			homeTeamID, awayTeamID, matchDate, matchID)
	} else {
		_, _ = db.Pool.Exec(ctx,
			`UPDATE matches SET home_team_id=$1, away_team_id=$2 WHERE id=$3`,
			homeTeamID, awayTeamID, matchID)
	}
	http.Redirect(w, r, "/admin/competitions/"+strconv.Itoa(compID)+"/matches", http.StatusSeeOther)
}

// POST /admin/matches/{id}/result
func AdminMatchSetResult(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	matchID, _ := strconv.Atoi(r.PathValue("match_id"))
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	homeScore, _ := strconv.Atoi(r.FormValue("home_score"))
	awayScore, _ := strconv.Atoi(r.FormValue("away_score"))

	ctx := context.Background()

	// Načti starý stav pro audit
	var oldHome, oldAway *int
	var oldFinished bool
	var compIDForResult int
	var homeTeamID, awayTeamID int
	_ = db.Pool.QueryRow(ctx,
		`SELECT competition_id, home_team_id, away_team_id, home_score, away_score, is_finished FROM matches WHERE id=$1`, matchID).
		Scan(&compIDForResult, &homeTeamID, &awayTeamID, &oldHome, &oldAway, &oldFinished)

	// Načti jména týmů
	var homeName, awayName string
	_ = db.Pool.QueryRow(ctx, `SELECT COALESCE(display_name, name) FROM teams WHERE id=$1`, homeTeamID).Scan(&homeName)
	_ = db.Pool.QueryRow(ctx, `SELECT COALESCE(display_name, name) FROM teams WHERE id=$1`, awayTeamID).Scan(&awayName)

	oldScoreStr := "–"
	if oldHome != nil && oldAway != nil {
		oldScoreStr = strconv.Itoa(*oldHome) + ":" + strconv.Itoa(*oldAway)
	}

	_, _ = db.Pool.Exec(ctx,
		`UPDATE matches SET home_score=$1, away_score=$2, is_finished=true WHERE id=$3`,
		homeScore, awayScore, matchID)

	desc := "Skóre " + homeName + " – " + awayName + ": " + oldScoreStr + " → " + strconv.Itoa(homeScore) + ":" + strconv.Itoa(awayScore)
	oldVal := map[string]interface{}{"home_score": oldHome, "away_score": oldAway, "is_finished": oldFinished}
	newVal := map[string]interface{}{"home_score": homeScore, "away_score": awayScore, "is_finished": true}
	oldJSON, _ := json.Marshal(oldVal)
	newJSON, _ := json.Marshal(newVal)
	oldStr := string(oldJSON)
	newStr := string(newJSON)
	LogAction(&admin.ID, admin.Username, "match_score", "match", &matchID, desc, &oldStr, &newStr)

	RecalculateTips(ctx, matchID, homeScore, awayScore)
	RecalculateStandings(compIDForResult)

	http.Redirect(w, r, "/admin/competitions/"+strconv.Itoa(compIDForResult)+"/matches", http.StatusSeeOther)
}

// POST /admin/matches/{id}/clear-result
func AdminMatchClearResult(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	matchID, _ := strconv.Atoi(r.PathValue("match_id"))
	ctx := context.Background()

	var compIDClear int
	var oldHome, oldAway *int
	var homeTeamID, awayTeamID int
	_ = db.Pool.QueryRow(ctx,
		`SELECT competition_id, home_team_id, away_team_id, home_score, away_score FROM matches WHERE id=$1`, matchID).
		Scan(&compIDClear, &homeTeamID, &awayTeamID, &oldHome, &oldAway)

	var homeName, awayName string
	_ = db.Pool.QueryRow(ctx, `SELECT COALESCE(display_name, name) FROM teams WHERE id=$1`, homeTeamID).Scan(&homeName)
	_ = db.Pool.QueryRow(ctx, `SELECT COALESCE(display_name, name) FROM teams WHERE id=$1`, awayTeamID).Scan(&awayName)

	_, _ = db.Pool.Exec(ctx,
		`UPDATE matches SET home_score=NULL, away_score=NULL, is_finished=false WHERE id=$1`, matchID)
	_, _ = db.Pool.Exec(ctx, `UPDATE tips SET points=NULL WHERE match_id=$1`, matchID)

	RecalculateStandings(compIDClear)

	oldHS := "–"
	if oldHome != nil && oldAway != nil {
		oldHS = strconv.Itoa(*oldHome) + ":" + strconv.Itoa(*oldAway)
	}
	desc := "Smazáno skóre " + homeName + " – " + awayName + ": " + oldHS + " → —"
	LogAction(&admin.ID, admin.Username, "match_score_clear", "match", &matchID, desc, nil, nil)

	http.Redirect(w, r, "/admin/competitions/"+strconv.Itoa(compIDClear)+"/matches", http.StatusSeeOther)
}

// POST /admin/matches/{id}/delete (AJAX)
func AdminMatchDelete(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		jsonError(w, "forbidden", http.StatusForbidden)
		return
	}
	matchID, _ := strconv.Atoi(r.PathValue("match_id"))
	ctx := context.Background()

	var isFinished bool
	var homeTeamID, awayTeamID int
	var matchDate *time.Time
	err := db.Pool.QueryRow(ctx,
		`SELECT is_finished, home_team_id, away_team_id, match_date FROM matches WHERE id=$1`, matchID).
		Scan(&isFinished, &homeTeamID, &awayTeamID, &matchDate)
	if err != nil {
		jsonError(w, "not_found", http.StatusNotFound)
		return
	}
	if isFinished {
		jsonError(w, "already_finished", http.StatusBadRequest)
		return
	}

	var homeName, awayName string
	_ = db.Pool.QueryRow(ctx, `SELECT COALESCE(display_name, name) FROM teams WHERE id=$1`, homeTeamID).Scan(&homeName)
	_ = db.Pool.QueryRow(ctx, `SELECT COALESCE(display_name, name) FROM teams WHERE id=$1`, awayTeamID).Scan(&awayName)

	matchDateStr := ""
	if matchDate != nil {
		matchDateStr = matchDate.Format("2006-01-02T15:04")
	}

	_, _ = db.Pool.Exec(ctx, `DELETE FROM tips WHERE match_id=$1`, matchID)
	_, _ = db.Pool.Exec(ctx, `DELETE FROM matches WHERE id=$1`, matchID)

	desc := "Smazán zápas " + homeName + " – " + awayName + " (" + matchDateStr + ")"
	oldVal := `{"home":"` + homeName + `","away":"` + awayName + `","match_date":"` + matchDateStr + `"}`
	LogAction(&admin.ID, admin.Username, "match_delete", "match", &matchID, desc, &oldVal, nil)

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// POST /admin/matches/{id}/set-date (AJAX)
func AdminMatchSetDate(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		jsonError(w, "forbidden", http.StatusForbidden)
		return
	}
	matchID, _ := strconv.Atoi(r.PathValue("match_id"))
	if err := r.ParseForm(); err != nil {
		return
	}
	matchDateStr := r.FormValue("match_date")

	ctx := context.Background()
	var isFinished bool
	var oldDate *time.Time
	var homeTeamID, awayTeamID int
	_ = db.Pool.QueryRow(ctx,
		`SELECT is_finished, match_date, home_team_id, away_team_id FROM matches WHERE id=$1`, matchID).
		Scan(&isFinished, &oldDate, &homeTeamID, &awayTeamID)

	if isFinished {
		jsonError(w, "already_finished", http.StatusBadRequest)
		return
	}

	var newDate *time.Time
	if matchDateStr != "" {
		t, err := time.ParseInLocation("2006-01-02T15:04", matchDateStr, pragueLocation)
		if err != nil {
			jsonError(w, "invalid_date", http.StatusBadRequest)
			return
		}
		newDate = &t
	}

	_, _ = db.Pool.Exec(ctx, `UPDATE matches SET match_date=$1 WHERE id=$2`, newDate, matchID)

	var homeName, awayName string
	_ = db.Pool.QueryRow(ctx, `SELECT COALESCE(display_name, name) FROM teams WHERE id=$1`, homeTeamID).Scan(&homeName)
	_ = db.Pool.QueryRow(ctx, `SELECT COALESCE(display_name, name) FROM teams WHERE id=$1`, awayTeamID).Scan(&awayName)

	oldDateStr := ""
	if oldDate != nil {
		oldDateStr = oldDate.Format("2006-01-02T15:04")
	}
	desc := "Změna data " + homeName + " – " + awayName + ": " + oldDateStr + " → " + matchDateStr
	LogAction(&admin.ID, admin.Username, "match_date_change", "match", &matchID, desc, nil, nil)

	displayStr := "—"
	isoStr := ""
	if newDate != nil {
		displayStr = newDate.Format("02.01.2006 15:04")
		isoStr = newDate.Format("2006-01-02T15:04")
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true,"display":"` + displayStr + `","iso":"` + isoStr + `"}`))
}

// POST /admin/tips/set-ajax (AJAX — admin nastaví tip za uživatele)
func AdminSetTip(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		jsonError(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		return
	}
	matchID, _ := strconv.Atoi(r.FormValue("match_id"))
	userID, _ := strconv.Atoi(r.FormValue("user_id"))
	homeScore, _ := strconv.Atoi(r.FormValue("home_score"))
	awayScore, _ := strconv.Atoi(r.FormValue("away_score"))

	ctx := context.Background()

	// Ověř zápas
	var matchHomeScore, matchAwayScore *int
	var homeTeamID, awayTeamID int
	err := db.Pool.QueryRow(ctx,
		`SELECT home_score, away_score, home_team_id, away_team_id FROM matches WHERE id=$1`, matchID).
		Scan(&matchHomeScore, &matchAwayScore, &homeTeamID, &awayTeamID)
	if err != nil {
		jsonError(w, "match_not_found", http.StatusNotFound)
		return
	}

	// Ověř uživatele
	var targetUsername string
	err = db.Pool.QueryRow(ctx, `SELECT username FROM users WHERE id=$1`, userID).Scan(&targetUsername)
	if err != nil {
		jsonError(w, "user_not_found", http.StatusNotFound)
		return
	}

	var homeName, awayName string
	_ = db.Pool.QueryRow(ctx, `SELECT COALESCE(display_name, name) FROM teams WHERE id=$1`, homeTeamID).Scan(&homeName)
	_ = db.Pool.QueryRow(ctx, `SELECT COALESCE(display_name, name) FROM teams WHERE id=$1`, awayTeamID).Scan(&awayName)

	// Upsert tipu
	var existingID int
	var oldHome, oldAway *int
	var oldPoints *int
	wasNew := true
	err = db.Pool.QueryRow(ctx,
		`SELECT id, home_score, away_score, points FROM tips WHERE user_id=$1 AND match_id=$2`,
		userID, matchID).Scan(&existingID, &oldHome, &oldAway, &oldPoints)
	if err == nil {
		wasNew = false
		_, _ = db.Pool.Exec(ctx,
			`UPDATE tips SET home_score=$1, away_score=$2, points=NULL WHERE id=$3`,
			homeScore, awayScore, existingID)
	} else {
		_ = db.Pool.QueryRow(ctx,
			`INSERT INTO tips (user_id, match_id, home_score, away_score, created_at)
			 VALUES ($1,$2,$3,$4,NOW()) RETURNING id`,
			userID, matchID, homeScore, awayScore).Scan(&existingID)
	}

	// Přepočítej body pokud je výsledek znám
	scored := matchHomeScore != nil && matchAwayScore != nil
	var pts *int
	if scored {
		tip := &models.Tip{HomeScore: homeScore, AwayScore: awayScore}
		p := tip.CalculatePoints(*matchHomeScore, *matchAwayScore)
		pts = &p
		_, _ = db.Pool.Exec(ctx, `UPDATE tips SET points=$1 WHERE id=$2`, p, existingID)
	}

	// Audit log
	oldVal := map[string]interface{}{"was_new": wasNew, "home_score": oldHome, "away_score": oldAway, "points": oldPoints}
	oldJSON, _ := json.Marshal(oldVal)
	oldStr := string(oldJSON)

	newValMap := map[string]interface{}{"home_score": homeScore, "away_score": awayScore, "points": pts, "user_id": userID}
	newJSON, _ := json.Marshal(newValMap)
	newStr := string(newJSON)

	ptsStr := ""
	if pts != nil {
		ptsStr = " (" + strconv.Itoa(*pts) + " b)"
	}
	desc := "Tip za " + targetUsername + ": " + strconv.Itoa(homeScore) + ":" + strconv.Itoa(awayScore) + ptsStr + " [" + homeName + " – " + awayName + "]"
	LogAction(&admin.ID, admin.Username, "admin_set_tip", "tip", &existingID, desc, &oldStr, &newStr)

	w.Header().Set("Content-Type", "application/json")
	ptsJSON := "null"
	if pts != nil {
		ptsJSON = strconv.Itoa(*pts)
	}
	_, _ = w.Write([]byte(`{"ok":true,"home":` + strconv.Itoa(homeScore) + `,"away":` + strconv.Itoa(awayScore) +
		`,"scored":` + strconv.FormatBool(scored) + `,"pts":` + ptsJSON + `}`))
}

// POST /admin/competitions/{competition_id}/users/{user_id}/remove-tips (AJAX)
// Smaže všechny tipy uživatele v dané soutěži → uživatel zmizí ze žebříčku.
func AdminRemoveUserTips(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		jsonError(w, "forbidden", http.StatusForbidden)
		return
	}
	compID, _ := strconv.Atoi(r.PathValue("competition_id"))
	userID, _ := strconv.Atoi(r.PathValue("user_id"))
	if compID == 0 || userID == 0 {
		jsonError(w, "bad_request", http.StatusBadRequest)
		return
	}
	ctx := context.Background()

	// Ověř že uživatel existuje
	var username string
	if err := db.Pool.QueryRow(ctx, `SELECT username FROM users WHERE id=$1`, userID).Scan(&username); err != nil {
		jsonError(w, "user_not_found", http.StatusNotFound)
		return
	}

	// Smaž všechny tipy tohoto uživatele ve všech zápasech dané soutěže
	res, err := db.Pool.Exec(ctx, `
		DELETE FROM tips
		WHERE user_id=$1
		  AND match_id IN (
		      SELECT id FROM matches WHERE competition_id=$2
		  )`, userID, compID)
	if err != nil {
		jsonError(w, "db_error", http.StatusInternalServerError)
		return
	}
	deleted := res.RowsAffected()

	desc := "Smazány tipy uživatele " + username + " v soutěži ID " + strconv.Itoa(compID) + " (" + strconv.Itoa(int(deleted)) + " tipů)"
	LogAction(&admin.ID, admin.Username, "user_tips_delete", "user", &userID, desc, nil, nil)

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true,"deleted":` + strconv.Itoa(int(deleted)) + `}`))
}

// GET /admin/unscored
func AdminUnscored(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		ctx := context.Background()
		matchRows, _ := db.Pool.Query(ctx,
			`SELECT m.id, m.competition_id, m.home_team_id, m.away_team_id,
			        m.home_score, m.away_score, m.match_date, m.is_finished,
			        ht.id, ht.name, ht.display_name,
			        at.id, at.name, at.display_name,
			        c.id, c.name
			   FROM matches m
			   JOIN competitions c ON c.id = m.competition_id
			   JOIN teams ht ON ht.id = m.home_team_id
			   JOIN teams at ON at.id = m.away_team_id
			  WHERE m.is_finished = false AND c.is_active = true
			  ORDER BY m.match_date ASC NULLS LAST`)
		type compEntry struct {
			Comp     *models.Competition
			Matches  []*models.Match
			DateFrom string
			DateTo   string
		}
		comps := map[int]*compEntry{}
		var compOrder []int
		for matchRows.Next() {
			m := &models.Match{HomeTeam: &models.Team{}, AwayTeam: &models.Team{}}
			comp := &models.Competition{}
			_ = matchRows.Scan(
				&m.ID, &m.CompetitionID, &m.HomeTeamID, &m.AwayTeamID,
				&m.HomeScore, &m.AwayScore, &m.MatchDate, &m.IsFinished,
				&m.HomeTeam.ID, &m.HomeTeam.Name, &m.HomeTeam.DisplayName,
				&m.AwayTeam.ID, &m.AwayTeam.Name, &m.AwayTeam.DisplayName,
				&comp.ID, &comp.Name)
			if _, ok := comps[comp.ID]; !ok {
				comps[comp.ID] = &compEntry{Comp: comp}
				compOrder = append(compOrder, comp.ID)
			}
			comps[comp.ID].Matches = append(comps[comp.ID].Matches, m)
		}
		matchRows.Close()

		RenderTemplate(w, r, tmpl, "unscored.html", TemplateData{
			"User":       admin,
			"Comps":      comps,
			"CompOrder":  compOrder,
		})
	}
}

// GET /admin/results
func AdminBulkResultsForm(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		ctx := context.Background()
		matchRows, _ := db.Pool.Query(ctx,
			`SELECT m.id, m.competition_id, m.home_team_id, m.away_team_id,
			        m.home_score, m.away_score, m.match_date, m.is_finished,
			        ht.id, ht.name, at.id, at.name,
			        c.id, c.name
			   FROM matches m
			   JOIN competitions c ON c.id = m.competition_id
			   JOIN teams ht ON ht.id = m.home_team_id
			   JOIN teams at ON at.id = m.away_team_id
			  WHERE m.is_finished = false AND c.is_active = true
			  ORDER BY m.match_date ASC NULLS LAST`)

		type compEntry2 struct {
			Comp    *models.Competition
			Matches []*models.Match
		}
		comps2 := map[int]*compEntry2{}
		var compOrder []int
		for matchRows.Next() {
			m := &models.Match{HomeTeam: &models.Team{}, AwayTeam: &models.Team{}}
			comp := &models.Competition{}
			_ = matchRows.Scan(
				&m.ID, &m.CompetitionID, &m.HomeTeamID, &m.AwayTeamID,
				&m.HomeScore, &m.AwayScore, &m.MatchDate, &m.IsFinished,
				&m.HomeTeam.ID, &m.HomeTeam.Name, &m.AwayTeam.ID, &m.AwayTeam.Name,
				&comp.ID, &comp.Name)
			if _, ok := comps2[comp.ID]; !ok {
				comps2[comp.ID] = &compEntry2{Comp: comp}
				compOrder = append(compOrder, comp.ID)
			}
			comps2[comp.ID].Matches = append(comps2[comp.ID].Matches, m)
		}
		matchRows.Close()
		comps := comps2

		flash := GetFlash(w, r)
		RenderTemplate(w, r, tmpl, "bulk_results.html", TemplateData{
			"User":       admin,
			"Comps":      comps,
			"CompOrder":  compOrder,
			"Flash":      flash,
		})
	}
}

// POST /admin/results
func AdminBulkResultsSubmit(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	ctx := context.Background()
	saved := 0
	skipped := 0
	affectedComps := map[int]bool{}

	for key := range r.Form {
		if !strings.HasPrefix(key, "home_") {
			continue
		}
		midStr := key[5:]
		mid, err := strconv.Atoi(midStr)
		if err != nil {
			continue
		}
		hVal := strings.TrimSpace(r.FormValue("home_" + midStr))
		aVal := strings.TrimSpace(r.FormValue("away_" + midStr))
		if hVal == "" || aVal == "" {
			skipped++
			continue
		}
		homeScore, err1 := strconv.Atoi(hVal)
		awayScore, err2 := strconv.Atoi(aVal)
		if err1 != nil || err2 != nil {
			skipped++
			continue
		}
		var isFinished bool
		var compID int
		err = db.Pool.QueryRow(ctx, `SELECT is_finished, competition_id FROM matches WHERE id=$1`, mid).
			Scan(&isFinished, &compID)
		if err != nil || isFinished {
			continue
		}
		_, _ = db.Pool.Exec(ctx,
			`UPDATE matches SET home_score=$1, away_score=$2, is_finished=true WHERE id=$3`,
			homeScore, awayScore, mid)
		RecalculateTips(ctx, mid, homeScore, awayScore)
		affectedComps[compID] = true
		saved++
	}

	for compID := range affectedComps {
		RecalculateStandings(compID)
	}

	msg := "Uloženo " + strconv.Itoa(saved) + " výsledků."
	if skipped > 0 {
		msg += " (" + strconv.Itoa(skipped) + " nevyplněných přeskočeno)"
	}
	middleware.SetFlash(w, r, "ok", msg)
	http.Redirect(w, r, "/admin/results", http.StatusSeeOther)
}

// GET /admin/api/unscored-count (AJAX badge)
func AdminUnscoredCount(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0}`))
		return
	}
	ctx := context.Background()
	var count int
	_ = db.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM matches m
		   JOIN competitions c ON c.id = m.competition_id
		  WHERE m.is_finished = false AND c.is_active = true`).Scan(&count)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"count":` + strconv.Itoa(count) + `}`))
}

// POST /admin/competitions/{id}/add-match (AJAX)
func AdminQuickAddMatchAjax(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		jsonError(w, "forbidden", http.StatusForbidden)
		return
	}
	compID, _ := strconv.Atoi(r.PathValue("competition_id"))
	if err := r.ParseForm(); err != nil {
		return
	}
	homeTeamID, _ := strconv.Atoi(r.FormValue("home_team_id"))
	awayTeamID, _ := strconv.Atoi(r.FormValue("away_team_id"))
	matchDateStr := r.FormValue("match_date")

	ctx := context.Background()

	var matchDate *time.Time
	if matchDateStr != "" {
		t, err := time.ParseInLocation("2006-01-02T15:04", matchDateStr, pragueLocation)
		if err == nil {
			matchDate = &t
		}
	}

	var newMatchID int
	_ = db.Pool.QueryRow(ctx,
		`INSERT INTO matches (competition_id, home_team_id, away_team_id, match_date, is_finished)
		 VALUES ($1,$2,$3,$4,false) RETURNING id`,
		compID, homeTeamID, awayTeamID, matchDate).Scan(&newMatchID)

	newVal := `{"competition_id":` + strconv.Itoa(compID) + `,"home":` + strconv.Itoa(homeTeamID) + `,"away":` + strconv.Itoa(awayTeamID) + `}`
	LogAction(&admin.ID, admin.Username, "match_add_quick", "match", &newMatchID,
		"Rychlé přidání zápasu", nil, &newVal)

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// POST /admin/matches/{match_id}/notify-now (AJAX)
// Resetuje notify_sent a okamžitě spustí notifikaci pro daný zápas.
func AdminMatchNotifyNow(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		jsonError(w, "forbidden", http.StatusForbidden)
		return
	}
	matchID, _ := strconv.Atoi(r.PathValue("match_id"))
	ctx := context.Background()

	// Zkontroluj zda zápas existuje
	var exists bool
	err := db.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM matches WHERE id=$1)`, matchID).Scan(&exists)
	if err != nil || !exists {
		jsonError(w, "not_found", http.StatusNotFound)
		return
	}

	// Reset notify_sent → spustí notifikaci
	_, _ = db.Pool.Exec(ctx, `UPDATE matches SET notify_sent = false WHERE id = $1`, matchID)

	// Spusť notifikaci pro tento zápas přímo v goroutině
	go sendMatchNotificationForID(matchID)

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}
