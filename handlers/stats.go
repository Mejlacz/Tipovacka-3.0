// handlers/stats.go — Tipovačka 2.0
// Detailní statistiky tipéra per-soutěž.
package handlers

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"time"

	"tipovacka/db"
	"tipovacka/models"
)

type StatsData struct {
	Comp           *models.Competition
	TotalFinished  int
	TipsGiven      int
	Exact          int
	Winner         int
	Miss           int
	TotalPts       int
	Avg            float64
	GroupAvg       float64
	Diff           float64
	Rank           int
	TotalPlayers   int
	HomeTipped     int
	DrawTipped     int
	AwayTipped     int
	TendencyLabel  string
	TendencyCount  int
	BestTeams      []TeamStat
	WorstTeams     []TeamStat
	BestStreak     int
	FavScore       string
	FavScoreCount  int
}

type TeamStat struct {
	Name string
	Tips int
	Pts  int
	Avg  float64
}

func computeStats(ctx context.Context, user *models.User, comp *models.Competition) *StatsData {
	// Round IDs
	var roundIDs []int
	rRows, _ := db.Pool.Query(ctx, `SELECT id FROM rounds WHERE competition_id=$1`, comp.ID)
	for rRows.Next() {
		var rid int
		_ = rRows.Scan(&rid)
		roundIDs = append(roundIDs, rid)
	}
	rRows.Close()
	if len(roundIDs) == 0 {
		return nil
	}

	// Finished matches
	type MatchInfo struct {
		ID         int
		HomeTeamID int
		AwayTeamID int
		HomeTeamName string
		AwayTeamName string
		MatchDate  *time.Time
	}
	finishedByID := map[int]MatchInfo{}
	mRows, _ := db.Pool.Query(ctx,
		`SELECT m.id, m.home_team_id, m.away_team_id, ht.name, at.name, m.match_date
		   FROM matches m
		   JOIN teams ht ON ht.id = m.home_team_id
		   JOIN teams at ON at.id = m.away_team_id
		  WHERE m.round_id = ANY($1) AND m.is_finished=TRUE`, roundIDs)
	for mRows.Next() {
		var mi MatchInfo
		_ = mRows.Scan(&mi.ID, &mi.HomeTeamID, &mi.AwayTeamID, &mi.HomeTeamName, &mi.AwayTeamName, &mi.MatchDate)
		finishedByID[mi.ID] = mi
	}
	mRows.Close()
	if len(finishedByID) == 0 {
		return nil
	}

	finishedIDs := make([]int, 0, len(finishedByID))
	for id := range finishedByID {
		finishedIDs = append(finishedIDs, id)
	}

	// User tips on finished matches
	type TipInfo struct {
		MatchID   int
		HomeScore int
		AwayScore int
		Points    *int
	}
	var userTips []TipInfo
	tRows, _ := db.Pool.Query(ctx,
		`SELECT match_id, home_score, away_score, points FROM tips
		  WHERE user_id=$1 AND match_id = ANY($2) AND points IS NOT NULL`, user.ID, finishedIDs)
	for tRows.Next() {
		var ti TipInfo
		_ = tRows.Scan(&ti.MatchID, &ti.HomeScore, &ti.AwayScore, &ti.Points)
		userTips = append(userTips, ti)
	}
	tRows.Close()
	if len(userTips) == 0 {
		return nil
	}

	var exact, winner, miss, totalPts int
	for _, t := range userTips {
		totalPts += *t.Points
		switch *t.Points {
		case 3:
			exact++
		case 1:
			winner++
		case 0:
			miss++
		}
	}
	n := len(userTips)
	avg := 0.0
	if n > 0 {
		avg = float64(totalPts) / float64(n)
	}

	// Group comparison
	type userAvgEntry struct {
		UserID int
		Avg    float64
	}
	allTipRows, _ := db.Pool.Query(ctx,
		`SELECT user_id, SUM(points), COUNT(*) FROM tips
		  WHERE match_id = ANY($1) AND points IS NOT NULL GROUP BY user_id`, finishedIDs)
	var userAvgs []userAvgEntry
	for allTipRows.Next() {
		var uid, sumPts, cnt int
		_ = allTipRows.Scan(&uid, &sumPts, &cnt)
		a := 0.0
		if cnt > 0 {
			a = float64(sumPts) / float64(cnt)
		}
		userAvgs = append(userAvgs, userAvgEntry{uid, a})
	}
	allTipRows.Close()

	groupAvg := 0.0
	if len(userAvgs) > 0 {
		sum := 0.0
		for _, ua := range userAvgs {
			sum += ua.Avg
		}
		groupAvg = sum / float64(len(userAvgs))
	}
	diff := avg - groupAvg

	// Rank
	rank := 1
	for _, ua := range userAvgs {
		if ua.UserID != user.ID && ua.Avg > avg {
			rank++
		}
	}
	totalPlayers := len(userAvgs)

	// Tendency
	var homeTipped, drawTipped, awayTipped int
	for _, t := range userTips {
		if t.HomeScore > t.AwayScore {
			homeTipped++
		} else if t.HomeScore == t.AwayScore {
			drawTipped++
		} else {
			awayTipped++
		}
	}
	tendencyLabel := "tendency_home"
	tendencyCount := homeTipped
	if drawTipped > tendencyCount {
		tendencyLabel = "tendency_draw"
		tendencyCount = drawTipped
	}
	if awayTipped > tendencyCount {
		tendencyLabel = "tendency_away"
		tendencyCount = awayTipped
	}

	// Team stats
	teamStats := map[int]*TeamStat{}
	teamNames := map[int]string{}
	for _, t := range userTips {
		m, ok := finishedByID[t.MatchID]
		if !ok {
			continue
		}
		pts := *t.Points
		for _, pair := range [][2]interface{}{{m.HomeTeamID, m.HomeTeamName}, {m.AwayTeamID, m.AwayTeamName}} {
			tid := pair[0].(int)
			tname := pair[1].(string)
			teamNames[tid] = tname
			if teamStats[tid] == nil {
				teamStats[tid] = &TeamStat{Name: tname}
			}
			teamStats[tid].Tips++
			teamStats[tid].Pts += pts
		}
	}
	var teamList []TeamStat
	for _, ts := range teamStats {
		if ts.Tips >= 2 {
			ts.Avg = float64(ts.Pts) / float64(ts.Tips)
			teamList = append(teamList, *ts)
		}
	}
	sort.Slice(teamList, func(i, j int) bool { return teamList[i].Avg > teamList[j].Avg })
	bestTeams := teamList
	if len(bestTeams) > 5 {
		bestTeams = bestTeams[:5]
	}
	sort.Slice(teamList, func(i, j int) bool { return teamList[i].Avg < teamList[j].Avg })
	worstTeams := teamList
	if len(worstTeams) > 5 {
		worstTeams = worstTeams[:5]
	}

	// Streak
	sort.Slice(userTips, func(i, j int) bool {
		mi := finishedByID[userTips[i].MatchID]
		mj := finishedByID[userTips[j].MatchID]
		if mi.MatchDate == nil {
			return false
		}
		if mj.MatchDate == nil {
			return true
		}
		return mi.MatchDate.Before(*mj.MatchDate)
	})
	bestStreak, curStreak := 0, 0
	for _, t := range userTips {
		if *t.Points == 3 {
			curStreak++
			if curStreak > bestStreak {
				bestStreak = curStreak
			}
		} else {
			curStreak = 0
		}
	}

	// Favourite score
	scoreFreq := map[string]int{}
	for _, t := range userTips {
		key := strconv.Itoa(t.HomeScore) + ":" + strconv.Itoa(t.AwayScore)
		scoreFreq[key]++
	}
	favScore := ""
	favScoreCount := 0
	for score, cnt := range scoreFreq {
		if cnt > favScoreCount {
			favScore = score
			favScoreCount = cnt
		}
	}

	return &StatsData{
		Comp:          comp,
		TotalFinished: len(finishedByID),
		TipsGiven:     n,
		Exact:         exact,
		Winner:        winner,
		Miss:          miss,
		TotalPts:      totalPts,
		Avg:           roundF(avg, 2),
		GroupAvg:      roundF(groupAvg, 2),
		Diff:          roundF(diff, 2),
		Rank:          rank,
		TotalPlayers:  totalPlayers,
		HomeTipped:    homeTipped,
		DrawTipped:    drawTipped,
		AwayTipped:    awayTipped,
		TendencyLabel: tendencyLabel,
		TendencyCount: tendencyCount,
		BestTeams:     bestTeams,
		WorstTeams:    worstTeams,
		BestStreak:    bestStreak,
		FavScore:      favScore,
		FavScoreCount: favScoreCount,
	}
}

func roundF(f float64, decimals int) float64 {
	// Simple rounding
	factor := 1.0
	for i := 0; i < decimals; i++ {
		factor *= 10
	}
	return float64(int(f*factor+0.5)) / factor
}

// GET /stats  — redirect to latest competition
func StatsRedirect(w http.ResponseWriter, r *http.Request) {
	user := RequireLogin(w, r)
	if user == nil {
		return
	}
	ctx := context.Background()
	var compID int
	// Try active first
	_ = db.Pool.QueryRow(ctx,
		`SELECT id FROM competitions WHERE is_active=TRUE ORDER BY id DESC LIMIT 1`).Scan(&compID)
	if compID == 0 {
		_ = db.Pool.QueryRow(ctx,
			`SELECT id FROM competitions ORDER BY id DESC LIMIT 1`).Scan(&compID)
	}
	if compID > 0 {
		http.Redirect(w, r, "/stats/"+strconv.Itoa(compID), http.StatusSeeOther)
		return
	}
	// No competitions — just redirect to root
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// GET /stats/{competition_id}
func StatsDetail(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := RequireLogin(w, r)
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
			http.Redirect(w, r, "/stats", http.StatusSeeOther)
			return
		}

		s := computeStats(ctx, user, comp)

		allCompRows, _ := db.Pool.Query(ctx,
			`SELECT id, name, season, is_active, sport, sort_order FROM competitions ORDER BY id DESC`)
		var activeComps, inactiveComps []*models.Competition
		for allCompRows.Next() {
			c := &models.Competition{}
			_ = allCompRows.Scan(&c.ID, &c.Name, &c.Season, &c.IsActive, &c.Sport, &c.SortOrder)
			if c.IsActive {
				activeComps = append(activeComps, c)
			} else {
				inactiveComps = append(inactiveComps, c)
			}
		}
		allCompRows.Close()

		// Compare users — others who tipped in this competition
		var roundIDs []int
		rRows, _ := db.Pool.Query(ctx, `SELECT id FROM rounds WHERE competition_id=$1`, compID)
		for rRows.Next() {
			var rid int
			_ = rRows.Scan(&rid)
			roundIDs = append(roundIDs, rid)
		}
		rRows.Close()

		var compareUsers []*models.User
		if len(roundIDs) > 0 {
			// Přidat filtr is_blocked=FALSE jen pokud sloupec existuje
			blockedFilter := ""
			if userCols.IsBlocked {
				blockedFilter = " AND u.is_blocked=FALSE"
			}
			cuRows, _ := db.Pool.Query(ctx,
				`SELECT DISTINCT u.id, u.username
				   FROM users u
				   JOIN tips t ON t.user_id = u.id
				   JOIN matches m ON m.id = t.match_id
				  WHERE m.round_id = ANY($1) AND u.id != $2`+blockedFilter+`
				  ORDER BY u.username`, roundIDs, user.ID)
			for cuRows.Next() {
				u := &models.User{}
				_ = cuRows.Scan(&u.ID, &u.Username)
				compareUsers = append(compareUsers, u)
			}
			cuRows.Close()
		}

		RenderTemplate(w, r, tmpl, "stats_detail.html", TemplateData{
			"User":                 user,
			"S":                    s,
			"Comp":                 comp,
			"ActiveCompetitions":   activeComps,
			"InactiveCompetitions": inactiveComps,
			"CompID":               compID,
			"CompareUsers":         compareUsers,
		})
	}
}

// GET /stats/{competition_id}/extended
// Rozšířené statistiky pro konkrétního uživatele v dané soutěži.
// Parametr: ?user_id=N (volitelný — výchozí je přihlášený uživatel)
func StatsExtended(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		currentUser := RequireLogin(w, r)
		if currentUser == nil {
			return
		}
		compID, _ := strconv.Atoi(r.PathValue("competition_id"))
		ctx := context.Background()

		comp := &models.Competition{}
		err := db.Pool.QueryRow(ctx,
			`SELECT id, name, season, is_active, sport, sort_order FROM competitions WHERE id=$1`, compID).
			Scan(&comp.ID, &comp.Name, &comp.Season, &comp.IsActive, &comp.Sport, &comp.SortOrder)
		if err != nil {
			http.Redirect(w, r, "/stats", http.StatusSeeOther)
			return
		}

		// Resolve target user
		targetUser := currentUser
		if userIDStr := r.URL.Query().Get("user_id"); userIDStr != "" {
			uid, _ := strconv.Atoi(userIDStr)
			if uid > 0 && uid != currentUser.ID {
				other := &models.User{}
				cols, _ := buildUserSelect()
				otherRow := db.Pool.QueryRow(ctx, "SELECT "+cols+" FROM users WHERE id=$1", uid)
				if err := scanUser(other, otherRow); err == nil {
					targetUser = other
				}
			}
		}

		// Extended stats query
		type ExtTip struct {
			TipHome   int
			TipAway   int
			Points    int
			ActHome   int
			ActAway   int
			HomeTeam  string
			AwayTeam  string
			MatchDate *time.Time
		}

		rows, _ := db.Pool.Query(ctx, `
			SELECT t.home_score, t.away_score, COALESCE(t.points,0),
			       m.home_score, m.away_score,
			       ht.name, at.name, m.match_date
			FROM tips t
			JOIN matches m ON m.id = t.match_id
			JOIN teams ht ON ht.id = m.home_team_id
			JOIN teams at ON at.id = m.away_team_id
			JOIN rounds ro ON ro.id = m.round_id
			WHERE ro.competition_id = $1 AND t.user_id = $2
			  AND m.is_finished = TRUE AND t.points IS NOT NULL
			ORDER BY m.match_date`, compID, targetUser.ID)

		var tips []ExtTip
		for rows.Next() {
			var et ExtTip
			var actH, actA *int
			_ = rows.Scan(&et.TipHome, &et.TipAway, &et.Points, &actH, &actA, &et.HomeTeam, &et.AwayTeam, &et.MatchDate)
			if actH != nil {
				et.ActHome = *actH
			}
			if actA != nil {
				et.ActAway = *actA
			}
			tips = append(tips, et)
		}
		rows.Close()

		// Tendence
		var homeTipped, drawTipped, awayTipped int
		var homeWon, drawWon, awayWon int
		for _, t := range tips {
			switch {
			case t.TipHome > t.TipAway:
				homeTipped++
				if t.ActHome > t.ActAway {
					homeWon++
				}
			case t.TipHome == t.TipAway:
				drawTipped++
				if t.ActHome == t.ActAway {
					drawWon++
				}
			default:
				awayTipped++
				if t.ActHome < t.ActAway {
					awayWon++
				}
			}
		}

		// Nejlepší / nejhorší týmy
		type TeamExtStat struct {
			Name    string
			Tips    int
			Pts     int
			Avg     float64
		}
		teamMap := map[string]*TeamExtStat{}
		for _, t := range tips {
			for _, name := range []string{t.HomeTeam, t.AwayTeam} {
				ts, ok := teamMap[name]
				if !ok {
					ts = &TeamExtStat{Name: name}
					teamMap[name] = ts
				}
				ts.Tips++
				ts.Pts += t.Points
			}
		}
		teamList := make([]TeamExtStat, 0, len(teamMap))
		for _, ts := range teamMap {
			if ts.Tips >= 2 {
				ts.Avg = float64(ts.Pts) / float64(ts.Tips)
				teamList = append(teamList, *ts)
			}
		}
		sort.Slice(teamList, func(i, j int) bool { return teamList[i].Avg > teamList[j].Avg })
		bestN := 5
		if len(teamList) < bestN {
			bestN = len(teamList)
		}
		bestTeams := make([]TeamExtStat, bestN)
		copy(bestTeams, teamList[:bestN])

		sort.Slice(teamList, func(i, j int) bool { return teamList[i].Avg < teamList[j].Avg })
		worstN := 5
		if len(teamList) < worstN {
			worstN = len(teamList)
		}
		worstTeams := make([]TeamExtStat, worstN)
		copy(worstTeams, teamList[:worstN])

		// Série
		bestStreak, curStreak, currentStreak := 0, 0, 0
		for i, t := range tips {
			if t.Points == 3 {
				curStreak++
				if curStreak > bestStreak {
					bestStreak = curStreak
				}
				if i == len(tips)-1 {
					currentStreak = curStreak
				}
			} else {
				if i == len(tips)-1 {
					currentStreak = 0
				}
				curStreak = 0
			}
		}
		// fix currentStreak for last element
		if len(tips) > 0 {
			curStreak2 := 0
			for i := len(tips) - 1; i >= 0; i-- {
				if tips[i].Points == 3 {
					curStreak2++
				} else {
					break
				}
			}
			currentStreak = curStreak2
		}

		// Users pro select
		var compareUsers []*models.User
		{
			blockedFilter := ""
			if userCols.IsBlocked {
				blockedFilter = " AND u.is_blocked=FALSE"
			}
			var roundIDs []int
			rRows, _ := db.Pool.Query(ctx, `SELECT id FROM rounds WHERE competition_id=$1`, compID)
			for rRows.Next() {
				var rid int
				_ = rRows.Scan(&rid)
				roundIDs = append(roundIDs, rid)
			}
			rRows.Close()
			if len(roundIDs) > 0 {
				cuRows, _ := db.Pool.Query(ctx,
					`SELECT DISTINCT u.id, u.username
					   FROM users u
					   JOIN tips t ON t.user_id = u.id
					   JOIN matches m ON m.id = t.match_id
					  WHERE m.round_id = ANY($1)`+blockedFilter+`
					  ORDER BY u.username`, roundIDs)
				for cuRows.Next() {
					u := &models.User{}
					_ = cuRows.Scan(&u.ID, &u.Username)
					compareUsers = append(compareUsers, u)
				}
				cuRows.Close()
			}
		}

		// All competitions for nav
		allCompRows, _ := db.Pool.Query(ctx,
			`SELECT id, name, season, is_active, sport, sort_order FROM competitions ORDER BY id DESC`)
		var activeComps, inactiveComps []*models.Competition
		for allCompRows.Next() {
			c := &models.Competition{}
			_ = allCompRows.Scan(&c.ID, &c.Name, &c.Season, &c.IsActive, &c.Sport, &c.SortOrder)
			if c.IsActive {
				activeComps = append(activeComps, c)
			} else {
				inactiveComps = append(inactiveComps, c)
			}
		}
		allCompRows.Close()

		type TendencyRow struct {
			Label   string
			Tipped  int
			Won     int
			WinPct  string
		}
		tendencies := []TendencyRow{
			{"Domácí výhra", homeTipped, homeWon, pct(homeWon, homeTipped)},
			{"Remíza", drawTipped, drawWon, pct(drawWon, drawTipped)},
			{"Hostující výhra", awayTipped, awayWon, pct(awayWon, awayTipped)},
		}

		RenderTemplate(w, r, tmpl, "stats/extended.html", TemplateData{
			"User":                 currentUser,
			"TargetUser":          targetUser,
			"Comp":                 comp,
			"CompID":               compID,
			"Tips":                 tips,
			"TotalTips":            len(tips),
			"Tendencies":          tendencies,
			"BestTeams":           bestTeams,
			"WorstTeams":          worstTeams,
			"BestStreak":          bestStreak,
			"CurrentStreak":       currentStreak,
			"ActiveCompetitions":  activeComps,
			"InactiveCompetitions": inactiveComps,
			"CompareUsers":        compareUsers,
		})
	}
}

func pct(won, total int) string {
	if total == 0 {
		return "—"
	}
	return fmt.Sprintf("%d%%", 100*won/total)
}

// GET /stats/{competition_id}/vs/{other_user_id}
func StatsVs(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := RequireLogin(w, r)
		if user == nil {
			return
		}
		compID, _ := strconv.Atoi(r.PathValue("competition_id"))
		otherID, _ := strconv.Atoi(r.PathValue("other_user_id"))
		ctx := context.Background()

		comp := &models.Competition{}
		err := db.Pool.QueryRow(ctx,
			`SELECT id, name, season, is_active, sport, sort_order FROM competitions WHERE id=$1`, compID).
			Scan(&comp.ID, &comp.Name, &comp.Season, &comp.IsActive, &comp.Sport, &comp.SortOrder)
		if err != nil {
			http.Redirect(w, r, "/stats", http.StatusSeeOther)
			return
		}

		other := &models.User{}
		{
			cols, _ := buildUserSelect()
			otherRow := db.Pool.QueryRow(ctx, "SELECT "+cols+" FROM users WHERE id=$1", otherID)
			if err := scanUser(other, otherRow); err != nil {
				http.Redirect(w, r, "/stats", http.StatusSeeOther)
				return
			}
		}

		sMe := computeStats(ctx, user, comp)
		sOther := computeStats(ctx, other, comp)

		type DuelMatch struct {
			Home     string
			Away     string
			Result   string
			PtsMe    int
			PtsOther int
			Winner   string
		}
		type Duel struct {
			WinsMe    int
			WinsOther int
			Draws     int
			Matches   []DuelMatch
		}
		duel := Duel{}

		if sMe != nil && sOther != nil {
			var roundIDs []int
			rRows, _ := db.Pool.Query(ctx, `SELECT id FROM rounds WHERE competition_id=$1`, compID)
			for rRows.Next() {
				var rid int
				_ = rRows.Scan(&rid)
				roundIDs = append(roundIDs, rid)
			}
			rRows.Close()

			if len(roundIDs) > 0 {
				type FinMatch struct {
					ID            int
					HomeTeamName  string
					AwayTeamName  string
					HomeScore     int
					AwayScore     int
				}
				var finMatches []FinMatch
				fmRows, _ := db.Pool.Query(ctx,
					`SELECT m.id, ht.name, at.name, m.home_score, m.away_score
					   FROM matches m
					   JOIN teams ht ON ht.id = m.home_team_id
					   JOIN teams at ON at.id = m.away_team_id
					  WHERE m.round_id = ANY($1) AND m.is_finished=TRUE ORDER BY m.match_date`, roundIDs)
				for fmRows.Next() {
					var fm FinMatch
					_ = fmRows.Scan(&fm.ID, &fm.HomeTeamName, &fm.AwayTeamName, &fm.HomeScore, &fm.AwayScore)
					finMatches = append(finMatches, fm)
				}
				fmRows.Close()

				fmIDs := make([]int, len(finMatches))
				for i, fm := range finMatches {
					fmIDs[i] = fm.ID
				}

				tipsMe := map[int]int{}
				tipsOther := map[int]int{}
				if len(fmIDs) > 0 {
					t1Rows, _ := db.Pool.Query(ctx,
						`SELECT match_id, points FROM tips WHERE user_id=$1 AND match_id = ANY($2) AND points IS NOT NULL`, user.ID, fmIDs)
					for t1Rows.Next() {
						var mid, pts int
						_ = t1Rows.Scan(&mid, &pts)
						tipsMe[mid] = pts
					}
					t1Rows.Close()

					t2Rows, _ := db.Pool.Query(ctx,
						`SELECT match_id, points FROM tips WHERE user_id=$1 AND match_id = ANY($2) AND points IS NOT NULL`, other.ID, fmIDs)
					for t2Rows.Next() {
						var mid, pts int
						_ = t2Rows.Scan(&mid, &pts)
						tipsOther[mid] = pts
					}
					t2Rows.Close()
				}

				for _, fm := range finMatches {
					pMe, okMe := tipsMe[fm.ID]
					pOther, okOther := tipsOther[fm.ID]
					if !okMe || !okOther {
						continue
					}
					winner := "draw"
					if pMe > pOther {
						winner = "me"
						duel.WinsMe++
					} else if pOther > pMe {
						winner = "other"
						duel.WinsOther++
					} else {
						duel.Draws++
					}
					duel.Matches = append(duel.Matches, DuelMatch{
						Home:     fm.HomeTeamName,
						Away:     fm.AwayTeamName,
						Result:   strconv.Itoa(fm.HomeScore) + ":" + strconv.Itoa(fm.AwayScore),
						PtsMe:    pMe,
						PtsOther: pOther,
						Winner:   winner,
					})
				}
			}
		}

		allCompRows, _ := db.Pool.Query(ctx,
			`SELECT id, name, season, is_active, sport, sort_order FROM competitions ORDER BY id DESC`)
		var activeComps, inactiveComps []*models.Competition
		for allCompRows.Next() {
			c := &models.Competition{}
			_ = allCompRows.Scan(&c.ID, &c.Name, &c.Season, &c.IsActive, &c.Sport, &c.SortOrder)
			if c.IsActive {
				activeComps = append(activeComps, c)
			} else {
				inactiveComps = append(inactiveComps, c)
			}
		}
		allCompRows.Close()

		RenderTemplate(w, r, tmpl, "stats_vs.html", TemplateData{
			"User":                 user,
			"Other":                other,
			"Comp":                 comp,
			"CompID":               compID,
			"SMe":                  sMe,
			"SOther":               sOther,
			"Duel":                 duel,
			"ActiveCompetitions":   activeComps,
			"InactiveCompetitions": inactiveComps,
		})
	}
}
