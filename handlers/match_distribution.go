package handlers

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"tipovacka/db"
)

// MatchTipDistribution vrací agregaci tipů pro daný zápas (po jeho startu).
// GET /api/match/{match_id}/tip-distribution
func MatchTipDistribution(w http.ResponseWriter, r *http.Request) {
	u := RequireLogin(w, r)
	if u == nil {
		return
	}

	matchIDStr := chi.URLParam(r, "match_id")
	matchID, err := strconv.Atoi(matchIDStr)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	ctx := context.Background()

	var isFinished bool
	var matchDate *time.Time
	err = db.Pool.QueryRow(ctx,
		`SELECT is_finished, match_date FROM matches WHERE id = $1`, matchID,
	).Scan(&isFinished, &matchDate)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error":"not found"}`))
		return
	}

	now := time.Now()
	started := isFinished || (matchDate != nil && matchDate.Before(now))
	if !started {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"hidden":true}`))
		return
	}

	var homeWins, draws, awayWins, total int
	err = db.Pool.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN home_score > away_score THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN home_score = away_score THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN home_score < away_score THEN 1 ELSE 0 END), 0),
			COUNT(*)
		FROM tips WHERE match_id = $1
	`, matchID).Scan(&homeWins, &draws, &awayWins, &total)
	if err != nil || total == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"total":0}`))
		return
	}

	type resp struct {
		HomePct  int `json:"home_pct"`
		DrawPct  int `json:"draw_pct"`
		AwayPct  int `json:"away_pct"`
		HomeWins int `json:"home_wins"`
		Draws    int `json:"draws"`
		AwayWins int `json:"away_wins"`
		Total    int `json:"total"`
	}

	homePct := homeWins * 100 / total
	drawPct := draws * 100 / total
	awayPct := 100 - homePct - drawPct

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp{
		HomePct:  homePct,
		DrawPct:  drawPct,
		AwayPct:  awayPct,
		HomeWins: homeWins,
		Draws:    draws,
		AwayWins: awayWins,
		Total:    total,
	})
}

// RoundSummary vrací shrnutí kola — top hráči, statistiky.
// GET /api/round/{round_id}/summary
func RoundSummary(w http.ResponseWriter, r *http.Request) {
	u := RequireLogin(w, r)
	if u == nil {
		return
	}

	roundIDStr := chi.URLParam(r, "round_id")
	roundID, err := strconv.Atoi(roundIDStr)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	ctx := context.Background()
	w.Header().Set("Content-Type", "application/json")

	// Zkontrolovat zda je kolo dokončeno
	var total, finished int
	db.Pool.QueryRow(ctx,
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN is_finished THEN 1 ELSE 0 END),0)
		   FROM matches WHERE round_id = $1`, roundID,
	).Scan(&total, &finished)

	if total == 0 || finished < total {
		json.NewEncoder(w).Encode(map[string]interface{}{"finished": false})
		return
	}

	type TopScorer struct {
		UserID   int    `json:"user_id"`
		Username string `json:"username"`
		Points   int    `json:"points"`
		Exact    int    `json:"exact"`
	}

	rows, err := db.Pool.Query(ctx, `
		SELECT u.id, u.username,
		       SUM(t.points)                                        AS pts,
		       SUM(CASE WHEN t.points = 3 THEN 1 ELSE 0 END)       AS exact_cnt
		  FROM tips t
		  JOIN matches m ON m.id = t.match_id
		  JOIN users u   ON u.id = t.user_id
		 WHERE m.round_id = $1
		   AND COALESCE(u.is_approved, true) = true
		   AND COALESCE(u.is_blocked, false) = false
		   AND COALESCE(u.is_inactive, false) = false
		   AND COALESCE(u.is_hidden, false) = false
		   AND t.points IS NOT NULL
		 GROUP BY u.id, u.username
		 ORDER BY pts DESC, exact_cnt DESC
		 LIMIT 5`, roundID)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"finished": false})
		return
	}
	defer rows.Close()

	var topScorers []TopScorer
	for rows.Next() {
		var ts TopScorer
		rows.Scan(&ts.UserID, &ts.Username, &ts.Points, &ts.Exact)
		topScorers = append(topScorers, ts)
	}

	// Globální statistiky kola
	var tipCount, exactCount, participants int
	var avgPts float64
	db.Pool.QueryRow(ctx, `
		SELECT COUNT(t.id),
		       COALESCE(SUM(CASE WHEN t.points = 3 THEN 1 ELSE 0 END), 0),
		       COUNT(DISTINCT t.user_id),
		       COALESCE(AVG(sub.pts), 0)
		  FROM tips t
		  JOIN matches m ON m.id = t.match_id
		  JOIN (SELECT t2.user_id, SUM(t2.points) AS pts
		          FROM tips t2
		          JOIN matches m2 ON m2.id = t2.match_id
		         WHERE m2.round_id = $1 AND t2.points IS NOT NULL
		         GROUP BY t2.user_id) sub ON sub.user_id = t.user_id
		 WHERE m.round_id = $1 AND t.points IS NOT NULL`,
		roundID,
	).Scan(&tipCount, &exactCount, &participants, &avgPts)

	type resp struct {
		Finished     bool        `json:"finished"`
		TopScorers   []TopScorer `json:"top_scorers"`
		TipCount     int         `json:"tip_count"`
		ExactCount   int         `json:"exact_count"`
		Participants int         `json:"participants"`
		AvgPts       float64     `json:"avg_pts"`
		MatchCount   int         `json:"match_count"`
	}

	if topScorers == nil {
		topScorers = []TopScorer{}
	}

	json.NewEncoder(w).Encode(resp{
		Finished:     true,
		TopScorers:   topScorers,
		TipCount:     tipCount,
		ExactCount:   exactCount,
		Participants: participants,
		AvgPts:       math.Round(avgPts*10) / 10,
		MatchCount:   finished,
	})
}

// StatsPerRound vrací přesnost tipů per kolo pro daného uživatele.
// GET /api/stats/{comp_id}/per-round?user_id=N
func StatsPerRound(w http.ResponseWriter, r *http.Request) {
	u := RequireApproved(w, r)
	if u == nil {
		return
	}

	compIDStr := chi.URLParam(r, "comp_id")
	compID, err := strconv.Atoi(compIDStr)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Volitelný user_id — výchozí je přihlášený
	userID := u.ID
	if uidStr := r.URL.Query().Get("user_id"); uidStr != "" {
		if uid, e := strconv.Atoi(uidStr); e == nil && uid > 0 {
			userID = uid
		}
	}

	ctx := context.Background()
	w.Header().Set("Content-Type", "application/json")

	rows, err := db.Pool.Query(ctx, `
		SELECT r.name,
		       COUNT(t.id)                                         AS tips,
		       COALESCE(SUM(CASE WHEN t.points = 3 THEN 1 ELSE 0 END), 0) AS exact_cnt,
		       COALESCE(SUM(CASE WHEN t.points = 1 THEN 1 ELSE 0 END), 0) AS winner_cnt,
		       COALESCE(SUM(CASE WHEN t.points = 0 THEN 1 ELSE 0 END), 0) AS miss_cnt,
		       COALESCE(SUM(t.points), 0)                         AS pts
		  FROM rounds r
		  JOIN matches m ON m.round_id = r.id
		  JOIN tips t    ON t.match_id = m.id
		 WHERE r.competition_id = $1
		   AND t.user_id = $2
		   AND t.points IS NOT NULL
		 GROUP BY r.id, r.name
		 ORDER BY r.id ASC`, compID, userID)
	if err != nil {
		w.Write([]byte(`{"rounds":[]}`))
		return
	}
	defer rows.Close()

	type RoundRow struct {
		Name      string  `json:"name"`
		Tips      int     `json:"tips"`
		ExactCnt  int     `json:"exact"`
		WinnerCnt int     `json:"winner"`
		MissCnt   int     `json:"miss"`
		Pts       int     `json:"pts"`
		ExactPct  float64 `json:"exact_pct"`
	}

	var rounds []RoundRow
	for rows.Next() {
		var rr RoundRow
		rows.Scan(&rr.Name, &rr.Tips, &rr.ExactCnt, &rr.WinnerCnt, &rr.MissCnt, &rr.Pts)
		if rr.Tips > 0 {
			rr.ExactPct = math.Round(float64(rr.ExactCnt)*100/float64(rr.Tips)*10) / 10
		}
		rounds = append(rounds, rr)
	}

	if rounds == nil {
		rounds = []RoundRow{}
	}

	type resp struct {
		Rounds []RoundRow `json:"rounds"`
	}
	json.NewEncoder(w).Encode(resp{Rounds: rounds})
}
