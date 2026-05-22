// handlers/admin_time.go — Tipovačka 2.0
// Diagnostická stránka časových pásem + manuální override pro ownera.
package handlers

import (
	"context"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"time"

	"tipovacka/db"
	"tipovacka/middleware"
)

// LoadTZOverrideFromDB načte při startu uložený TZ offset z app_config.
func LoadTZOverrideFromDB() {
	var val string
	err := db.Pool.QueryRow(context.Background(),
		`SELECT value FROM app_config WHERE key='tz_offset_minutes'`).Scan(&val)
	if err != nil {
		// Žádný override nastaven — OK
		return
	}
	minutes, err := strconv.Atoi(val)
	if err != nil {
		log.Printf("[time] app_config tz_offset_minutes má neplatnou hodnotu: %q", val)
		return
	}
	SetTZOffsetOverride(&minutes)
	log.Printf("[time] Načten TZ override: %+d minut od UTC", minutes)
}

// ─── GET /admin/time ─────────────────────────────────────────────────────────

func AdminTimeDiag(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		ctx := context.Background()

		// 1. Serverový UTC čas
		utcNow := time.Now().UTC()

		// 2. Pražský čas z location (Europe/Prague)
		pragLoc, locErr := time.LoadLocation("Europe/Prague")
		var pragueFromLoc time.Time
		var pragueLocErrStr string
		if locErr != nil {
			pragueLocErrStr = locErr.Error()
			pragueFromLoc = utcNow // fallback
		} else {
			pragueFromLoc = utcNow.In(pragLoc)
		}

		// 3. Čas v databázi (NOW() s timezone)
		var dbNow time.Time
		_ = db.Pool.QueryRow(ctx, `SELECT NOW()`).Scan(&dbNow)

		// 4. Čas v DB přepočtený na Prahu (co DB "ví" o pražském čase)
		var dbPragueNow time.Time
		_ = db.Pool.QueryRow(ctx, `SELECT NOW() AT TIME ZONE 'Europe/Prague'`).Scan(&dbPragueNow)

		// 5. NowPrague() — co app používá pro deadline porovnání
		appNowPrague := NowPrague()

		// 6. Příklad: nejbližší nadcházející zápas — jak se zobrazuje
		type matchExample struct {
			ID          int
			Home        string
			Away        string
			RawInDB     time.Time  // jak pgx vrátí z DB
			FmtPrague   string     // jak to zobrazí tipovačka (fmtPrague = t.Format)
			FmtISO      string     // co jde do JS countdown
			IsBeforeNow bool       // deadline ještě neuplynul?
		}
		var example *matchExample
		var rawDate time.Time
		var homeN, awayN string
		var exID int
		err := db.Pool.QueryRow(ctx, `
			SELECT m.id, ht.name, at.name, m.match_date
			FROM matches m
			JOIN teams ht ON ht.id = m.home_team_id
			JOIN teams at ON at.id = m.away_team_id
			WHERE m.is_finished = false AND m.match_date IS NOT NULL
			ORDER BY m.match_date ASC LIMIT 1
		`).Scan(&exID, &homeN, &awayN, &rawDate)
		if err == nil {
			// fmtPrague = t.Format (wall-clock as-is)
			fp := rawDate.Format("02.01.2006 15:04")
			// fmtISO: rebuild as Prague real time
			var prgTime time.Time
			if pragLoc != nil {
				prgTime = time.Date(rawDate.Year(), rawDate.Month(), rawDate.Day(),
					rawDate.Hour(), rawDate.Minute(), rawDate.Second(), rawDate.Nanosecond(), pragLoc)
			} else {
				prgTime = rawDate
			}
			fi := prgTime.Format(time.RFC3339)
			example = &matchExample{
				ID:          exID,
				Home:        homeN,
				Away:        awayN,
				RawInDB:     rawDate,
				FmtPrague:   fp,
				FmtISO:      fi,
				IsBeforeNow: appNowPrague.Before(rawDate),
			}
		}

		// 7. Shoda — varování pokud NowPrague wall-clock ≠ Prague from location
		locMin := pragueFromLoc.Hour()*60 + pragueFromLoc.Minute()
		appMin := appNowPrague.Hour()*60 + appNowPrague.Minute()
		diff := locMin - appMin
		if diff < 0 {
			diff = -diff
		}
		timeInSync := diff <= 1 // tolerance 1 minuta (kvůli přelomu sekundy)

		// 8. Aktuální override
		override := GetTZOffsetOverride()

		flash := middleware.GetFlash(w, r)
		RenderTemplate(w, r, tmpl, "admin/time.html", TemplateData{
			"User":             admin,
			"Flash":            flash,
			"UTCNow":           utcNow,
			"PragueFromLoc":    pragueFromLoc,
			"PragueLocErr":     pragueLocErrStr,
			"DBNow":            dbNow,
			"DBPragueNow":      dbPragueNow,
			"AppNowPrague":     appNowPrague,
			"TimeInSync":       timeInSync,
			"DiffMinutes":      diff,
			"Example":          example,
			"TZOverride":       override,
			"PragueOffsetNow":  int(pragueFromLoc.Sub(utcNow).Minutes()), // aktuální offset Praha vs UTC
		})
	}
}

// ─── POST /admin/time/offset ─────────────────────────────────────────────────

func AdminTimeSetOffset(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	if !admin.IsOwner {
		http.Error(w, "403 — jen pro Owner", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	ctx := context.Background()
	action := r.FormValue("action")

	if action == "clear" {
		// Smaž override, použij automatický Europe/Prague
		_, _ = db.Pool.Exec(ctx, `DELETE FROM app_config WHERE key='tz_offset_minutes'`)
		SetTZOffsetOverride(nil)
		LogAction(&admin.ID, admin.Username, "tz_override_clear", "config", nil,
			"TZ override vymazán — zpět na automatický Europe/Prague", nil, nil)
		middleware.SetFlash(w, r, "ok", "TZ override vymazán. Používá se automatický Europe/Prague.")
	} else {
		offsetStr := r.FormValue("offset_minutes")
		minutes, err := strconv.Atoi(offsetStr)
		if err != nil || minutes < -720 || minutes > 720 {
			middleware.SetFlash(w, r, "error", "Neplatný offset (zadej celé číslo minut, např. 120 pro UTC+2).")
			http.Redirect(w, r, "/admin/time", http.StatusSeeOther)
			return
		}
		// Ulož do DB a nastav do paměti
		_, _ = db.Pool.Exec(ctx,
			`INSERT INTO app_config (key, value) VALUES ('tz_offset_minutes', $1)
			 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
			strconv.Itoa(minutes))
		SetTZOffsetOverride(&minutes)
		newVal := strconv.Itoa(minutes)
		LogAction(&admin.ID, admin.Username, "tz_override_set", "config", nil,
			"TZ override nastaven", nil, &newVal)
		middleware.SetFlash(w, r, "ok",
			"TZ override nastaven: UTC"+formatOffset(minutes)+". NowPrague() nyní používá tento offset.")
	}
	http.Redirect(w, r, "/admin/time", http.StatusSeeOther)
}

func formatOffset(minutes int) string {
	sign := "+"
	if minutes < 0 {
		sign = "-"
		minutes = -minutes
	}
	h := minutes / 60
	m := minutes % 60
	if m == 0 {
		return sign + strconv.Itoa(h)
	}
	return sign + strconv.Itoa(h) + ":" + strconv.Itoa(m)
}
