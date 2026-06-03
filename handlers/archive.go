// handlers/archive.go — Tipovačka 3.0
// Archiv soutěží — kola odstraněna, zápasy přímo pod soutěží.
package handlers

import (
	"context"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"tipovacka/db"
	"tipovacka/models"
)

// GET /archive
func ArchiveIndex(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := RequireApproved(w, r)
		if user == nil {
			return
		}
		ctx := context.Background()

		rows, _ := db.Pool.Query(ctx,
			`SELECT id, name, season, is_active, sport, sort_order FROM competitions WHERE COALESCE(is_hidden,false)=false ORDER BY id DESC`)
		var all []*models.Competition
		for rows.Next() {
			c := &models.Competition{}
			_ = rows.Scan(&c.ID, &c.Name, &c.Season, &c.IsActive, &c.Sport, &c.SortOrder)
			all = append(all, c)
		}
		rows.Close()

		var active, hockey, football []*models.Competition
		hockeyKW := []string{"hokej", "nhl"}
		for _, c := range all {
			if c.IsActive {
				active = append(active, c)
				continue
			}
			nameLower := strings.ToLower(c.Name)
			isHockey := false
			for _, kw := range hockeyKW {
				if strings.Contains(nameLower, kw) {
					isHockey = true
					break
				}
			}
			if isHockey {
				hockey = append(hockey, c)
			} else {
				football = append(football, c)
			}
		}

		RenderTemplate(w, r, tmpl, "archive_index.html", TemplateData{
			"User":     user,
			"Active":   active,
			"Hockey":   hockey,
			"Football": football,
		})
	}
}

// GET /archive/competition/{competition_id}
func ArchiveCompetition(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := RequireApproved(w, r)
		if user == nil {
			return
		}
		compID, _ := strconv.Atoi(r.PathValue("competition_id"))
		ctx := context.Background()

		comp := &models.Competition{}
		err := db.Pool.QueryRow(ctx,
			`SELECT id, name, season, is_active, sport, sort_order FROM competitions WHERE id=$1`, compID).
			Scan(&comp.ID, &comp.Name, &comp.Season, &comp.IsActive, &comp.Sport, &comp.SortOrder)
		if err != nil {
			http.Redirect(w, r, "/archive", http.StatusSeeOther)
			return
		}

		if !comp.IsActive {
			// Neaktivní — matice žebříčku
			var matches []*models.Match
			matchRows, _ := db.Pool.Query(ctx,
				`SELECT m.id, m.competition_id, m.home_team_id, m.away_team_id,
				        m.home_score, m.away_score, m.is_finished, m.match_date,
				        ht.name, at.name
				   FROM matches m
				   JOIN teams ht ON ht.id = m.home_team_id
				   JOIN teams at ON at.id = m.away_team_id
				  WHERE m.competition_id = $1
				  ORDER BY m.match_date`, compID)
			for matchRows.Next() {
				m := &models.Match{}
				_ = matchRows.Scan(&m.ID, &m.CompetitionID, &m.HomeTeamID, &m.AwayTeamID,
					&m.HomeScore, &m.AwayScore, &m.IsFinished, &m.MatchDate,
					&m.HomeTeamName, &m.AwayTeamName)
				matches = append(matches, m)
			}
			matchRows.Close()

			matchIDs := make([]int, len(matches))
			for i, m := range matches {
				matchIDs[i] = m.ID
			}

			// Tips matrix: user_id -> match_id -> Tip
			tipsMatrix := map[int]map[int]*models.Tip{}
			if len(matchIDs) > 0 {
				tipRows, _ := db.Pool.Query(ctx,
					`SELECT id, user_id, match_id, home_score, away_score, points
					   FROM tips WHERE match_id = ANY($1)`, matchIDs)
				for tipRows.Next() {
					t := &models.Tip{}
					_ = tipRows.Scan(&t.ID, &t.UserID, &t.MatchID, &t.HomeScore, &t.AwayScore, &t.Points)
					if tipsMatrix[t.UserID] == nil {
						tipsMatrix[t.UserID] = map[int]*models.Tip{}
					}
					tipsMatrix[t.UserID][t.MatchID] = t
				}
				tipRows.Close()
			}

			type UserRow struct {
				User       *models.User
				Total      int
				Extra      int
				GrandTotal int
				Exact      int
				Partial    int
				Missed     int
				Place      int
			}

			var userRows []UserRow

			cachedRows, _ := db.Pool.Query(ctx,
				`SELECT user_id, tip_points, extra_points, grand_total, exact_count, partial_count, miss_count
				   FROM competition_standings WHERE competition_id=$1`, compID)
			var hasCached bool
			type cachedRow struct {
				UserID, TipPts, ExtraPts, GrandTotal, Exact, Partial, Miss int
			}
			var cached []cachedRow
			for cachedRows.Next() {
				hasCached = true
				var cr cachedRow
				_ = cachedRows.Scan(&cr.UserID, &cr.TipPts, &cr.ExtraPts, &cr.GrandTotal, &cr.Exact, &cr.Partial, &cr.Miss)
				cached = append(cached, cr)
			}
			cachedRows.Close()

			if hasCached {
				userIDs := make([]int, len(cached))
				for i, cr := range cached {
					userIDs[i] = cr.UserID
				}
				usersByID := map[int]*models.User{}
				if len(userIDs) > 0 {
					urows, _ := db.Pool.Query(ctx,
						`SELECT id, username FROM users WHERE id = ANY($1)`, userIDs)
					for urows.Next() {
						u := &models.User{}
						_ = urows.Scan(&u.ID, &u.Username)
						u.IsApproved = true
						usersByID[u.ID] = u
					}
					urows.Close()
				}
				for _, cr := range cached {
					u, ok := usersByID[cr.UserID]
					if !ok {
						continue
					}
					userRows = append(userRows, UserRow{
						User:       u,
						Total:      cr.TipPts,
						Extra:      cr.ExtraPts,
						GrandTotal: cr.GrandTotal,
						Exact:      cr.Exact,
						Partial:    cr.Partial,
						Missed:     cr.Miss,
					})
				}
			} else {
				userIDs := make([]int, 0, len(tipsMatrix))
				for uid := range tipsMatrix {
					userIDs = append(userIDs, uid)
				}
				usersByID := map[int]*models.User{}
				if len(userIDs) > 0 {
					urows, _ := db.Pool.Query(ctx,
						`SELECT id, username FROM users WHERE id = ANY($1)`, userIDs)
					for urows.Next() {
						u := &models.User{}
						_ = urows.Scan(&u.ID, &u.Username)
						u.IsApproved = true
						usersByID[u.ID] = u
					}
					urows.Close()
				}

				extraPtsByUser := map[int]int{}
				extraRows, _ := db.Pool.Query(ctx,
					`SELECT ea.user_id, COALESCE(SUM(ea.points),0)
					   FROM extra_answers ea
					   JOIN extra_questions eq ON eq.id = ea.question_id
					  WHERE eq.competition_id=$1 AND ea.points IS NOT NULL
					  GROUP BY ea.user_id`, compID)
				for extraRows.Next() {
					var uid, pts int
					_ = extraRows.Scan(&uid, &pts)
					extraPtsByUser[uid] = pts
				}
				extraRows.Close()

				for uid, utips := range tipsMatrix {
					u, ok := usersByID[uid]
					if !ok {
						continue
					}
					var tipTotal, exact, partial, missed int
					for _, t := range utips {
						if t.Points != nil {
							tipTotal += *t.Points
							switch *t.Points {
							case 3:
								exact++
							case 1:
								partial++
							case 0:
								missed++
							}
						}
					}
					extra := extraPtsByUser[uid]
					userRows = append(userRows, UserRow{
						User:       u,
						Total:      tipTotal,
						Extra:      extra,
						GrandTotal: tipTotal + extra,
						Exact:      exact,
						Partial:    partial,
						Missed:     missed,
					})
				}
			}

			for i := 0; i < len(userRows)-1; i++ {
				for j := i + 1; j < len(userRows); j++ {
					ai := userRows[i]
					aj := userRows[j]
					if aj.GrandTotal > ai.GrandTotal || (aj.GrandTotal == ai.GrandTotal && aj.Exact > ai.Exact) {
						userRows[i], userRows[j] = userRows[j], userRows[i]
					}
				}
			}
			place := 1
			for i := range userRows {
				if i > 0 &&
					userRows[i].GrandTotal == userRows[i-1].GrandTotal &&
					userRows[i].Exact == userRows[i-1].Exact {
					userRows[i].Place = userRows[i-1].Place
				} else {
					userRows[i].Place = place
				}
				place++
			}

			hasExtra := false
			for _, ur := range userRows {
				if ur.Extra > 0 {
					hasExtra = true
					break
				}
			}

			RenderTemplate(w, r, tmpl, "archive_competition_leaderboard.html", TemplateData{
				"User":       user,
				"Comp":       comp,
				"UserRows":   userRows,
				"Matches":    matches,
				"TipsMatrix": tipsMatrix,
				"HasExtra":   hasExtra,
			})
			return
		}

		// Aktivní soutěž — zobraz tipy uživatele jako flat list
		var matchList []*models.Match
		matchRows2, _ := db.Pool.Query(ctx,
			`SELECT m.id, m.competition_id, m.home_team_id, m.away_team_id,
			        m.home_score, m.away_score, m.is_finished, m.match_date,
			        ht.name, at.name
			   FROM matches m
			   JOIN teams ht ON ht.id = m.home_team_id
			   JOIN teams at ON at.id = m.away_team_id
			  WHERE m.competition_id = $1 ORDER BY m.match_date`, compID)
		for matchRows2.Next() {
			m := &models.Match{}
			_ = matchRows2.Scan(&m.ID, &m.CompetitionID, &m.HomeTeamID, &m.AwayTeamID,
				&m.HomeScore, &m.AwayScore, &m.IsFinished, &m.MatchDate,
				&m.HomeTeamName, &m.AwayTeamName)
			matchList = append(matchList, m)
		}
		matchRows2.Close()

		matchIDSlice := make([]int, len(matchList))
		for i, m := range matchList {
			matchIDSlice[i] = m.ID
		}
		userTips := map[int]*models.Tip{}
		if len(matchIDSlice) > 0 {
			tipRows, _ := db.Pool.Query(ctx,
				`SELECT id, user_id, match_id, home_score, away_score, points
				   FROM tips WHERE user_id=$1 AND match_id = ANY($2)`, user.ID, matchIDSlice)
			for tipRows.Next() {
				t := &models.Tip{}
				_ = tipRows.Scan(&t.ID, &t.UserID, &t.MatchID, &t.HomeScore, &t.AwayScore, &t.Points)
				userTips[t.MatchID] = t
			}
			tipRows.Close()
		}

		type MatchRow struct {
			Match  *models.Match
			Tip    *models.Tip
			Points *int
		}
		var rows []MatchRow
		overallTotal := 0
		for _, m := range matchList {
			t := userTips[m.ID]
			var pts *int
			if t != nil {
				pts = t.Points
			}
			if pts != nil {
				overallTotal += *pts
			}
			rows = append(rows, MatchRow{Match: m, Tip: t, Points: pts})
		}

		RenderTemplate(w, r, tmpl, "archive_competition.html", TemplateData{
			"User":         user,
			"Comp":         comp,
			"Rows":         rows,
			"OverallTotal": overallTotal,
		})
	}
}

// GET /archive/round/{round_id} — zpětná kompatibilita, redirect na archiv
func ArchiveRoundRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/archive", http.StatusMovedPermanently)
}
