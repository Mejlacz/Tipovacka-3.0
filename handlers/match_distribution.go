package handlers

import (
	"context"
	"encoding/json"
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

	// Zjistit, zda zápas už začal nebo je dokončen
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

	// Tipy zobrazíme jen po startu nebo dohrání zápasu
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
	awayPct := 100 - homePct - drawPct // aby součet byl přesně 100

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
