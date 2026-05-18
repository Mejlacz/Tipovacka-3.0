// handlers/leaderboard.go — Tipovačka 2.0
// Maticový žebříček: řádky = hráči, sloupce = zápasy, buňky = tipy.
package handlers

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"tipovacka/db"
	"tipovacka/middleware"
	"tipovacka/models"
)

var teamPrefixes = map[string]bool{
	"FC": true, "FK": true, "SK": true, "AC": true,
	"MFK": true, "1.": true, "SFC": true, "TJ": true,
	"FBC": true, "HC": true,
}

func abbrev(name string) string {
	for _, word := range strings.Fields(name) {
		clean := strings.Trim(word, ".,")
		if !teamPrefixes[strings.ToUpper(clean)] && len([]rune(clean)) >= 2 {
			r := []rune(clean)
			if len(r) > 4 {
				r = r[:4]
			}
			return strings.ToUpper(string(r))
		}
	}
	r := []rune(name)
	if len(r) > 4 {
		r = r[:4]
	}
	return strings.ToUpper(string(r))
}

func canSeeHidden(u *models.User) bool {
	if u.IsOwner {
		return true
	}
	ctx := context.Background()
	var count int
	_ = db.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM group_memberships gm
		  JOIN user_groups ug ON ug.id = gm.group_id
		 WHERE gm.user_id = $1 AND ug.can_see_hidden = true`, u.ID).Scan(&count)
	return count > 0
}

func shouldShow(target, current *models.User, seeHidden bool) bool {
	if target.IsBlocked || target.IsInactive {
		return false
	}
	if !target.IsHidden {
		return true
	}
	if seeHidden {
		return true
	}
	return target.ID == current.ID
}

// GetFlash vrátí flash ze session.
func GetFlash(w http.ResponseWriter, r *http.Request) map[string]string {
	return middleware.GetFlash(w, r)
}

// loadAllUsers načte všechny uživatele z DB pomocí schema-aware selectu.
// Přizpůsobuje se dostupným sloupcům (dynamická detekce při startu).
func loadAllUsers(ctx context.Context) []*models.User {
	cols, _ := buildUserSelect()
	rows, err := db.Pool.Query(ctx, "SELECT "+cols+" FROM users ORDER BY username")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var users []*models.User
	for rows.Next() {
		u := &models.User{}
		if err := scanUser(u, rows); err == nil {
			users = append(users, u)
		}
	}
	return users
}

// GET /leaderboard
func Leaderboard(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := RequireApproved(w, r)
		if u == nil {
			return
		}

		flash := GetFlash(w, r)

		compIDStr := r.URL.Query().Get("competition_id")
		var compID int
		if compIDStr != "" {
			compID, _ = strconv.Atoi(compIDStr)
		}

		ctx := context.Background()

		// Načti soutěže
		compRows, _ := db.Pool.Query(ctx,
			`SELECT c.id, c.name, c.season, c.is_active, c.sport, c.sort_order
			   FROM competitions c
			  WHERE c.is_active = true AND COALESCE(c.is_hidden,false)=false
			  ORDER BY c.sort_order ASC NULLS LAST, c.id DESC`)
		var competitions []*models.Competition
		for compRows.Next() {
			c := &models.Competition{}
			_ = compRows.Scan(&c.ID, &c.Name, &c.Season, &c.IsActive, &c.Sport, &c.SortOrder)
			competitions = append(competitions, c)
		}
		compRows.Close()

		if compID == 0 && len(competitions) > 0 {
			compID = competitions[0].ID
		}

		now := NowPrague()

		// Extra reveal: zjisti jestli jsou extra tipy ostatních viditelné
		// Reveal nastane POUZE pokud admin explicitně nastavil extra_reveal_at ("Odkrýt všem")
		extraRevealed := false
		if compID > 0 {
			var extraRevealAt *time.Time
			_ = db.Pool.QueryRow(ctx,
				`SELECT extra_reveal_at FROM competitions WHERE id=$1`, compID).
				Scan(&extraRevealAt)
			if extraRevealAt != nil {
				extraRevealed = now.After(*extraRevealAt)
			}
		}

		// Zápasy pro vybranou soutěž
		var matches []*models.Match
		var compRounds []*models.Round
		var compTeams []*models.Team
		nextRoundName := "Kolo 1"

		if compID > 0 {
			matchRows, _ := db.Pool.Query(ctx,
				`SELECT m.id, m.round_id, m.home_team_id, m.away_team_id,
				        m.home_score, m.away_score, m.match_date, m.is_finished,
				        COALESCE(m.notify_sent, false),
				        ht.id, ht.name, ht.display_name, ht.logo_url,
				        at.id, at.name, at.display_name, at.logo_url,
				        r.id, r.name, r.deadline, r.competition_id
				   FROM matches m
				   JOIN rounds r ON r.id = m.round_id
				   JOIN teams ht ON ht.id = m.home_team_id
				   JOIN teams at ON at.id = m.away_team_id
				  WHERE r.competition_id = $1
				  ORDER BY m.match_date DESC NULLS LAST`, compID)
			for matchRows.Next() {
				m := &models.Match{HomeTeam: &models.Team{}, AwayTeam: &models.Team{}, Round: &models.Round{}}
				_ = matchRows.Scan(
					&m.ID, &m.RoundID, &m.HomeTeamID, &m.AwayTeamID,
					&m.HomeScore, &m.AwayScore, &m.MatchDate, &m.IsFinished,
					&m.NotifySent,
					&m.HomeTeam.ID, &m.HomeTeam.Name, &m.HomeTeam.DisplayName, &m.HomeTeam.LogoURL,
					&m.AwayTeam.ID, &m.AwayTeam.Name, &m.AwayTeam.DisplayName, &m.AwayTeam.LogoURL,
					&m.Round.ID, &m.Round.Name, &m.Round.Deadline, &m.Round.CompetitionID)
				matches = append(matches, m)
			}
			matchRows.Close()

			if u.IsAdmin {
				rndRows, _ := db.Pool.Query(ctx,
					`SELECT id, competition_id, name, deadline, is_active FROM rounds
					  WHERE competition_id = $1 AND is_active = true ORDER BY id DESC`, compID)
				for rndRows.Next() {
					rnd := &models.Round{}
					_ = rndRows.Scan(&rnd.ID, &rnd.CompetitionID, &rnd.Name, &rnd.Deadline, &rnd.IsActive)
					compRounds = append(compRounds, rnd)
				}
				rndRows.Close()

				teamRows, _ := db.Pool.Query(ctx,
					`SELECT t.id, t.name, t.sport, t.alias, t.display_name, t.logo_url, t.category, t.competition_id
					   FROM teams t
					   JOIN competition_teams ct ON ct.team_id = t.id
					  WHERE ct.competition_id = $1
					  ORDER BY t.name`, compID)
				for teamRows.Next() {
					t := &models.Team{}
					_ = teamRows.Scan(&t.ID, &t.Name, &t.Sport, &t.Alias, &t.DisplayName, &t.LogoURL, &t.Category, &t.CompetitionID)
					compTeams = append(compTeams, t)
				}
				teamRows.Close()
			}

			// Navrhni název pro nové kolo
			if len(compRounds) > 0 {
				lastName := compRounds[0].Name
				re := regexp.MustCompile(`\d+`)
				loc := re.FindStringIndex(lastName)
				if loc != nil {
					num, _ := strconv.Atoi(lastName[loc[0]:loc[1]])
					nextRoundName = lastName[:loc[0]] + strconv.Itoa(num+1) + lastName[loc[1]:]
				} else {
					nextRoundName = "Kolo " + strconv.Itoa(len(compRounds)+1)
				}
			}
		}

		// Match IDs kde zápas už začal
		startedMatchIDs := map[int]bool{}
		matchIDs := make([]int, 0, len(matches))
		for _, m := range matches {
			matchIDs = append(matchIDs, m.ID)
			if m.IsFinished ||
				(m.MatchDate != nil && !m.MatchDate.After(now)) ||
				(m.HomeScore != nil && m.AwayScore != nil) {
				startedMatchIDs[m.ID] = true
			}
		}

		// Všechny tipy
		tipsByUser := map[int]map[int]*models.Tip{}
		if len(matchIDs) > 0 {
			tipRows, _ := db.Pool.Query(ctx,
				`SELECT id, user_id, match_id, home_score, away_score, points, created_at
				   FROM tips WHERE match_id = ANY($1)`, matchIDs)
			for tipRows.Next() {
				t := &models.Tip{}
				_ = tipRows.Scan(&t.ID, &t.UserID, &t.MatchID, &t.HomeScore, &t.AwayScore, &t.Points, &t.CreatedAt)
				if tipsByUser[t.UserID] == nil {
					tipsByUser[t.UserID] = map[int]*models.Tip{}
				}
				tipsByUser[t.UserID][t.MatchID] = t
			}
			tipRows.Close()
		}

		// Extra otázky
		var extraQuestions []*models.ExtraQuestion
		extraAnswersMatrix := map[int]map[int]*models.ExtraAnswer{}
		if compID > 0 {
			qRows, _ := db.Pool.Query(ctx,
				`SELECT id, competition_id, order_num, text, max_points, correct_answer, is_closed
				   FROM extra_questions WHERE competition_id = $1
				  ORDER BY order_num, id`, compID)
			for qRows.Next() {
				q := &models.ExtraQuestion{}
				_ = qRows.Scan(&q.ID, &q.CompetitionID, &q.OrderNum, &q.Text, &q.MaxPoints, &q.CorrectAnswer, &q.IsClosed)
				extraQuestions = append(extraQuestions, q)
			}
			qRows.Close()

			if len(extraQuestions) > 0 {
				qIDs := make([]int, len(extraQuestions))
				for i, q := range extraQuestions {
					qIDs[i] = q.ID
				}
				ansRows, _ := db.Pool.Query(ctx,
					`SELECT id, question_id, user_id, answer, points, created_at
					   FROM extra_answers WHERE question_id = ANY($1)`, qIDs)
				for ansRows.Next() {
					ea := &models.ExtraAnswer{}
					_ = ansRows.Scan(&ea.ID, &ea.QuestionID, &ea.UserID, &ea.Answer, &ea.Points, &ea.CreatedAt)
					if extraAnswersMatrix[ea.UserID] == nil {
						extraAnswersMatrix[ea.UserID] = map[int]*models.ExtraAnswer{}
					}
					extraAnswersMatrix[ea.UserID][ea.QuestionID] = ea
				}
				ansRows.Close()
			}
		}

		// Extra cols
		var extraCols []models.ExtraCol
		for _, q := range extraQuestions {
			parts := strings.Split(q.Text, "|~~|")
			var corrParts []string
			if q.CorrectAnswer != nil {
				corrParts = strings.Split(*q.CorrectAnswer, "|~~|")
			}
			for i, part := range parts {
				var correctAnswers []string
				if i < len(corrParts) {
					for _, ca := range strings.Split(corrParts[i], "|") {
						ca = strings.TrimSpace(ca)
						if ca != "" {
							correctAnswers = append(correctAnswers, ca)
						}
					}
				}
				extraCols = append(extraCols, models.ExtraCol{
					QID:            q.ID,
					SubIndex:       i,
					ColKey:         strconv.Itoa(q.ID) + "_" + strconv.Itoa(i),
					SubText:        strings.TrimSpace(part),
					MaxPoints:      q.MaxPoints,
					IsLast:         i == len(parts)-1,
					CorrectAnswers: correctAnswers,
					IsClosed:       q.IsClosed,
				})
			}
		}

		// expanded_ea[userID][colKey] = answerText
		expandedEA := map[int]map[string]string{}
		for uid, qaMap := range extraAnswersMatrix {
			for qid, ea := range qaMap {
				ansParts := strings.Split(ea.Answer, "|~~|")
				for i, part := range ansParts {
					ck := strconv.Itoa(qid) + "_" + strconv.Itoa(i)
					if expandedEA[uid] == nil {
						expandedEA[uid] = map[string]string{}
					}
					expandedEA[uid][ck] = strings.TrimSpace(part)
				}
			}
		}

		// Extra pts matrix
		extraPtsMatrix := map[int]map[int]*int{}
		for uid, qaMap := range extraAnswersMatrix {
			for qid, ea := range qaMap {
				if extraPtsMatrix[uid] == nil {
					extraPtsMatrix[uid] = map[int]*int{}
				}
				extraPtsMatrix[uid][qid] = ea.Points
			}
		}

		// Načti všechny uživatele
		allUsers := loadAllUsers(ctx)
		seeHidden := canSeeHidden(u)

		matchIDSet := map[int]bool{}
		for _, id := range matchIDs {
			matchIDSet[id] = true
		}

		var userRows []*models.UserRow
		for _, usr := range allUsers {
			if !shouldShow(usr, u, seeHidden) {
				continue
			}
			userTips := tipsByUser[usr.ID]
			if len(userTips) == 0 {
				continue
			}
			total, exact, winner, miss, tipCount := 0, 0, 0, 0, 0
			for mid, tip := range userTips {
				if matchIDSet[mid] {
					tipCount++
				}
				pts := 0
				if tip.Points != nil {
					pts = *tip.Points
				}
				total += pts
				switch {
				case pts == 3:
					exact++
				case pts == 1:
					winner++
				case tip.Points != nil && *tip.Points == 0 && startedMatchIDs[mid]:
					miss++
				}
			}
			// Extra body
			extraPts := 0
			for _, q := range extraQuestions {
				if eaMap, ok := extraAnswersMatrix[usr.ID]; ok {
					if ea, ok := eaMap[q.ID]; ok && ea.Points != nil {
						extraPts += *ea.Points
					}
				}
			}
			finished := exact + winner + miss
			var accuracy *int
			if finished > 0 {
				acc := exact * 100 / finished
				accuracy = &acc
			}
			row := &models.UserRow{
				User:             usr,
				Total:            total,
				Extra:            extraPts,
				GrandTotal:       total + extraPts,
				Exact:            exact,
				Winner:           winner,
				Miss:             miss,
				TipCount:         tipCount,
				FinishedTipCount: finished,
				Accuracy:         accuracy,
				TipRatio:         strconv.Itoa(tipCount) + "/" + strconv.Itoa(len(matchIDs)),
			}
			userRows = append(userRows, row)
		}

		sort.Slice(userRows, func(i, j int) bool {
			if userRows[i].GrandTotal != userRows[j].GrandTotal {
				return userRows[i].GrandTotal > userRows[j].GrandTotal
			}
			return userRows[i].Exact > userRows[j].Exact
		})

		place := 1
		for i, row := range userRows {
			if i > 0 &&
				row.GrandTotal == userRows[i-1].GrandTotal &&
				row.Exact == userRows[i-1].Exact {
				row.Place = userRows[i-1].Place
			} else {
				row.Place = place
			}
			place++
		}

		// Trend
		finishedForTrend := []*models.Match{}
		for _, m := range matches {
			if m.HomeScore != nil {
				finishedForTrend = append(finishedForTrend, m)
			}
		}
		if len(finishedForTrend) > 0 {
			byRound := map[int][]*models.Match{}
			for _, m := range finishedForTrend {
				byRound[m.RoundID] = append(byRound[m.RoundID], m)
			}
			// Trend má smysl jen pokud jsou dokončené zápasy ve více kolech —
			// jinak jsou všechny tipy v "posledním kole" a prevTotal=0 pro všechny,
			// což by zobrazovalo zavádějící čísla (↓11, ↓13…).
			if len(byRound) >= 2 {
				latestRoundID := -1
				var latestDate time.Time
				for rid, ms := range byRound {
					for _, m := range ms {
						if m.MatchDate != nil && m.MatchDate.After(latestDate) {
							latestDate = *m.MatchDate
							latestRoundID = rid
						}
					}
				}
				latestIDs := map[int]bool{}
				for _, m := range byRound[latestRoundID] {
					latestIDs[m.ID] = true
				}

				prevTotals := map[int][2]int{}
				for _, row := range userRows {
					uid := row.User.ID
					prevTotal, prevExact := 0, 0
					for mid, t := range tipsByUser[uid] {
						if !latestIDs[mid] && t.Points != nil {
							prevTotal += *t.Points
							if *t.Points == 3 {
								prevExact++
							}
						}
					}
					userExtra := 0
					if eaMap, ok := extraAnswersMatrix[uid]; ok {
						for _, ea := range eaMap {
							if ea.Points != nil {
								userExtra += *ea.Points
							}
						}
					}
					prevTotals[uid] = [2]int{prevTotal + userExtra, prevExact}
				}

				type prevEntry struct{ uid, gt, exact int }
				var prevSorted []prevEntry
				for uid, v := range prevTotals {
					prevSorted = append(prevSorted, prevEntry{uid, v[0], v[1]})
				}
				sort.Slice(prevSorted, func(i, j int) bool {
					if prevSorted[i].gt != prevSorted[j].gt {
						return prevSorted[i].gt > prevSorted[j].gt
					}
					return prevSorted[i].exact > prevSorted[j].exact
				})
				prevPlaceMap := map[int]int{}
				p := 1
				for i, e := range prevSorted {
					if i > 0 && e.gt == prevSorted[i-1].gt && e.exact == prevSorted[i-1].exact {
						prevPlaceMap[e.uid] = prevPlaceMap[prevSorted[i-1].uid]
					} else {
						prevPlaceMap[e.uid] = p
					}
					p++
				}
				for _, row := range userRows {
					if prev, ok := prevPlaceMap[row.User.ID]; ok {
						row.Trend = prev - row.Place
					}
				}
			}
		}

		// Streak: consecutive exact tips from most recent backwards
		if len(matchIDs) > 0 {
			type mdate struct {
				id int
				d  time.Time
			}
			var ordered []mdate
			for _, m := range matches {
				if m.HomeScore != nil && m.MatchDate != nil {
					ordered = append(ordered, mdate{m.ID, *m.MatchDate})
				}
			}
			sort.Slice(ordered, func(i, j int) bool { return ordered[i].d.After(ordered[j].d) })
			for _, row := range userRows {
				streak := 0
				for _, md := range ordered {
					tip := tipsByUser[row.User.ID][md.id]
					if tip == nil || tip.Points == nil {
						break
					}
					if *tip.Points == 3 {
						streak++
					} else {
						break
					}
				}
				row.Streak = streak
			}
		}

		RenderTemplate(w, r, tmpl, "leaderboard.html", TemplateData{
			"User":                  u,
			"UserRows":              userRows,
			"Matches":               matches,
			"TipsByUser":            tipsByUser,
			"StartedMatchIDs":       startedMatchIDs,
			"Competitions":          competitions,
			"SelectedCompetitionID": compID,
			"Abbrev":                abbrev,
			"ExtraQuestions":        extraQuestions,
			"ExtraAnswersMatrix":    extraAnswersMatrix,
			"ExtraCols":             extraCols,
			"ExpandedEA":            expandedEA,
			"ExtraPtsMatrix":        extraPtsMatrix,
			"ExtraRevealed":         extraRevealed,
			"Flash":                 flash,
			"CompRounds":            compRounds,
			"CompTeams":             compTeams,
			"NextRoundName":         nextRoundName,
		})
	}
}

// ─── GET /leaderboard/chart-data ─────────────────────────────────────────────
// Vrátí JSON s kumulativními body hráčů po jednotlivých zápasech (pro Chart.js).
// X-osa = zápasy seřazené podle data; Y-osa = součet bodů do daného zápasu.

func LeaderboardChartData(tmpl *template.Template) http.HandlerFunc {
	type matchInfo struct {
		ID    int
		Label string // "DD.MM."
	}
	type dataset struct {
		Label string `json:"label"`
		IsMe  bool   `json:"isMe"`
		Data  []int  `json:"data"`
	}
	type chartResp struct {
		Labels   []string  `json:"labels"`
		Datasets []dataset `json:"datasets"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		u := RequireLogin(w, r)
		if u == nil {
			return
		}
		w.Header().Set("Content-Type", "application/json")

		compID, err := strconv.Atoi(r.URL.Query().Get("competition_id"))
		if err != nil || compID == 0 {
			w.Write([]byte(`{"labels":[],"datasets":[]}`))
			return
		}

		ctx := context.Background()

		// Všechny dokončené zápasy seřazené podle data
		var matchList []matchInfo
		mrows, _ := db.Pool.Query(ctx,
			`SELECT m.id,
			        COALESCE(TO_CHAR(m.match_date, 'DD.MM.'), '#' || m.id::text)
			   FROM matches m
			   JOIN rounds r ON r.id = m.round_id
			  WHERE r.competition_id = $1 AND m.is_finished = true
			  ORDER BY m.match_date ASC NULLS LAST, m.id ASC`, compID)
		for mrows.Next() {
			var mi matchInfo
			_ = mrows.Scan(&mi.ID, &mi.Label)
			matchList = append(matchList, mi)
		}
		mrows.Close()

		if len(matchList) == 0 {
			w.Write([]byte(`{"labels":[],"datasets":[]}`))
			return
		}

		matchIDs := make([]int, len(matchList))
		matchIdx := map[int]int{}
		for i, mi := range matchList {
			matchIDs[i] = mi.ID
			matchIdx[mi.ID] = i
		}

		// Tipy s body pro tyto zápasy
		type tipRow struct {
			MatchID int
			UserID  int
			Pts     int
		}
		var tipRows []tipRow
		trows, _ := db.Pool.Query(ctx,
			`SELECT t.match_id, t.user_id, COALESCE(t.points, 0)::int
			   FROM tips t
			  WHERE t.match_id = ANY($1) AND t.points IS NOT NULL`, matchIDs)
		for trows.Next() {
			var tr tipRow
			_ = trows.Scan(&tr.MatchID, &tr.UserID, &tr.Pts)
			tipRows = append(tipRows, tr)
		}
		trows.Close()

		// Jména uživatelů
		unames := map[int]string{}
		unameRows, _ := db.Pool.Query(ctx,
			`SELECT id, username FROM users WHERE COALESCE(is_blocked,false)=false AND COALESCE(is_inactive,false)=false`)
		for unameRows.Next() {
			var uid int
			var uname string
			_ = unameRows.Scan(&uid, &uname)
			unames[uid] = uname
		}
		unameRows.Close()

		// Body[matchIdx] per user
		type userCum struct {
			Name string
			Pts  []int
		}
		userMap := map[int]*userCum{}
		getUser := func(uid int) *userCum {
			if uc, ok := userMap[uid]; ok {
				return uc
			}
			name := unames[uid]
			if name == "" {
				return nil
			}
			uc := &userCum{Name: name, Pts: make([]int, len(matchList))}
			userMap[uid] = uc
			return uc
		}

		for _, tr := range tipRows {
			uc := getUser(tr.UserID)
			if uc == nil {
				continue
			}
			if idx, ok := matchIdx[tr.MatchID]; ok {
				uc.Pts[idx] = tr.Pts
			}
		}

		// Kumulativní součet
		for _, uc := range userMap {
			for i := 1; i < len(uc.Pts); i++ {
				uc.Pts[i] += uc.Pts[i-1]
			}
		}

		// Seřaď podle celkových bodů
		type userEntry struct {
			uid int
			uc  *userCum
		}
		var entries []userEntry
		for uid, uc := range userMap {
			entries = append(entries, userEntry{uid, uc})
		}
		sort.Slice(entries, func(i, j int) bool {
			n := len(matchList) - 1
			return entries[i].uc.Pts[n] > entries[j].uc.Pts[n]
		})

		labels := make([]string, len(matchList))
		for i, mi := range matchList {
			labels[i] = mi.Label
		}

		var datasets []dataset
		for _, e := range entries {
			datasets = append(datasets, dataset{
				Label: e.uc.Name,
				IsMe:  e.uid == u.ID,
				Data:  e.uc.Pts,
			})
		}

		enc := json.NewEncoder(w)
		_ = enc.Encode(chartResp{Labels: labels, Datasets: datasets})
	}
}

// GET /api/my-rank — returns {place, points, comp_name} for current user in top active competition
func MyRankAPI(tmpl *template.Template) http.HandlerFunc {
	type resp struct {
		Place    int    `json:"place"`
		Points   int    `json:"points"`
		CompName string `json:"comp_name"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		u := RequireLogin(w, r)
		if u == nil {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		ctx := context.Background()
		// Find top active competition by sort_order
		var compID int
		var compName string
		_ = db.Pool.QueryRow(ctx,
			`SELECT id, name FROM competitions WHERE is_active=true AND COALESCE(is_hidden,false)=false ORDER BY COALESCE(sort_order,9999) ASC, id DESC LIMIT 1`).
			Scan(&compID, &compName)
		if compID == 0 {
			w.Write([]byte(`{}`))
			return
		}
		// Get all grand totals and find rank
		type row struct{ uid, pts, exact int }
		rows, _ := db.Pool.Query(ctx,
			`SELECT t.user_id,
			        COALESCE(SUM(t.points),0)::int as pts,
			        COALESCE(SUM(CASE WHEN t.points=3 THEN 1 ELSE 0 END),0)::int as exact
			   FROM tips t
			   JOIN matches m ON m.id=t.match_id
			   JOIN rounds r ON r.id=m.round_id
			  WHERE r.competition_id=$1 AND m.is_finished=true AND t.points IS NOT NULL
			  GROUP BY t.user_id`, compID)
		var allRows []row
		myPts, myExact := 0, 0
		for rows.Next() {
			var rr row
			_ = rows.Scan(&rr.uid, &rr.pts, &rr.exact)
			allRows = append(allRows, rr)
			if rr.uid == u.ID {
				myPts = rr.pts
				myExact = rr.exact
			}
		}
		rows.Close()
		// Add extra points
		var extraPts int
		_ = db.Pool.QueryRow(ctx,
			`SELECT COALESCE(SUM(ea.points),0)::int FROM extra_answers ea
			   JOIN extra_questions eq ON eq.id=ea.question_id
			  WHERE eq.competition_id=$1 AND ea.user_id=$2 AND ea.points IS NOT NULL`,
			compID, u.ID).Scan(&extraPts)
		myTotal := myPts + extraPts
		place := 1
		for _, rr := range allRows {
			if rr.uid == u.ID {
				continue
			}
			if rr.pts > myPts || (rr.pts == myPts && rr.exact > myExact) {
				place++
			}
		}
		enc := json.NewEncoder(w)
		_ = enc.Encode(resp{Place: place, Points: myTotal, CompName: compName})
	}
}

// GET /leaderboard/last-update?competition_id=X
func LeaderboardLastUpdate(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := RequireLogin(w, r)
		if u == nil {
			return
		}
		_ = u
		w.Header().Set("Content-Type", "application/json")
		compID, _ := strconv.Atoi(r.URL.Query().Get("competition_id"))
		if compID == 0 {
			w.Write([]byte(`{"ts":0}`))
			return
		}
		ctx := context.Background()
		var ts int64
		_ = db.Pool.QueryRow(ctx,
			`SELECT COALESCE(EXTRACT(EPOCH FROM MAX(m.updated_at))::bigint, 0)
			   FROM matches m JOIN rounds r ON r.id=m.round_id
			  WHERE r.competition_id=$1 AND m.is_finished=true`, compID).Scan(&ts)
		if ts == 0 {
			// fallback: count finished matches
			_ = db.Pool.QueryRow(ctx,
				`SELECT COUNT(*) FROM matches m JOIN rounds r ON r.id=m.round_id
				  WHERE r.competition_id=$1 AND m.is_finished=true`, compID).Scan(&ts)
		}
		w.Write([]byte(`{"ts":` + strconv.FormatInt(ts, 10) + `}`))
	}
}
