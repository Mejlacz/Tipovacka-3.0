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

	// Deadline upozornění — použij competition.deadline nebo match_date
	rows, err := db.Pool.Query(ctx, `
		SELECT m.id,
		       ht.name || ' – ' || at.name,
		       COALESCE(
		           TO_CHAR(c.deadline, 'DD.MM HH24:MI'),
		           TO_CHAR(m.match_date, 'DD.MM HH24:MI')
		       ),
		       COALESCE(c.deadline, m.match_date) AS effective_deadline,
		       (SELECT COUNT(*) FROM users u
		        WHERE COALESCE(u.is_approved,true)=true AND COALESCE(u.is_blocked,false)=false AND COALESCE(u.is_inactive,false)=false
		          AND u.id NOT IN (SELECT user_id FROM tips WHERE match_id=m.id)) AS missing
		  FROM matches m
		  JOIN competitions c ON c.id = m.competition_id
		  JOIN teams ht ON ht.id = m.home_team_id
		  JOIN teams at ON at.id = m.away_team_id
		 WHERE c.is_active=true AND m.is_finished=false
		   AND COALESCE(c.deadline, m.match_date) IS NOT NULL
		   AND COALESCE(c.deadline, m.match_date) > $1
		   AND COALESCE(c.deadline, m.match_date) < $2
		 ORDER BY COALESCE(c.deadline, m.match_date) ASC
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
		var effectiveDL time.Time
		var missing int
		_ = rows.Scan(&mid, &label, &dl, &effectiveDL, &missing)
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
