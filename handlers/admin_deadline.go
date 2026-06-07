package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
	"tipovacka/db"
)

func AdminDeadlineAlerts(w http.ResponseWriter, r *http.Request) {
	u := RequireAdmin(w, r)
	if u == nil {
		return
	}
	if !UserCanSeeDeadline(u.ID, u.IsOwner) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"alerts":[]}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	ctx := context.Background()
	loc, _ := time.LoadLocation("Europe/Prague")
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	horizon := now.Add(3 * time.Hour)

	rows, err := db.Pool.Query(ctx, `
		SELECT m.id,
		       ht.name || ' – ' || at.name,
		       TO_CHAR(r.deadline, 'DD.MM HH24:MI'),
		       (SELECT COUNT(*) FROM users u
		        WHERE COALESCE(u.is_approved,true)=true AND COALESCE(u.is_blocked,false)=false AND COALESCE(u.is_inactive,false)=false
		          AND u.id NOT IN (SELECT user_id FROM tips WHERE match_id=m.id)) AS missing
		  FROM matches m
		  JOIN rounds r ON r.id=m.round_id
		  JOIN competitions c ON c.id=r.competition_id
		  JOIN teams ht ON ht.id=m.home_team_id
		  JOIN teams at ON at.id=m.away_team_id
		 WHERE c.is_active=true AND m.is_finished=false
		   AND r.deadline IS NOT NULL
		   AND r.deadline > $1 AND r.deadline < $2
		 ORDER BY r.deadline ASC
	`, now, horizon)
	if err != nil {
		w.Write([]byte(`{"alerts":[]}`))
		return
	}
	type alertOut struct {
		Match        string `json:"match"`
		Deadline     string `json:"deadline"`
		MissingCount int    `json:"missing_count"`
	}
	var alerts []alertOut
	for rows.Next() {
		var mid int
		var label, dl string
		var missing int
		_ = rows.Scan(&mid, &label, &dl, &missing)
		if missing > 0 {
			alerts = append(alerts, alertOut{Match: label, Deadline: dl, MissingCount: missing})
		}
	}
	rows.Close()
	if alerts == nil {
		alerts = []alertOut{}
	}
	type resp struct {
		Alerts []alertOut `json:"alerts"`
	}
	json.NewEncoder(w).Encode(resp{Alerts: alerts})
}
