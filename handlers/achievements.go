// handlers/achievements.go — Tipovačka 2.0
// Stránka /achievements — přehled achievementů per soutěž.
package handlers

import (
	"context"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"tipovacka/db"
	"tipovacka/models"
)

// ── Achievement definitions ────────────────────────────────────────────────────

type Achievement struct {
	ID       string
	Icon     string
	Name     string
	Desc     string
	Category string
}

var CompAchievements = []Achievement{
	{ID: "c_full", Icon: "📋", Name: "Hráč", Desc: "Tipoval všechny odehrané zápasy v soutěži.", Category: "aktivita"},
	{ID: "c_lucky", Icon: "⭐", Name: "Šťastná střela", Desc: "První přesný výsledek v soutěži.", Category: "přesnost"},
	{ID: "c_sniper5", Icon: "🎯", Name: "Střelec", Desc: "5+ přesných výsledků v soutěži.", Category: "přesnost"},
	{ID: "c_sniper15", Icon: "🔮", Name: "Věštec", Desc: "15+ přesných výsledků v soutěži.", Category: "přesnost"},
	{ID: "c_solid", Icon: "📈", Name: "Solidní", Desc: "40 %+ úspěšnost v soutěži (min. 10 odehraných tipů).", Category: "úspěšnost"},
	{ID: "c_pro", Icon: "🏆", Name: "Profík", Desc: "60 %+ úspěšnost v soutěži (min. 10 odehraných tipů).", Category: "úspěšnost"},
	{ID: "c_sniper20", Icon: "🔮🔮", Name: "Jasnovidec", Desc: "20+ přesných výsledků v soutěži.", Category: "přesnost"},
	{ID: "c_draw", Icon: "🤝", Name: "Remizář", Desc: "Přesně tipoval alespoň 1 remízu.", Category: "remízy"},
	{ID: "c_hot3", Icon: "🔥", Name: "Série 3", Desc: "3 správné tipy (přesný nebo správný vítěz) v řadě.", Category: "série"},
	{ID: "c_hot5", Icon: "🔥🔥", Name: "Série 5", Desc: "5 správných tipů v řadě.", Category: "série"},
	{ID: "c_hot7", Icon: "🔥🔥🔥", Name: "Série 7", Desc: "7 správných tipů v řadě.", Category: "série"},
	{ID: "c_perfect", Icon: "💎", Name: "Perfekcionista", Desc: "Celé kolo pouze s přesnými výsledky (min. 3 tipy v kole).", Category: "speciální"},
	{ID: "c_undefeated", Icon: "🛡️", Name: "Neporazitelný", Desc: "Žádný nulový tip v celé soutěži (min. 10 odehraných tipů).", Category: "speciální"},
	{ID: "c_cannon", Icon: "💥", Name: "Kanonýr", Desc: "Přesně tipoval výsledek s celkem 5+ góly.", Category: "speciální"},
	{ID: "c_round_win", Icon: "👑", Name: "Král kola", Desc: "1. místo v jakémkoliv kole soutěže (min. 2 účastníci s body).", Category: "speciální"},
	{ID: "c_rounds2", Icon: "👑👑", Name: "Sériový král", Desc: "Vyhrál alespoň 2 různá kola v soutěži.", Category: "speciální"},
	{ID: "c_top3", Icon: "🥉", Name: "Medailista", Desc: "TOP 3 v soutěži (min. 5 účastníků s alespoň 1 tipem).", Category: "pořadí"},
	{ID: "c_champion", Icon: "🥇", Name: "Šampion", Desc: "1. místo v uzavřené soutěži (min. 5 účastníků).", Category: "pořadí"},
	{ID: "c_no_exact", Icon: "😅", Name: "Žádný přesný", Desc: "Odehrál alespoň 5 tipů v soutěži bez jediného přesného výsledku.", Category: "speciální"},
}

// userAchStats holds per-user computed data for achievement checking.
type userAchStats struct {
	TotalTips       int
	FinishedTips    int
	Exact           int
	Rate            float64
	FullParticipation bool
	HasPerfect      bool
	MaxStreak       int
	IsRoundWinner   bool
	RoundsWon       int
	ExactDraws      int
	Undefeated      bool
	BigScoreExact   bool
	Rank            int
	NumParticipants int
	CompIsActive    bool
}

func checkCompAchievement(id string, s userAchStats) bool {
	switch id {
	case "c_full":      return s.FullParticipation
	case "c_lucky":     return s.Exact >= 1
	case "c_sniper5":   return s.Exact >= 5
	case "c_sniper15":  return s.Exact >= 15
	case "c_sniper20":  return s.Exact >= 20
	case "c_draw":      return s.ExactDraws >= 1
	case "c_solid":     return s.Rate >= 0.40
	case "c_pro":       return s.Rate >= 0.60
	case "c_hot3":      return s.MaxStreak >= 3
	case "c_hot5":      return s.MaxStreak >= 5
	case "c_hot7":      return s.MaxStreak >= 7
	case "c_perfect":   return s.HasPerfect
	case "c_undefeated": return s.Undefeated
	case "c_cannon":    return s.BigScoreExact
	case "c_round_win": return s.IsRoundWinner
	case "c_rounds2":   return s.RoundsWon >= 2
	case "c_top3":      return s.Rank > 0 && s.Rank <= 3 && s.NumParticipants >= 5
	case "c_champion":  return s.Rank == 1 && s.NumParticipants >= 5 && !s.CompIsActive
	case "c_no_exact":  return s.Exact == 0 && s.FinishedTips >= 5
	}
	return false
}

// computeAllCompAchievements returns {userID: []earnedAchievements} for a competition.
func computeAllCompAchievements(ctx context.Context, compID int, compIsActive bool) map[int][]Achievement {
	// 1. Load rounds
	rRows, _ := db.Pool.Query(ctx, `SELECT id FROM rounds WHERE competition_id=$1`, compID)
	var roundIDs []int
	for rRows.Next() {
		var rid int
		_ = rRows.Scan(&rid)
		roundIDs = append(roundIDs, rid)
	}
	rRows.Close()
	if len(roundIDs) == 0 {
		return nil
	}

	// 2. Load matches (ordered by match_date for streak computation)
	type matchInfo struct {
		ID        int
		RoundID   int
		HomeScore *int
		AwayScore *int
		Date      *time.Time
		IsFinished bool
	}
	mRows, _ := db.Pool.Query(ctx,
		`SELECT id, round_id, home_score, away_score, match_date, is_finished
		   FROM matches WHERE round_id = ANY($1)
		  ORDER BY match_date NULLS LAST, id`, roundIDs)
	var matches []matchInfo
	for mRows.Next() {
		var m matchInfo
		_ = mRows.Scan(&m.ID, &m.RoundID, &m.HomeScore, &m.AwayScore, &m.Date, &m.IsFinished)
		matches = append(matches, m)
	}
	mRows.Close()
	if len(matches) == 0 {
		return nil
	}

	finishedMatchIDs := map[int]bool{}
	matchToRound := map[int]int{}
	matchOrder := map[int]int{}
	for i, m := range matches {
		matchToRound[m.ID] = m.RoundID
		matchOrder[m.ID] = i
		if m.IsFinished {
			finishedMatchIDs[m.ID] = true
		}
	}

	// Home/away score per finished match (for cannon check)
	type matchScores struct{ home, away int }
	finishedScores := map[int]matchScores{}
	for _, m := range matches {
		if m.IsFinished && m.HomeScore != nil && m.AwayScore != nil {
			finishedScores[m.ID] = matchScores{*m.HomeScore, *m.AwayScore}
		}
	}

	allMatchIDs := make([]int, len(matches))
	for i, m := range matches {
		allMatchIDs[i] = m.ID
	}

	// 3. Load all tips
	type tipInfo struct {
		UserID    int
		MatchID   int
		HomeScore int
		AwayScore int
		Points    *int
	}
	tipRows, _ := db.Pool.Query(ctx,
		`SELECT user_id, match_id, home_score, away_score, points
		   FROM tips WHERE match_id = ANY($1)`, allMatchIDs)
	userTips := map[int][]tipInfo{}
	for tipRows.Next() {
		var t tipInfo
		_ = tipRows.Scan(&t.UserID, &t.MatchID, &t.HomeScore, &t.AwayScore, &t.Points)
		userTips[t.UserID] = append(userTips[t.UserID], t)
	}
	tipRows.Close()
	if len(userTips) == 0 {
		return nil
	}

	// 4. Round winners
	roundWinners := map[int]map[int]bool{} // round_id → set of winning user_ids
	for _, rid := range roundIDs {
		roundTotals := map[int]int{}
		for uid, tips := range userTips {
			pts := 0
			for _, t := range tips {
				if matchToRound[t.MatchID] == rid && t.Points != nil {
					pts += *t.Points
				}
			}
			if pts > 0 {
				roundTotals[uid] = pts
			}
		}
		if len(roundTotals) >= 2 {
			maxPts := 0
			for _, pts := range roundTotals {
				if pts > maxPts {
					maxPts = pts
				}
			}
			winners := map[int]bool{}
			for uid, pts := range roundTotals {
				if pts == maxPts {
					winners[uid] = true
				}
			}
			roundWinners[rid] = winners
		}
	}

	// 5. Overall totals for ranking
	finalTotals := map[int]int{}
	for uid, tips := range userTips {
		total := 0
		for _, t := range tips {
			if t.Points != nil {
				total += *t.Points
			}
		}
		finalTotals[uid] = total
	}
	numParticipants := len(finalTotals)

	// 6. Per-user achievement stats
	results := map[int][]Achievement{}
	for uid, tips := range userTips {
		// Finished tips (match has result and tip has points)
		var finishedTips []tipInfo
		for _, t := range tips {
			if finishedMatchIDs[t.MatchID] && t.Points != nil {
				finishedTips = append(finishedTips, t)
			}
		}
		nFinished := len(finishedTips)
		exact := 0
		for _, t := range finishedTips {
			if *t.Points == 3 {
				exact++
			}
		}

		// Rate (min 10 finished)
		rate := 0.0
		if nFinished >= 10 {
			sumPts := 0
			for _, t := range finishedTips {
				sumPts += *t.Points
			}
			rate = float64(sumPts) / float64(nFinished*3)
		}

		// Full participation
		tippedFinished := map[int]bool{}
		for _, t := range tips {
			if finishedMatchIDs[t.MatchID] {
				tippedFinished[t.MatchID] = true
			}
		}
		fullParticipation := len(finishedMatchIDs) > 0 && len(tippedFinished) == len(finishedMatchIDs)

		// Tips per round (for perfect round check)
		roundTips := map[int][]tipInfo{}
		for _, t := range tips {
			rid := matchToRound[t.MatchID]
			roundTips[rid] = append(roundTips[rid], t)
		}
		hasPerfect := false
		for _, rts := range roundTips {
			if len(rts) < 3 {
				continue
			}
			allExact := true
			for _, t := range rts {
				if t.Points == nil || *t.Points != 3 {
					allExact = false
					break
				}
			}
			if allExact {
				hasPerfect = true
				break
			}
		}

		// Streak (sort by match order)
		sortedFinished := make([]tipInfo, len(finishedTips))
		copy(sortedFinished, finishedTips)
		sort.Slice(sortedFinished, func(i, j int) bool {
			return matchOrder[sortedFinished[i].MatchID] < matchOrder[sortedFinished[j].MatchID]
		})
		maxStreak, curStreak := 0, 0
		for _, t := range sortedFinished {
			if *t.Points > 0 {
				curStreak++
				if curStreak > maxStreak {
					maxStreak = curStreak
				}
			} else {
				curStreak = 0
			}
		}

		// Round winner
		isRoundWinner := false
		roundsWon := 0
		for _, winners := range roundWinners {
			if winners[uid] {
				isRoundWinner = true
				roundsWon++
			}
		}

		// Exact draws
		exactDraws := 0
		for _, t := range finishedTips {
			if *t.Points == 3 && t.HomeScore == t.AwayScore {
				exactDraws++
			}
		}

		// Undefeated
		undefeated := nFinished >= 10
		if undefeated {
			for _, t := range finishedTips {
				if *t.Points == 0 {
					undefeated = false
					break
				}
			}
		}

		// Cannon: exact tip with 5+ total goals
		bigScoreExact := false
		for _, t := range finishedTips {
			if *t.Points == 3 {
				sc, ok := finishedScores[t.MatchID]
				if ok && (sc.home+sc.away) >= 5 {
					bigScoreExact = true
					break
				}
			}
		}

		// Rank
		userTotal := finalTotals[uid]
		rank := 1
		for ouid, ototal := range finalTotals {
			if ouid != uid && ototal > userTotal {
				rank++
			}
		}

		stats := userAchStats{
			TotalTips:         len(tips),
			FinishedTips:      nFinished,
			Exact:             exact,
			Rate:              rate,
			FullParticipation: fullParticipation,
			HasPerfect:        hasPerfect,
			MaxStreak:         maxStreak,
			IsRoundWinner:     isRoundWinner,
			RoundsWon:         roundsWon,
			ExactDraws:        exactDraws,
			Undefeated:        undefeated,
			BigScoreExact:     bigScoreExact,
			Rank:              rank,
			NumParticipants:   numParticipants,
			CompIsActive:      compIsActive,
		}

		var earned []Achievement
		for _, ach := range CompAchievements {
			if checkCompAchievement(ach.ID, stats) {
				earned = append(earned, ach)
			}
		}
		results[uid] = earned
	}
	return results
}

// GET /achievements
func AchievementsPage(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := RequireApproved(w, r)
		if user == nil {
			return
		}
		ctx := context.Background()

		// Load only recent competitions — achievements tracking starts from LM 2025/2026.
		// LIMIT keeps old historical competitions out of the selector.
		compRows, _ := db.Pool.Query(ctx,
			`SELECT id, name, season, is_active, sport, sort_order FROM competitions
			  WHERE COALESCE(is_hidden,false)=false ORDER BY sort_order NULLS LAST, id DESC LIMIT 10`)
		var competitions []*models.Competition
		for compRows.Next() {
			c := &models.Competition{}
			_ = compRows.Scan(&c.ID, &c.Name, &c.Season, &c.IsActive, &c.Sport, &c.SortOrder)
			competitions = append(competitions, c)
		}
		compRows.Close()

		// Determine selected competition
		selectedAll := r.URL.Query().Get("competition_id") == "all"
		var selectedComp *models.Competition
		if !selectedAll {
			if v := r.URL.Query().Get("competition_id"); v != "" {
				cid, _ := strconv.Atoi(v)
				for _, c := range competitions {
					if c.ID == cid {
						selectedComp = c
						break
					}
				}
			}
			if selectedComp == nil {
				for _, c := range competitions {
					if c.IsActive {
						selectedComp = c
						break
					}
				}
			}
			if selectedComp == nil && len(competitions) > 0 {
				selectedComp = competitions[0]
			}
		}

		type AchRow struct {
			User         *models.User
			EarnedIDs    map[string]bool
			EarnedCounts map[string]int // how many competitions the achievement was earned in
			Count        int            // total achievements (sum of EarnedCounts)
		}
		var rows []AchRow

		// Load visible users once (used by both per-comp and all-comps paths)
		visibleUsers := map[int]*models.User{}
		cols, _ := buildUserSelect()
		filterSQL := ""
		if userCols.IsBlocked {
			filterSQL += " AND is_blocked=FALSE"
		}
		uRows, _ := db.Pool.Query(ctx,
			"SELECT "+cols+" FROM users WHERE is_hidden=FALSE"+filterSQL+" ORDER BY username")
		for uRows.Next() {
			u := &models.User{}
			if scanUser(u, uRows) == nil {
				visibleUsers[u.ID] = u
			}
		}
		uRows.Close()

		if selectedAll {
			// Aggregate across ALL competitions
			// userAchCounts[uid][achID] = number of competitions where user earned this achievement
			userAchCounts := map[int]map[string]int{}
			for _, comp := range competitions {
				achByUser := computeAllCompAchievements(ctx, comp.ID, comp.IsActive)
				for uid, earned := range achByUser {
					if _, ok := userAchCounts[uid]; !ok {
						userAchCounts[uid] = map[string]int{}
					}
					for _, ach := range earned {
						userAchCounts[uid][ach.ID]++
					}
				}
			}
			for uid, achCounts := range userAchCounts {
				u, ok := visibleUsers[uid]
				if !ok {
					continue
				}
				earnedIDs := map[string]bool{}
				total := 0
				for achID, cnt := range achCounts {
					if cnt > 0 {
						earnedIDs[achID] = true
						total += cnt
					}
				}
				rows = append(rows, AchRow{
					User:         u,
					EarnedIDs:    earnedIDs,
					EarnedCounts: achCounts,
					Count:        total,
				})
			}
		} else if selectedComp != nil {
			achByUser := computeAllCompAchievements(ctx, selectedComp.ID, selectedComp.IsActive)
			for uid, earned := range achByUser {
				u, ok := visibleUsers[uid]
				if !ok {
					continue
				}
				earnedIDs := map[string]bool{}
				earnedCounts := map[string]int{}
				for _, a := range earned {
					earnedIDs[a.ID] = true
					earnedCounts[a.ID] = 1
				}
				rows = append(rows, AchRow{
					User:         u,
					EarnedIDs:    earnedIDs,
					EarnedCounts: earnedCounts,
					Count:        len(earned),
				})
			}
		}

		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Count != rows[j].Count {
				return rows[i].Count > rows[j].Count
			}
			return strings.ToLower(rows[i].User.Username) < strings.ToLower(rows[j].User.Username)
		})

		RenderTemplate(w, r, tmpl, "achievements/index.html", TemplateData{
			"User":            user,
			"Competitions":    competitions,
			"SelectedComp":    selectedComp,
			"SelectedAll":     selectedAll,
			"Rows":            rows,
			"AllAchievements": CompAchievements,
		})
	}
}
