// handlers/admin_neon_sync.go — Tipovačka 2.0
// Admin UI pro ruční Neon sync + zobrazení logu.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"tipovacka/config"
	"tipovacka/db"
	"tipovacka/middleware"
)

// tabulky v FK pořadí
var neonSyncTables = []string{
	"users",
	"competitions",
	"teams",
	"rounds",
	"extra_questions",
	"matches",
	"tips",
	"extra_answers",
	"competition_teams",
	"notification_settings",
	"push_subscriptions",
	"vapid_config",
	"backup_schedule",
	"site_config",
	"audit_log",
}

// InitNeonSyncTables vytvoří tabulky neon_sync_log a neon_sync_config pokud neexistují.
func InitNeonSyncTables() {
	ctx := context.Background()
	_, err := db.Pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS neon_sync_log (
		  id SERIAL PRIMARY KEY,
		  started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		  finished_at TIMESTAMPTZ,
		  status TEXT NOT NULL DEFAULT 'running',
		  triggered_by TEXT NOT NULL DEFAULT 'manual',
		  message TEXT NOT NULL DEFAULT '',
		  row_counts TEXT NOT NULL DEFAULT '{}'
		);
		CREATE TABLE IF NOT EXISTS neon_sync_config (
		  id INTEGER PRIMARY KEY DEFAULT 1,
		  enabled BOOLEAN NOT NULL DEFAULT false,
		  auto_hour INTEGER NOT NULL DEFAULT 3,
		  last_sync_at TIMESTAMPTZ,
		  last_sync_status TEXT,
		  last_sync_msg TEXT,
		  last_row_counts TEXT DEFAULT '{}'
		);
		INSERT INTO neon_sync_config (id) VALUES (1) ON CONFLICT DO NOTHING;
	`)
	if err != nil {
		log.Printf("[neon_sync] InitNeonSyncTables error: %v", err)
	}
}

// NeonSyncConfig obsahuje nastavení z DB.
type NeonSyncConfig struct {
	Enabled        bool
	AutoHour       int
	LastSyncAt     *time.Time
	LastSyncStatus *string
	LastSyncMsg    *string
	LastRowCounts  string
}

// NeonSyncLogEntry je řádek z neon_sync_log.
type NeonSyncLogEntry struct {
	ID          int
	StartedAt   time.Time
	FinishedAt  *time.Time
	Status      string
	TriggeredBy string
	Message     string
	RowCounts   string
	DurationSec float64
}

func loadNeonSyncConfig(ctx context.Context) NeonSyncConfig {
	var cfg NeonSyncConfig
	cfg.LastRowCounts = "{}"
	_ = db.Pool.QueryRow(ctx, `
		SELECT enabled, auto_hour, last_sync_at, last_sync_status, last_sync_msg, COALESCE(last_row_counts,'{}')
		FROM neon_sync_config WHERE id=1
	`).Scan(&cfg.Enabled, &cfg.AutoHour, &cfg.LastSyncAt, &cfg.LastSyncStatus, &cfg.LastSyncMsg, &cfg.LastRowCounts)
	return cfg
}

func loadNeonSyncLog(ctx context.Context, limit int) []NeonSyncLogEntry {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, started_at, finished_at, status, triggered_by, message, COALESCE(row_counts,'{}')
		FROM neon_sync_log ORDER BY id DESC LIMIT $1
	`, limit)
	if err != nil {
		log.Printf("[neon_sync] log query error: %v", err)
		return nil
	}
	defer rows.Close()

	var entries []NeonSyncLogEntry
	for rows.Next() {
		var e NeonSyncLogEntry
		_ = rows.Scan(&e.ID, &e.StartedAt, &e.FinishedAt, &e.Status, &e.TriggeredBy, &e.Message, &e.RowCounts)
		if e.FinishedAt != nil {
			e.DurationSec = e.FinishedAt.Sub(e.StartedAt).Seconds()
		}
		entries = append(entries, e)
	}
	return entries
}

// hourOption je jedna položka pro select hodiny.
type hourOption struct {
	Val      int
	Label    string
	Selected bool
}

// NeonSyncLogView je řádek logu připravený pro šablonu.
type NeonSyncLogView struct {
	ID           int
	StartedAtStr string
	FinishedAtStr string
	Status       string
	TriggeredBy  string
	Message      string
	DurationSec  float64
}

// GET /admin/neon-sync
func AdminNeonSync(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		ctx := context.Background()

		cfg := loadNeonSyncConfig(ctx)
		rawLog := loadNeonSyncLog(ctx, 20)
		flash := middleware.GetFlash(w, r)

		// Připrav log pro šablonu
		var logViews []NeonSyncLogView
		for _, e := range rawLog {
			v := NeonSyncLogView{
				ID:           e.ID,
				StartedAtStr: e.StartedAt.In(pragueLocation).Format("02.01.2006 15:04:05"),
				Status:       e.Status,
				TriggeredBy:  e.TriggeredBy,
				Message:      e.Message,
				DurationSec:  e.DurationSec,
			}
			if e.FinishedAt != nil {
				v.FinishedAtStr = e.FinishedAt.In(pragueLocation).Format("02.01.2006 15:04:05")
			}
			logViews = append(logViews, v)
		}

		// Hodiny pro select
		hours := make([]hourOption, 24)
		for i := 0; i < 24; i++ {
			hours[i] = hourOption{
				Val:      i,
				Label:    fmt.Sprintf("%02d:00", i),
				Selected: i == cfg.AutoHour,
			}
		}

		// Poslední sync info
		var lastSyncAt, lastSyncStatus, lastSyncMsg string
		if cfg.LastSyncAt != nil {
			lastSyncAt = cfg.LastSyncAt.In(pragueLocation).Format("02.01.2006 15:04:05")
		}
		if cfg.LastSyncStatus != nil {
			lastSyncStatus = *cfg.LastSyncStatus
		}
		if cfg.LastSyncMsg != nil {
			lastSyncMsg = *cfg.LastSyncMsg
		}

		RenderTemplate(w, r, tmpl, "admin/neon_sync.html", TemplateData{
			"User":           admin,
			"ConfigEnabled":  cfg.Enabled,
			"Log":            logViews,
			"Flash":          flash,
			"NeonURLSet":     config.NeonBackupURL != "",
			"Hours":          hours,
			"LastSyncAt":     lastSyncAt,
			"LastSyncStatus": lastSyncStatus,
			"LastSyncMsg":    lastSyncMsg,
		})
	}
}

// POST /admin/neon-sync/run
func AdminNeonSyncRun(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}

	if config.NeonBackupURL == "" {
		middleware.SetFlash(w, r, "error", "NEON_BACKUP_URL není nastavena v env proměnných.")
		http.Redirect(w, r, "/admin/neon-sync", http.StatusSeeOther)
		return
	}

	middleware.SetFlash(w, r, "ok", "Záloha spuštěna na pozadí…")
	go func() {
		total, err := RunNeonSync("manual")
		if err != nil {
			log.Printf("[neon_sync] záloha selhala: %v", err)
		} else {
			log.Printf("[neon_sync] záloha dokončena, celkem %d řádků", total)
		}
	}()

	http.Redirect(w, r, "/admin/neon-sync", http.StatusSeeOther)
}

// POST /admin/neon-sync/settings
func AdminNeonSyncSettings(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	enabled := r.FormValue("enabled") == "on" || r.FormValue("enabled") == "true"
	autoHour, _ := strconv.Atoi(r.FormValue("auto_hour"))
	if autoHour < 0 || autoHour > 23 {
		autoHour = 3
	}

	ctx := context.Background()
	_, err := db.Pool.Exec(ctx,
		`UPDATE neon_sync_config SET enabled=$1, auto_hour=$2 WHERE id=1`,
		enabled, autoHour)
	if err != nil {
		log.Printf("[neon_sync] settings save error: %v", err)
		middleware.SetFlash(w, r, "error", "Nepodařilo se uložit nastavení.")
	} else {
		middleware.SetFlash(w, r, "ok", "Nastavení uloženo.")
	}

	http.Redirect(w, r, "/admin/neon-sync", http.StatusSeeOther)
}

// ─── Sync logika ──────────────────────────────────────────────────────────────

// RunNeonSync spustí import dat CockroachDB (Tip 2.0) → Neon (Tip 3.0).
// Vrátí celkový počet zkopírovaných řádků a chybu.
func RunNeonSync(triggeredBy string) (int, error) {
	ctx := context.Background()
	startedAt := time.Now()

	// Vlož log záznam se stavem "running"
	var logID int
	err := db.Pool.QueryRow(ctx, `
		INSERT INTO neon_sync_log (started_at, status, triggered_by, message, row_counts)
		VALUES ($1, 'running', $2, 'Záloha spuštěna…', '{}')
		RETURNING id
	`, startedAt, triggeredBy).Scan(&logID)
	if err != nil {
		log.Printf("[neon_sync] nelze vložit log: %v", err)
		logID = 0
	}

	finish := func(status, message string, counts map[string]int) (int, error) {
		finishedAt := time.Now()
		duration := finishedAt.Sub(startedAt).Seconds()
		fullMsg := fmt.Sprintf("%s (trvalo %.1fs)", message, duration)
		if len(fullMsg) > 490 {
			fullMsg = fullMsg[:490]
		}

		countsJSON, _ := json.Marshal(counts)

		if logID > 0 {
			_, _ = db.Pool.Exec(ctx, `
				UPDATE neon_sync_log SET finished_at=$1, status=$2, message=$3, row_counts=$4
				WHERE id=$5
			`, finishedAt, status, fullMsg, string(countsJSON), logID)
		}

		// Aktualizuj config
		_, _ = db.Pool.Exec(ctx, `
			UPDATE neon_sync_config SET
				last_sync_at=$1, last_sync_status=$2, last_sync_msg=$3, last_row_counts=$4
			WHERE id=1
		`, finishedAt, status, fullMsg, string(countsJSON))

		total := 0
		for _, n := range counts {
			total += n
		}
		if status == "ok" {
			return total, nil
		}
		return total, fmt.Errorf("%s", message)
	}

	// Připoj se ke CockroachDB (zdroj = Tipovačka 2.0)
	cockPool, err := pgxpool.New(ctx, config.NeonBackupURL)
	if err != nil {
		return finish("error", fmt.Sprintf("Nelze se připojit ke CockroachDB: %v", err), nil)
	}
	defer cockPool.Close()

	if err := cockPool.Ping(ctx); err != nil {
		return finish("error", fmt.Sprintf("CockroachDB ping selhal: %v", err), nil)
	}

	// Zkopíruj každou tabulku: CockroachDB (zdroj) → Neon (cíl = db.Pool)
	counts := map[string]int{}
	for _, table := range neonSyncTables {
		n, err := copyTableNeon(ctx, cockPool, db.Pool, table)
		if err != nil {
			log.Printf("[neon_sync] chyba při kopírování '%s': %v", table, err)
			counts[table] = -1
			continue
		}
		counts[table] = n
	}

	total := 0
	for _, n := range counts {
		if n > 0 {
			total += n
		}
	}

	return finish("ok",
		fmt.Sprintf("✅ Import dokončen — %d řádků ve %d tabulkách", total, len(neonSyncTables)),
		counts)
}

// getTableColumns vrátí seznam sloupců tabulky v daném poolu.
func getTableColumns(ctx context.Context, pool *pgxpool.Pool, table string) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT column_name FROM information_schema.columns
		WHERE table_schema='public' AND table_name=$1
		ORDER BY ordinal_position
	`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var col string
		_ = rows.Scan(&col)
		cols = append(cols, col)
	}
	return cols, nil
}

// copyTableNeon zkopíruje tabulku ze src (CRDB) do dst (Neon).
// Zjistí průnik sloupců, smaže dst, vloží ze src po dávkách 200.
func copyTableNeon(ctx context.Context, src, dst *pgxpool.Pool, table string) (int, error) {
	srcCols, err := getTableColumns(ctx, src, table)
	if err != nil {
		return 0, fmt.Errorf("src columns: %v", err)
	}
	dstColList, err := getTableColumns(ctx, dst, table)
	if err != nil {
		// Tabulka v Neonu neexistuje — přeskočit
		return 0, nil
	}
	dstColSet := map[string]bool{}
	for _, c := range dstColList {
		dstColSet[c] = true
	}

	// Průnik sloupců
	var cols []string
	for _, c := range srcCols {
		if dstColSet[c] {
			cols = append(cols, c)
		}
	}
	if len(cols) == 0 {
		return 0, nil
	}

	// Quoted column list
	quotedCols := make([]string, len(cols))
	for i, c := range cols {
		quotedCols[i] = `"` + c + `"`
	}
	colsSQL := strings.Join(quotedCols, ", ")

	// Načti data ze src
	srcRows, err := src.Query(ctx, fmt.Sprintf(`SELECT %s FROM "%s"`, colsSQL, table))
	if err != nil {
		return 0, fmt.Errorf("src query: %v", err)
	}
	defer srcRows.Close()

	var allRows [][]interface{}
	for srcRows.Next() {
		vals, err := srcRows.Values()
		if err != nil {
			return 0, fmt.Errorf("src scan: %v", err)
		}
		allRows = append(allRows, vals)
	}
	srcRows.Close()

	// Smaž v dst
	_, err = dst.Exec(ctx, fmt.Sprintf(`DELETE FROM "%s"`, table))
	if err != nil {
		log.Printf("[neon_sync] DELETE FROM %s: %v (pokračuji)", table, err)
	}

	if len(allRows) == 0 {
		return 0, nil
	}

	// Vlož po dávkách 200
	const batchSize = 200
	total := 0
	for i := 0; i < len(allRows); i += batchSize {
		end := i + batchSize
		if end > len(allRows) {
			end = len(allRows)
		}
		batch := allRows[i:end]

		// Sestav INSERT ... VALUES ($1,$2,...), ($N,...), ...
		var valueParts []string
		var flatVals []interface{}
		ph := 1
		for _, row := range batch {
			var placeholders []string
			for range row {
				placeholders = append(placeholders, fmt.Sprintf("$%d", ph))
				ph++
			}
			valueParts = append(valueParts, "("+strings.Join(placeholders, ",")+")")
			flatVals = append(flatVals, row...)
		}

		insertSQL := fmt.Sprintf(
			`INSERT INTO "%s" (%s) VALUES %s ON CONFLICT DO NOTHING`,
			table, colsSQL, strings.Join(valueParts, ","),
		)
		_, err := dst.Exec(ctx, insertSQL, flatVals...)
		if err != nil {
			return total, fmt.Errorf("insert batch: %v", err)
		}
		total += len(batch)
	}

	return total, nil
}
