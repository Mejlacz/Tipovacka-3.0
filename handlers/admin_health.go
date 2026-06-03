// handlers/admin_health.go — Tipovačka 2.0
// Diagnostický přehled pro adminy: stav DB, počty řádků, nadcházející zápasy, audit log.
package handlers

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"runtime"
	"time"

	"tipovacka/db"
)

// GET /admin/health-dashboard
func AdminHealthDashboard(tmpl *template.Template) http.HandlerFunc {
	startTime := time.Now()
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		ctx := context.Background()

		// ── DB check ───────────────────────────────────────────────────────────
		dbOK := true
		dbMsg := "OK"
		if err := db.Pool.Ping(ctx); err != nil {
			dbOK = false
			dbMsg = err.Error()
		}

		// ── Row counts ─────────────────────────────────────────────────────────
		tables := []string{"users", "competitions", "rounds", "teams", "matches", "tips"}
		counts := map[string]int{}
		for _, t := range tables {
			var n int
			db.Pool.QueryRow(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", t)).Scan(&n) //nolint
			counts[t] = n
		}

		// ── Upcoming matches ──────────────────────────────────────────────────
		type UpcomingMatch struct {
			ID       int
			Home     string
			Away     string
			Date     string
			CompName string
		}
		var upcoming []UpcomingMatch
		urows, _ := db.Pool.Query(ctx, `
			SELECT m.id, ht.name, at.name, m.match_date, c.name
			FROM matches m
			JOIN competitions c ON c.id = m.competition_id
			JOIN teams ht ON ht.id = m.home_team_id
			JOIN teams at ON at.id = m.away_team_id
			WHERE m.is_finished = false AND m.match_date IS NOT NULL
			  AND m.match_date > NOW() - INTERVAL '3 hours'
			  AND c.is_active = true
			ORDER BY m.match_date
			LIMIT 15`)
		if urows != nil {
			for urows.Next() {
				var u UpcomingMatch
				var dt *time.Time
				_ = urows.Scan(&u.ID, &u.Home, &u.Away, &dt, &u.CompName)
				if dt != nil {
					u.Date = dt.Format("02.01.2006 15:04")
				}
				upcoming = append(upcoming, u)
			}
			urows.Close()
		}

		// ── Unscored (finished but no score) ──────────────────────────────────
		var unscoredCount int
		db.Pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM matches m
			JOIN competitions c ON c.id = m.competition_id
			WHERE m.is_finished = false AND c.is_active = true
			  AND m.home_score IS NULL AND m.match_date < NOW()
		`).Scan(&unscoredCount)

		// ── Recent audit log ──────────────────────────────────────────────────
		type AuditEntry struct {
			Ts       string
			Admin    string
			Action   string
			Entity   string
			Desc     string
		}
		var recentLog []AuditEntry
		lrows, _ := db.Pool.Query(ctx, `
			SELECT timestamp, COALESCE(admin_username,'—'), action, COALESCE(entity_type,'—'),
			       COALESCE(description,'')
			FROM audit_log ORDER BY timestamp DESC LIMIT 15`)
		if lrows != nil {
			for lrows.Next() {
				var e AuditEntry
				var ts *time.Time
				_ = lrows.Scan(&ts, &e.Admin, &e.Action, &e.Entity, &e.Desc)
				if ts != nil {
					e.Ts = ts.Format("02.01 15:04")
				}
				recentLog = append(recentLog, e)
			}
			lrows.Close()
		}

		// ── Active competitions ────────────────────────────────────────────────
		type ActiveComp struct {
			ID       int
			Name     string
			Matches  int
			Unscored int
		}
		var activeComps []ActiveComp
		acRows, _ := db.Pool.Query(ctx, `SELECT id, name FROM competitions WHERE is_active ORDER BY id DESC`)
		if acRows != nil {
			for acRows.Next() {
				var ac ActiveComp
				_ = acRows.Scan(&ac.ID, &ac.Name)
				db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM matches WHERE competition_id=$1`, ac.ID).Scan(&ac.Matches)
				db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM matches WHERE competition_id=$1 AND is_finished=false AND home_score IS NULL`, ac.ID).Scan(&ac.Unscored)
				activeComps = append(activeComps, ac)
			}
			acRows.Close()
		}

		// ── System info ───────────────────────────────────────────────────────
		var memStats runtime.MemStats
		runtime.ReadMemStats(&memStats)
		uptime := time.Since(startTime)
		uptimeStr := fmt.Sprintf("%dh %dm %ds", int(uptime.Hours()), int(uptime.Minutes())%60, int(uptime.Seconds())%60)

		RenderTemplate(w, r, tmpl, "admin/health.html", TemplateData{
			"User":          admin,
			"DBOk":          dbOK,
			"DBMsg":         dbMsg,
			"Counts":        counts,
			"Upcoming":      upcoming,
			"UnscoredCount": unscoredCount,
			"RecentLog":     recentLog,
			"ActiveComps":   activeComps,
			"UptimeStr":     uptimeStr,
			"GoVersion":     runtime.Version(),
			"NumGoroutine":  runtime.NumGoroutine(),
			"HeapAllocMB":   float64(memStats.HeapAlloc) / 1024 / 1024,
			"Now":           time.Now().Format("02.01.2006 15:04:05"),
		})
	}
}
