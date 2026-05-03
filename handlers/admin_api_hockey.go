// handlers/admin_api_hockey.go — Tipovačka 2.0
// Import zápasů a výsledků z TheSportsDB (free, key="3").
//
// Endpoint:
//   GET /admin/api/hockey-leagues  — seznam hokejových lig filtrovanych klíčovými slovy
//   GET /admin/api/hockey-seasons  — sezóny pro danou ligu (?league_id=4380)
//
// Interní helpers volané z admin_api_import.go:
//   ashPreview(leagueID, season, skipFinished) ([]previewMatchItem, int, error)
//   ashImport(ctx, compID, roundID, leagueID, season, skipFinished) (created, teamsNew, skipped int, err error)
//   ashUpdateResults(ctx, roundID, compID, leagueID, season) (updated, noScore, notFound int, err error)
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

	"tipovacka/db"
)

// ── TheSportsDB structs ───────────────────────────────────────────────────────

type tsdbLeagueList struct {
	Leagues []tsdbLeague `json:"leagues"`
}

type tsdbLeague struct {
	IDLeague  string `json:"idLeague"`
	StrLeague string `json:"strLeague"`
	StrSport  string `json:"strSport"`
	StrCountry string `json:"strCountry"`
}

type tsdbSeasonList struct {
	Seasons []tsdbSeason `json:"seasons"`
}

type tsdbSeason struct {
	StrSeason string `json:"strSeason"`
}

type tsdbEventList struct {
	Events []tsdbEvent `json:"events"`
}

type tsdbEvent struct {
	IDEvent      string  `json:"idEvent"`
	StrHomeTeam  string  `json:"strHomeTeam"`
	StrAwayTeam  string  `json:"strAwayTeam"`
	IDHomeTeam   string  `json:"idHomeTeam"`
	IDAwayTeam   string  `json:"idAwayTeam"`
	DateEvent    string  `json:"dateEvent"`   // "2026-05-15"
	StrTime      string  `json:"strTime"`     // "16:20:00"
	IntHomeScore *string `json:"intHomeScore"` // null or "2" (string!)
	IntAwayScore *string `json:"intAwayScore"` // null or "2" (string!)
	StrStatus    string  `json:"strStatus"`   // "Not Started", "Match Finished", ...
}

// tsdbBase je základní URL TheSportsDB free API.
const tsdbBase = "https://www.thesportsdb.com/api/v1/json/3/"

// tsdbCall volá TheSportsDB API a dekóduje JSON do dst.
func tsdbCall(path string, dst interface{}) error {
	url := tsdbBase + strings.TrimPrefix(path, "/")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		msg := string(body)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("API %d: %s", resp.StatusCode, msg)
	}
	return json.Unmarshal(body, dst)
}

// tsdbScoreInt převede *string skóre (null nebo "2") na *int.
func tsdbScoreInt(s *string) *int {
	if s == nil {
		return nil
	}
	n, err := strconv.Atoi(*s)
	if err != nil {
		return nil
	}
	return &n
}

// tsdbIsFinished vrátí true pokud je zápas odehraný.
func tsdbIsFinished(status string) bool {
	s := strings.ToLower(status)
	return strings.Contains(s, "finish") || strings.Contains(s, "ft") || s == "match finished"
}

// ── GET /admin/api/hockey-leagues ────────────────────────────────────────────

// Pevně definované ligy z TheSportsDB (free API nevrací kompletní seznam).
// ID 4976 = Mens Ice Hockey World Championships (IIHF MS)
// ID 5137 = Olympics Ice Hockey
var hardcodedHockeyLeagues = []struct {
	ID   string
	Name string
}{
	{"4976", "Mistrovství světa v hokeji (IIHF)"},
	{"5137", "Olympijské hry — lední hokej"},
}

func AdminAPIHockeyLeagues(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	admin := RequireAdmin(w, r)
	if admin == nil {
		w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
		return
	}

	type leagueItem struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	items := make([]leagueItem, 0, len(hardcodedHockeyLeagues))
	for _, l := range hardcodedHockeyLeagues {
		items = append(items, leagueItem{ID: l.ID, Name: l.Name})
	}
	b, _ := json.Marshal(map[string]interface{}{"ok": true, "leagues": items})
	w.Write(b)
}

// ── GET /admin/api/hockey-seasons ────────────────────────────────────────────

func AdminAPIHockeySeasons(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	admin := RequireAdmin(w, r)
	if admin == nil {
		w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
		return
	}

	leagueID := strings.TrimSpace(r.URL.Query().Get("league_id"))
	if leagueID == "" {
		w.Write([]byte(`{"ok":false,"error":"Chybí league_id"}`))
		return
	}

	var list tsdbSeasonList
	// Chyba API je OK — prostě použijeme jen statické roky
	_ = tsdbCall("search_all_seasons.php?id="+leagueID, &list)

	type seasonItem struct {
		Season string `json:"season"`
	}
	seen := map[string]bool{}
	var items []seasonItem
	for _, s := range list.Seasons {
		if s.StrSeason != "" && !seen[s.StrSeason] {
			seen[s.StrSeason] = true
			items = append(items, seasonItem{Season: s.StrSeason})
		}
	}

	// Přidej aktuální rok a rok příští, pokud v API chybí (TheSportsDB free tier
	// nezahrnuje nejnovější sezóny dokud nejsou zpětně doplněny).
	now := time.Now().Year()
	for _, yr := range []int{now, now - 1} {
		s := strconv.Itoa(yr)
		if !seen[s] {
			seen[s] = true
			items = append([]seasonItem{{Season: s}}, items...)
		}
	}

	// Zobraz jen aktuální rok a dopředu — minulé sezóny jsou pro import irelevantní.
	minYear := now
	var recent []seasonItem
	for _, it := range items {
		yr, err := strconv.Atoi(strings.SplitN(it.Season, "/", 2)[0])
		if err == nil && yr >= minYear && yr <= now {
			recent = append(recent, it)
		}
	}
	if len(recent) > 0 {
		items = recent
	}

	if items == nil {
		items = []seasonItem{}
	}
	b, _ := json.Marshal(map[string]interface{}{"ok": true, "seasons": items})
	w.Write(b)
}

// ── previewMatchItem — sdílená struktura pro preview odpověď ─────────────────

type previewMatchItem struct {
	Home    string `json:"home"`
	Away    string `json:"away"`
	Date    string `json:"date"`     // formátovaný pro zobrazení "02.01.2006 15:04"
	RawDate string `json:"raw_date"` // ISO "2006-01-02T15:04:05" pro import
	Status  string `json:"status"`
	ScoreH  *int   `json:"score_h"`
	ScoreA  *int   `json:"score_a"`
}

// ── ashLoadEvents — načte zápasy z TheSportsDB ────────────────────────────────

func ashLoadEvents(leagueID, season string) ([]tsdbEvent, error) {
	path := fmt.Sprintf("eventsseason.php?id=%s&s=%s", leagueID, season)
	var list tsdbEventList
	if err := tsdbCall(path, &list); err != nil {
		return nil, err
	}
	return list.Events, nil
}

// tsdbEventDate převede datum + čas z TheSportsDB na time.Time v Prague tz.
func tsdbEventDate(dateEvent, strTime string) *time.Time {
	if dateEvent == "" {
		return nil
	}
	ts := dateEvent
	if strTime != "" {
		ts += "T" + strTime
	} else {
		ts += "T00:00:00"
	}
	t, err := time.ParseInLocation("2006-01-02T15:04:05", ts, pragueLocation)
	if err != nil {
		return nil
	}
	return &t
}

// ── ashPreview — vrátí náhled zápasů pro frontend ────────────────────────────

func ashPreview(leagueID, season string, skipFinished bool) ([]previewMatchItem, int, error) {
	events, err := ashLoadEvents(leagueID, season)
	if err != nil {
		return nil, 0, err
	}

	var items []previewMatchItem
	skipped := 0
	for _, e := range events {
		finished := tsdbIsFinished(e.StrStatus)
		if skipFinished && finished {
			skipped++
			continue
		}
		item := previewMatchItem{
			Home:   cleanHockeyTeamName(e.StrHomeTeam),
			Away:   cleanHockeyTeamName(e.StrAwayTeam),
			Status: e.StrStatus,
		}
		if t := tsdbEventDate(e.DateEvent, e.StrTime); t != nil {
			item.Date    = t.Format("02.01.2006 15:04")
			item.RawDate = t.Format("2006-01-02T15:04:05")
		}
		if finished {
			item.ScoreH = tsdbScoreInt(e.IntHomeScore)
			item.ScoreA = tsdbScoreInt(e.IntAwayScore)
		}
		items = append(items, item)
	}
	if items == nil {
		items = []previewMatchItem{}
	}
	return items, skipped, nil
}

// ── ashImport — importuje zápasy z TheSportsDB do DB ─────────────────────────

func ashImport(ctx context.Context, compID, roundID int, leagueID, season string, skipFinished bool) (created, teamsNew, skipped int, err error) {
	events, ferr := ashLoadEvents(leagueID, season)
	if ferr != nil {
		return 0, 0, 0, ferr
	}
	if len(events) == 0 {
		return 0, 0, 0, fmt.Errorf("API nevrátilo žádné zápasy pro ligu %s sezónu %s", leagueID, season)
	}

	for _, e := range events {
		finished := tsdbIsFinished(e.StrStatus)
		if skipFinished && finished {
			skipped++
			continue
		}

		// Upsert teams — TheSportsDB má string ID, převedeme na int pro fdTeam
		homeExtID, _ := strconv.Atoi(e.IDHomeTeam)
		awayExtID, _ := strconv.Atoi(e.IDAwayTeam)

		homeTeam := fdTeam{ID: homeExtID, Name: cleanHockeyTeamName(e.StrHomeTeam), ShortName: cleanHockeyTeamName(e.StrHomeTeam)}
		awayTeam := fdTeam{ID: awayExtID, Name: cleanHockeyTeamName(e.StrAwayTeam), ShortName: cleanHockeyTeamName(e.StrAwayTeam)}

		homeID, isNew := upsertTeam(ctx, homeTeam, "hockey")
		if homeID == 0 {
			skipped++
			continue
		}
		if isNew {
			teamsNew++
		}
		awayID, isNew := upsertTeam(ctx, awayTeam, "hockey")
		if awayID == 0 {
			skipped++
			continue
		}
		if isNew {
			teamsNew++
		}

		// Přiřaď k soutěži
		_, _ = db.Pool.Exec(ctx,
			`INSERT INTO competition_teams (competition_id, team_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			compID, homeID)
		_, _ = db.Pool.Exec(ctx,
			`INSERT INTO competition_teams (competition_id, team_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			compID, awayID)

		// Datum
		matchDate := tsdbEventDate(e.DateEvent, e.StrTime)

		// Duplicita
		var existingID int
		_ = db.Pool.QueryRow(ctx,
			`SELECT id FROM matches WHERE round_id=$1 AND home_team_id=$2 AND away_team_id=$3`,
			roundID, homeID, awayID).Scan(&existingID)

		if existingID > 0 {
			if matchDate != nil {
				_, _ = db.Pool.Exec(ctx, `UPDATE matches SET match_date=$1 WHERE id=$2`, matchDate, existingID)
			}
			skipped++
			continue
		}

		var newMatchID int
		ferr := db.Pool.QueryRow(ctx,
			`INSERT INTO matches (round_id, home_team_id, away_team_id, match_date, is_finished)
			 VALUES ($1,$2,$3,$4,false) RETURNING id`,
			roundID, homeID, awayID, matchDate).Scan(&newMatchID)
		if ferr != nil {
			skipped++
			continue
		}
		created++
	}
	return created, teamsNew, skipped, nil
}

// ── ashUpdateResults — doplní výsledky z TheSportsDB ─────────────────────────

func ashUpdateResults(ctx context.Context, roundID, compID int, leagueID, season string) (updated, noScore, notFound int, err error) {
	events, ferr := ashLoadEvents(leagueID, season)
	if ferr != nil {
		return 0, 0, 0, ferr
	}

	// Načti existující zápasy v kole
	rows, rerr := db.Pool.Query(ctx,
		`SELECT m.id, ht.name, at.name
		 FROM matches m
		 JOIN teams ht ON ht.id = m.home_team_id
		 JOIN teams at ON at.id = m.away_team_id
		 WHERE m.round_id = $1`, roundID)
	if rerr != nil {
		return 0, 0, 0, rerr
	}
	type matchRow struct {
		ID   int
		Home string
		Away string
	}
	var dbMatches []matchRow
	for rows.Next() {
		var mr matchRow
		if rerr := rows.Scan(&mr.ID, &mr.Home, &mr.Away); rerr == nil {
			dbMatches = append(dbMatches, mr)
		}
	}
	rows.Close()

	for _, e := range events {
		if !tsdbIsFinished(e.StrStatus) {
			noScore++
			continue
		}
		homeGoals := tsdbScoreInt(e.IntHomeScore)
		awayGoals := tsdbScoreInt(e.IntAwayScore)
		if homeGoals == nil || awayGoals == nil {
			noScore++
			continue
		}

		// Najdi odpovídající zápas v DB
		matchID := 0
		for _, mr := range dbMatches {
			if normTeamNameASH(mr.Home) == normTeamNameASH(e.StrHomeTeam) &&
				normTeamNameASH(mr.Away) == normTeamNameASH(e.StrAwayTeam) {
				matchID = mr.ID
				break
			}
		}
		if matchID == 0 {
			notFound++
			continue
		}

		_, _ = db.Pool.Exec(ctx,
			`UPDATE matches SET home_score=$1, away_score=$2, is_finished=true WHERE id=$3`,
			*homeGoals, *awayGoals, matchID)
		RecalculateTips(ctx, matchID, *homeGoals, *awayGoals)
		updated++
	}

	RecalculateStandings(compID)
	return updated, noScore, notFound, nil
}

// cleanHockeyTeamName odstraní suffix " Ice Hockey" který TheSportsDB přidává.
// "Czech Republic Ice Hockey" → "Czech Republic"
func cleanHockeyTeamName(s string) string {
	for _, suf := range []string{" Ice Hockey", " ice hockey", " Ice hockey"} {
		if strings.HasSuffix(s, suf) {
			return strings.TrimSpace(s[:len(s)-len(suf)])
		}
	}
	return strings.TrimSpace(s)
}

// normTeamNameASH normalizuje název týmu pro porovnání.
func normTeamNameASH(s string) string {
	return strings.ToLower(strings.TrimSpace(cleanHockeyTeamName(s)))
}

// ashSeasonFromString vrátí sezónu z řetězce nebo aktuální rok.
// Pokud obsahuje "/", vezme první část: "2024/25" → "2024".
func ashSeasonFromString(season string) string {
	season = strings.TrimSpace(season)
	if season == "" {
		return strconv.Itoa(time.Now().Year())
	}
	if idx := strings.Index(season, "/"); idx > 0 {
		return season[:idx]
	}
	return season
}
