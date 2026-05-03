// handlers/admin_api_import.go — Tipovačka 2.0
// Import zápasů z football-data.org API (v4).
//
// Endpointy:
//   GET  /admin/api/competitions — JSON seznam dostupných soutěží z API
//   GET  /admin/api/rounds       — JSON seznam kol pro vybranou soutěž (AJAX)
//   GET  /admin/api/preview      — JSON náhled zápasů z API (AJAX)
//   POST /admin/api/import       — skutečný import do DB
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tipovacka/config"
	"tipovacka/db"
	"tipovacka/middleware"
)

// ── football-data.org API structs ─────────────────────────────────────────────

type fdMatchList struct {
	Matches []fdMatch `json:"matches"`
}

type fdMatch struct {
	ID       int    `json:"id"`
	UtcDate  string `json:"utcDate"`
	Status   string `json:"status"`
	Matchday *int   `json:"matchday"`
	HomeTeam fdTeam `json:"homeTeam"`
	AwayTeam fdTeam `json:"awayTeam"`
	Score    fdScore `json:"score"`
}

type fdTeam struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	ShortName string `json:"shortName"`
	TLA       string `json:"tla"`
}

type fdScore struct {
	// Duration: "REGULAR" | "EXTRA_TIME" | "PENALTY_SHOOTOUT"
	// Pro tipovačku vždy bereme skóre po základní době:
	//   fotbal = po 90 min  →  score.fullTime
	//   hokej  = po 60 min  →  score.fullTime (analogicky)
	// Prodloužení a penalty do výsledků NEVSTUPUJÍ.
	Duration  string  `json:"duration"`
	FullTime  fdGoals `json:"fullTime"`  // skóre po základní době (90/60 min)
	ExtraTime fdGoals `json:"extraTime"` // po prodloužení — záměrně nepoužíváme
	Penalties fdGoals `json:"penalties"` // penalty shootout — záměrně nepoužíváme
	HalfTime  fdGoals `json:"halfTime"`  // informativní, nepoužíváme pro výsledek
}

type fdGoals struct {
	Home *int `json:"home"`
	Away *int `json:"away"`
}

// regularTimeScore vrátí skóre po základní době (fullTime) — ignoruje prodloužení a penalty.
// football-data.org v4: score.fullTime je vždy výsledek po 90 min,
// bez ohledu na to zda se hrálo prodloužení (extraTime/penalties se ukládají zvlášť).
func regularTimeScore(s fdScore) (*int, *int) {
	return s.FullTime.Home, s.FullTime.Away
}

type fdCompetitionList struct {
	Competitions []fdCompetition `json:"competitions"`
}

type fdCompetition struct {
	ID   int    `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
	Area struct {
		Name string `json:"name"`
	} `json:"area"`
	Type string `json:"type"`
}

// fdCall volá football-data.org API a dekóduje JSON do dst.
func fdCall(path string, dst interface{}) error {
	if config.FootballAPIKey == "" {
		return fmt.Errorf("FOOTBALL_API_KEY není nastaven")
	}
	url := "https://api.football-data.org/v4/" + strings.TrimPrefix(path, "/")
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Auth-Token", config.FootballAPIKey)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		if resp.StatusCode == 429 {
			return fmt.Errorf("API limit překročen (429) — počkej chvíli a zkus znovu (free tier: 10 req/min)")
		}
		// Pokus o čtení chybové zprávy z API
		var apiErr struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &apiErr)
		msg := apiErr.Message
		if msg == "" {
			msg = string(body)
			if len(msg) > 200 {
				msg = msg[:200]
			}
		}
		return fmt.Errorf("API %d: %s", resp.StatusCode, msg)
	}
	return json.Unmarshal(body, dst)
}

// ── GET /admin/api/rounds ─────────────────────────────────────────────────────
// Vrátí JSON seznam kol pro zadanou soutěž.
// Query params: competition_id

func AdminAPIRounds(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	admin := RequireAdmin(w, r)
	if admin == nil {
		w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
		return
	}

	compID, _ := strconv.Atoi(r.URL.Query().Get("competition_id"))
	if compID == 0 {
		w.Write([]byte(`{"ok":true,"rounds":[]}`))
		return
	}

	ctx := context.Background()
	rows, err := db.Pool.Query(ctx,
		`SELECT id, name FROM rounds WHERE competition_id=$1 ORDER BY id`, compID)
	if err != nil {
		b, _ := json.Marshal(map[string]interface{}{"ok": false, "error": err.Error()})
		w.Write(b)
		return
	}
	defer rows.Close()

	type roundItem struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	var rounds []roundItem
	for rows.Next() {
		var ri roundItem
		if err := rows.Scan(&ri.ID, &ri.Name); err == nil {
			rounds = append(rounds, ri)
		}
	}
	if rounds == nil {
		rounds = []roundItem{}
	}
	b, _ := json.Marshal(map[string]interface{}{"ok": true, "rounds": rounds})
	w.Write(b)
}

// ── GET /admin/api/competitions ───────────────────────────────────────────────
// Vrátí JSON seznam dostupných soutěží z football-data.org API.

func AdminAPICompetitions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	admin := RequireAdmin(w, r)
	if admin == nil {
		w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
		return
	}
	if config.FootballAPIKey == "" {
		w.Write([]byte(`{"ok":false,"error":"FOOTBALL_API_KEY není nastaven"}`))
		return
	}

	var list fdCompetitionList
	if err := fdCall("competitions", &list); err != nil {
		b, _ := json.Marshal(map[string]interface{}{"ok": false, "error": err.Error()})
		w.Write(b)
		return
	}

	type compItem struct {
		Code string `json:"code"`
		Name string `json:"name"`
		Area string `json:"area"`
	}
	var items []compItem
	for _, c := range list.Competitions {
		if c.Code == "" {
			continue
		}
		items = append(items, compItem{
			Code: c.Code,
			Name: c.Name,
			Area: c.Area.Name,
		})
	}
	if items == nil {
		items = []compItem{}
	}
	b, _ := json.Marshal(map[string]interface{}{"ok": true, "competitions": items})
	w.Write(b)
}

// ── GET /admin/api/preview ────────────────────────────────────────────────────
// Vrátí JSON seznam zápasů z football-data.org pro zobrazení náhledu.
// Query params: fd_code, matchday (nepovinné), skip_finished=1 (nepovinné)

func AdminAPIPreview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	admin := RequireAdmin(w, r)
	if admin == nil {
		w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
		return
	}

	sport := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sport")))
	fdCode := strings.TrimSpace(r.URL.Query().Get("fd_code"))
	matchdayStr := strings.TrimSpace(r.URL.Query().Get("matchday"))
	skipFinished := r.URL.Query().Get("skip_finished") == "1"

	// ── Hockey: api-sports.io ─────────────────────────────────────────────────
	if sport == "hockey" {
		season := ashSeasonFromString(matchdayStr)
		items, skipped, err := ashPreview(fdCode, season, skipFinished)
		if err != nil {
			b, _ := json.Marshal(map[string]interface{}{"ok": false, "error": err.Error()})
			w.Write(b)
			return
		}
		b, _ := json.Marshal(map[string]interface{}{"ok": true, "matches": items, "skipped": skipped})
		w.Write(b)
		return
	}

	// ── Football: football-data.org ───────────────────────────────────────────
	if config.FootballAPIKey == "" {
		w.Write([]byte(`{"ok":false,"error":"FOOTBALL_API_KEY není nastaven v prostředí"}`))
		return
	}

	fdCode = strings.ToUpper(fdCode)

	if fdCode == "" {
		w.Write([]byte(`{"ok":false,"error":"Chybí kód soutěže (fd_code)"}`))
		return
	}

	path := "competitions/" + fdCode + "/matches"
	if matchdayStr != "" {
		path += "?matchday=" + matchdayStr
	}

	var list fdMatchList
	if err := fdCall(path, &list); err != nil {
		b, _ := json.Marshal(map[string]interface{}{"ok": false, "error": err.Error()})
		w.Write(b)
		return
	}

	// Zjednodušená odpověď pro frontend
	type previewMatch struct {
		Home     string `json:"home"`
		Away     string `json:"away"`
		Date     string `json:"date"`
		Status   string `json:"status"`
		Duration string `json:"duration"`
		ScoreH   *int   `json:"score_h"`
		ScoreA   *int   `json:"score_a"`
	}
	var preview []previewMatch
	skipped := 0
	for _, m := range list.Matches {
		h, a := regularTimeScore(m.Score)
		if skipFinished && h != nil {
			skipped++
			continue
		}
		pm := previewMatch{
			Home:     m.HomeTeam.Name,
			Away:     m.AwayTeam.Name,
			Status:   m.Status,
			Duration: m.Score.Duration,
		}
		if m.UtcDate != "" {
			if t, err := time.Parse(time.RFC3339, m.UtcDate); err == nil {
				pm.Date = t.In(pragueLocation).Format("02.01.2006 15:04")
			}
		}
		// Vždy skóre po základní době — prodloužení/penalty se ignorují
		pm.ScoreH = h
		pm.ScoreA = a
		preview = append(preview, pm)
	}
	if preview == nil {
		preview = []previewMatch{}
	}
	b, _ := json.Marshal(map[string]interface{}{"ok": true, "matches": preview, "skipped": skipped})
	w.Write(b)
}

// ── POST /admin/api/import ────────────────────────────────────────────────────
// Importuje zápasy z football-data.org do zvoleného kola.
// Form params:
//   competition_id  — ID naší soutěže
//   round_id        — ID kola (nebo 0 = vytvořit nové)
//   new_round_name  — název nového kola (pokud round_id == 0)
//   fd_code         — kód soutěže v API (např. CL, PL)
//   matchday        — číslo kola v API (nepovinné)
//   sport           — sport pro týmy (default "football")
//   skip_finished   — pokud "1", přeskočí odehrané zápasy

func AdminAPIImport(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	compID, _ := strconv.Atoi(r.FormValue("competition_id"))
	roundID, _ := strconv.Atoi(r.FormValue("round_id"))
	newRoundName := strings.TrimSpace(r.FormValue("new_round_name"))
	fdCode := strings.TrimSpace(r.FormValue("fd_code"))
	matchdayStr := strings.TrimSpace(r.FormValue("matchday"))
	sport := r.FormValue("sport")
	if sport == "" {
		sport = "football"
	}
	skipFinished := r.FormValue("skip_finished") == "1"

	if fdCode == "" {
		middleware.SetFlash(w, r, "error", "Chybí kód soutěže (fd_code).")
		http.Redirect(w, r, "/admin/io", http.StatusSeeOther)
		return
	}
	if compID == 0 {
		middleware.SetFlash(w, r, "error", "Chybí výběr soutěže.")
		http.Redirect(w, r, "/admin/io", http.StatusSeeOther)
		return
	}

	// Ověř soutěž
	ctx := context.Background()
	var compName string
	if err := db.Pool.QueryRow(ctx, `SELECT name FROM competitions WHERE id=$1`, compID).Scan(&compName); err != nil {
		middleware.SetFlash(w, r, "error", "Soutěž nenalezena.")
		http.Redirect(w, r, "/admin/io", http.StatusSeeOther)
		return
	}

	// Vytvoř nebo ověř kolo
	if roundID == 0 {
		if newRoundName == "" {
			if matchdayStr != "" {
				newRoundName = "Kolo " + matchdayStr
			} else {
				newRoundName = "Import z API"
			}
		}
		if err := db.Pool.QueryRow(ctx,
			`INSERT INTO rounds (competition_id, name, is_active) VALUES ($1,$2,true) RETURNING id`,
			compID, newRoundName).Scan(&roundID); err != nil {
			middleware.SetFlash(w, r, "error", "Nepodařilo se vytvořit kolo: "+err.Error())
			http.Redirect(w, r, "/admin/io", http.StatusSeeOther)
			return
		}
	} else {
		// Ověř že kolo patří do soutěže
		var ownerComp int
		if err := db.Pool.QueryRow(ctx, `SELECT competition_id FROM rounds WHERE id=$1`, roundID).Scan(&ownerComp); err != nil || ownerComp != compID {
			middleware.SetFlash(w, r, "error", "Kolo nepatří do vybrané soutěže.")
			http.Redirect(w, r, "/admin/io", http.StatusSeeOther)
			return
		}
	}

	// ── Hockey: api-sports.io ─────────────────────────────────────────────────
	if sport == "hockey" {
		season := ashSeasonFromString(matchdayStr)
		created, teamsNew, skipped, err := ashImport(ctx, compID, roundID, fdCode, season, skipFinished)
		if err != nil {
			middleware.SetFlash(w, r, "error", "Chyba API: "+err.Error())
			http.Redirect(w, r, "/admin/io", http.StatusSeeOther)
			return
		}
		msg := fmt.Sprintf("Import dokončen: <b>%d</b> nových zápasů, <b>%d</b> nových týmů, %d přeskočeno.", created, teamsNew, skipped)
		middleware.SetFlash(w, r, "ok", msg)
		http.Redirect(w, r, fmt.Sprintf("/admin/competitions/%d/rounds", compID), http.StatusSeeOther)
		return
	}

	// ── Football: football-data.org ───────────────────────────────────────────
	if config.FootballAPIKey == "" {
		middleware.SetFlash(w, r, "error", "FOOTBALL_API_KEY není nastaven v prostředí serveru.")
		http.Redirect(w, r, "/admin/io", http.StatusSeeOther)
		return
	}
	fdCode = strings.ToUpper(fdCode)

	// Stáhni zápasy z API
	path := "competitions/" + fdCode + "/matches"
	if matchdayStr != "" {
		path += "?matchday=" + matchdayStr
	}
	var list fdMatchList
	if err := fdCall(path, &list); err != nil {
		middleware.SetFlash(w, r, "error", "Chyba API: "+err.Error())
		http.Redirect(w, r, "/admin/io", http.StatusSeeOther)
		return
	}
	if len(list.Matches) == 0 {
		middleware.SetFlash(w, r, "error", "API nevrátilo žádné zápasy pro tento filtr.")
		http.Redirect(w, r, "/admin/io", http.StatusSeeOther)
		return
	}

	// Importuj zápasy
	created, skipped, teamsNew := 0, 0, 0

	for _, m := range list.Matches {
		// Skóre po základní době (90 min fotbal / 60 min hokej).
		// Prodloužení (extraTime) a penalty NEPOUŽÍVÁME — football-data.org
		// ukládá výsledky po 90 min do score.fullTime, ET a pens zvlášť.
		homeScore, awayScore := regularTimeScore(m.Score)
		isFinished := m.Status == "FINISHED" && homeScore != nil && awayScore != nil

		// Přeskoč odehrané pokud je nastaveno
		if skipFinished && homeScore != nil {
			skipped++
			continue
		}

		// Upsert domácí tým
		homeID, isNew := upsertTeam(ctx, m.HomeTeam, sport)
		if homeID == 0 {
			skipped++
			continue
		}
		if isNew {
			teamsNew++
		}

		// Upsert hostující tým
		awayID, isNew := upsertTeam(ctx, m.AwayTeam, sport)
		if awayID == 0 {
			skipped++
			continue
		}
		if isNew {
			teamsNew++
		}

		// Přiřaď oba týmy k soutěži
		_, _ = db.Pool.Exec(ctx,
			`INSERT INTO competition_teams (competition_id, team_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			compID, homeID)
		_, _ = db.Pool.Exec(ctx,
			`INSERT INTO competition_teams (competition_id, team_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			compID, awayID)

		// Datum zápasu
		var matchDate *time.Time
		if m.UtcDate != "" {
			if t, err := time.Parse(time.RFC3339, m.UtcDate); err == nil {
				tp := t.In(pragueLocation)
				matchDate = &tp
			}
		}

		// Zkontroluj duplicitu (stejný round + same teams)
		var existingID int
		_ = db.Pool.QueryRow(ctx,
			`SELECT id FROM matches WHERE round_id=$1 AND home_team_id=$2 AND away_team_id=$3`,
			roundID, homeID, awayID).Scan(&existingID)

		if existingID > 0 {
			// Aktualizuj datum a skóre pokud existuje
			if isFinished {
				_, _ = db.Pool.Exec(ctx,
					`UPDATE matches SET match_date=$1, home_score=$2, away_score=$3, is_finished=$4 WHERE id=$5`,
					matchDate, homeScore, awayScore, isFinished, existingID)
				RecalculateTips(ctx, existingID, *homeScore, *awayScore)
			} else if matchDate != nil {
				_, _ = db.Pool.Exec(ctx, `UPDATE matches SET match_date=$1 WHERE id=$2`, matchDate, existingID)
			}
			skipped++
			continue
		}

		// Vlož nový zápas
		var newMatchID int
		err := db.Pool.QueryRow(ctx,
			`INSERT INTO matches (round_id, home_team_id, away_team_id, match_date, home_score, away_score, is_finished)
			 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			roundID, homeID, awayID, matchDate, homeScore, awayScore, isFinished).Scan(&newMatchID)
		if err != nil {
			skipped++
			continue
		}
		if isFinished {
			RecalculateTips(ctx, newMatchID, *homeScore, *awayScore)
		}
		created++
	}

	// Přepočítej standings
	RecalculateStandings(compID)

	msg := fmt.Sprintf("Import dokončen: <b>%d</b> nových zápasů, <b>%d</b> nových týmů, %d přeskočeno.",
		created, teamsNew, skipped)
	middleware.SetFlash(w, r, "ok", msg)
	http.Redirect(w, r, fmt.Sprintf("/admin/competitions/%d/rounds", compID), http.StatusSeeOther)
}

// ── POST /admin/api/update-results ───────────────────────────────────────────
// Doplní výsledky z football-data.org do existujících zápasů ve zvoleném kole.
// Nezakládá nové zápasy ani týmy — pouze aktualizuje skóre + přepočítá tipy.
// Form params: competition_id, round_id, fd_code, matchday, sport

func AdminAPIUpdateResults(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	admin := RequireAdmin(w, r)
	if admin == nil {
		w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
		return
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		_ = r.ParseForm()
	}

	jsonErr := func(msg string) {
		b, _ := json.Marshal(map[string]interface{}{"ok": false, "error": msg})
		w.Write(b)
	}

	compID, _ := strconv.Atoi(r.FormValue("competition_id"))
	roundID, _ := strconv.Atoi(r.FormValue("round_id"))
	fdCode := strings.TrimSpace(r.FormValue("fd_code"))
	matchdayStr := strings.TrimSpace(r.FormValue("matchday"))
	sport := r.FormValue("sport")
	if sport == "" {
		sport = "football"
	}

	if fdCode == "" {
		jsonErr("Chybí kód soutěže (fd_code).")
		return
	}
	if compID == 0 || roundID == 0 {
		jsonErr("Vyber soutěž a konkrétní kolo (ne ➕ nové).")
		return
	}

	ctx := context.Background()

	// Ověř že kolo patří do soutěže
	var ownerComp int
	if err := db.Pool.QueryRow(ctx, `SELECT competition_id FROM rounds WHERE id=$1`, roundID).Scan(&ownerComp); err != nil || ownerComp != compID {
		jsonErr("Kolo nepatří do vybrané soutěže.")
		return
	}

	// ── Hockey: api-sports.io ─────────────────────────────────────────────────
	if sport == "hockey" {
		season := ashSeasonFromString(matchdayStr)
		upd, noScr, notFnd, err := ashUpdateResults(ctx, roundID, compID, fdCode, season)
		if err != nil {
			jsonErr("Chyba API: " + err.Error())
			return
		}
		msg := fmt.Sprintf("Hotovo: %d aktualizováno, %d bez skóre, %d nenalezeno v kole.", upd, noScr, notFnd)
		b, _ := json.Marshal(map[string]interface{}{"ok": true, "message": msg})
		w.Write(b)
		return
	}

	// ── Football: football-data.org ───────────────────────────────────────────
	if config.FootballAPIKey == "" {
		jsonErr("FOOTBALL_API_KEY není nastaven v prostředí serveru.")
		return
	}
	fdCode = strings.ToUpper(fdCode)

	// Stáhni zápasy z API
	path := "competitions/" + fdCode + "/matches"
	if matchdayStr != "" {
		path += "?matchday=" + matchdayStr
	}
	var list fdMatchList
	if err := fdCall(path, &list); err != nil {
		jsonErr("Chyba API: " + err.Error())
		return
	}

	updated, noScore, notFound := 0, 0, 0

	for _, m := range list.Matches {
		// Bereme pouze FINISHED zápasy — skóre po základní době (fullTime),
		// bez prodloužení (extraTime) a penalt (penalties).
		if m.Status != "FINISHED" {
			noScore++
			continue
		}
		homeScore, awayScore := regularTimeScore(m.Score)
		if homeScore == nil || awayScore == nil {
			noScore++
			continue
		}

		var homeID, awayID int
		_ = db.Pool.QueryRow(ctx, `SELECT id FROM teams WHERE name=$1 AND sport=$2`, m.HomeTeam.Name, sport).Scan(&homeID)
		_ = db.Pool.QueryRow(ctx, `SELECT id FROM teams WHERE name=$1 AND sport=$2`, m.AwayTeam.Name, sport).Scan(&awayID)

		if homeID == 0 || awayID == 0 {
			notFound++
			continue
		}

		var matchID int
		err := db.Pool.QueryRow(ctx,
			`SELECT id FROM matches WHERE round_id=$1 AND home_team_id=$2 AND away_team_id=$3`,
			roundID, homeID, awayID).Scan(&matchID)
		if err != nil {
			notFound++
			continue
		}

		_, err = db.Pool.Exec(ctx,
			`UPDATE matches SET home_score=$1, away_score=$2, is_finished=true WHERE id=$3`,
			homeScore, awayScore, matchID)
		if err != nil {
			notFound++
			continue
		}

		RecalculateTips(ctx, matchID, *homeScore, *awayScore)
		updated++
	}

	RecalculateStandings(compID)

	// Audit log
	adminID := admin.ID
	logDesc := fmt.Sprintf("Doplnit výsledky API: %s matchday=%s → %d aktualizováno, %d bez skóre, %d nenalezeno (kolo id=%d)",
		fdCode, matchdayStr, updated, noScore, notFound, roundID)
	LogAction(&adminID, admin.Username, "api_update_results", "round", &roundID, logDesc, nil, nil)

	msg := fmt.Sprintf("Výsledky doplněny: %d zápasů aktualizováno", updated)
	if noScore > 0 {
		msg += fmt.Sprintf(", %d bez skóre přeskočeno", noScore)
	}
	if notFound > 0 {
		msg += fmt.Sprintf(", %d nenalezeno v DB", notFound)
	}
	msg += "."
	b, _ := json.Marshal(map[string]interface{}{"ok": true, "message": msg})
	w.Write(b)
}

// ── AutoFetchResults volá scheduler pro automatické doplnění výsledků ────────
// Vrátí počet aktualizovaných a nenalezených zápasů. Používá db.Pool přímo.
func AutoFetchResults(ctx context.Context, compID int, fdCode, sport string) (updated, notFound int) {
	if config.FootballAPIKey == "" || fdCode == "" {
		return 0, 0
	}

	var list fdMatchList
	if err := fdCall("competitions/"+fdCode+"/matches", &list); err != nil {
		return 0, 0
	}

	for _, m := range list.Matches {
		// Pouze FINISHED zápasy — skóre po základní době (fullTime = 90/60 min),
		// prodloužení a penalty NEBEREME v potaz.
		if m.Status != "FINISHED" {
			continue
		}
		homeScore, awayScore := regularTimeScore(m.Score)
		if homeScore == nil || awayScore == nil {
			continue
		}
		var homeID, awayID int
		_ = db.Pool.QueryRow(ctx, `SELECT id FROM teams WHERE name=$1 AND sport=$2`, m.HomeTeam.Name, sport).Scan(&homeID)
		_ = db.Pool.QueryRow(ctx, `SELECT id FROM teams WHERE name=$1 AND sport=$2`, m.AwayTeam.Name, sport).Scan(&awayID)
		if homeID == 0 || awayID == 0 {
			notFound++
			continue
		}
		// Najdi zápas v libovolném kole dané soutěže
		var matchID int
		err := db.Pool.QueryRow(ctx,
			`SELECT m.id FROM matches m
			   JOIN rounds r ON r.id = m.round_id
			  WHERE r.competition_id=$1 AND m.home_team_id=$2 AND m.away_team_id=$3
			  LIMIT 1`,
			compID, homeID, awayID).Scan(&matchID)
		if err != nil {
			notFound++
			continue
		}
		_, err = db.Pool.Exec(ctx,
			`UPDATE matches SET home_score=$1, away_score=$2, is_finished=true WHERE id=$3`,
			homeScore, awayScore, matchID)
		if err != nil {
			continue
		}
		RecalculateTips(ctx, matchID, *homeScore, *awayScore)
		updated++
	}

	if updated > 0 {
		RecalculateStandings(compID)
		LogAction(nil, "scheduler", "auto_fetch_results", "competition", &compID,
			fmt.Sprintf("Auto-fetch %s: %d aktualizováno, %d nenalezeno", fdCode, updated, notFound), nil, nil)
	}
	return updated, notFound
}

// upsertTeam vrátí ID týmu (existující nebo nově vytvořeného).
// Druhý return value = true pokud byl tým nově vytvořen.
func upsertTeam(ctx context.Context, ft fdTeam, sport string) (int, bool) {
	if ft.Name == "" {
		return 0, false
	}
	// Alias = TLA (3-písmená zkratka)
	alias := ft.TLA
	// ShortName jako display_name
	displayName := ft.ShortName
	if displayName == ft.Name {
		displayName = ""
	}

	// 1. Přesná shoda jména + sport
	var id int
	err := db.Pool.QueryRow(ctx,
		`SELECT id FROM teams WHERE name=$1 AND sport=$2`, ft.Name, sport).Scan(&id)
	if err == nil {
		_, _ = db.Pool.Exec(ctx,
			`UPDATE teams SET
			   alias       = COALESCE(NULLIF(alias,''), NULLIF($1,'')),
			   display_name= COALESCE(NULLIF(display_name,''), NULLIF($2,''))
			 WHERE id=$3`,
			alias, displayName, id)
		return id, false
	}

	// 2. Shoda přes alias (case-insensitive) — admin nastaví alias "Czech Republic" na tým "Česko"
	err = db.Pool.QueryRow(ctx,
		`SELECT id FROM teams WHERE LOWER(alias)=LOWER($1) AND sport=$2`, ft.Name, sport).Scan(&id)
	if err == nil {
		return id, false
	}

	// 3. Shoda přes display_name (case-insensitive)
	err = db.Pool.QueryRow(ctx,
		`SELECT id FROM teams WHERE LOWER(display_name)=LOWER($1) AND sport=$2`, ft.Name, sport).Scan(&id)
	if err == nil {
		return id, false
	}

	// Nenalezen — vytvoř nový tým
	var newID int
	insertErr := db.Pool.QueryRow(ctx,
		`INSERT INTO teams (name, sport, display_name, alias)
		 VALUES ($1,$2,$3,$4) RETURNING id`,
		ft.Name, sport, PtrStr(displayName), PtrStr(alias)).Scan(&newID)
	if insertErr != nil {
		return 0, false
	}
	return newID, true
}
