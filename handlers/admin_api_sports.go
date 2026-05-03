// handlers/admin_api_sports.go — Tipovačka 2.0
// Hokejový import z api-sports.io (https://v1.hockey.api-sports.io).
// Používá se pro IIHF Mistrovství světa a Zimní olympijské hry.
//
// Config: API_SPORTS_HOCKEY_KEY (= config.HockeySportsKey)
// Endpointy (HTTP):
//   GET /admin/api/apisports-leagues — seznam hardcoded lig
//
// Interní helpers volané z admin_api_import.go:
//   asioPreview(leagueID, season, skipFinished) ([]previewMatchItem, int, error)
//   asioUpdateResults(ctx, roundID, compID, leagueID, season) (updated, noScore, notFound int, err error)
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"tipovacka/config"
	"tipovacka/db"
)

// ── api-sports.io structs ──────────────────────────────────────────────────────

type asioResponse struct {
	Response []asioGame `json:"response"`
}

type asioGame struct {
	ID     int        `json:"id"`
	Date   string     `json:"date"`   // "2024-05-10T14:00:00+00:00"
	Status asioStatus `json:"status"`
	Teams  asioTeams  `json:"teams"`
	Scores asioScores `json:"scores"`
}

type asioStatus struct {
	Short string `json:"short"` // "NS", "FT", "AOT", "PEN", ...
	Long  string `json:"long"`  // "Not Started", "Game Finished", ...
}

type asioTeams struct {
	Home asioTeam `json:"home"`
	Away asioTeam `json:"away"`
}

type asioTeam struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type asioScores struct {
	Home asioScoreDetail `json:"home"`
	Away asioScoreDetail `json:"away"`
}

type asioScoreDetail struct {
	Current *int `json:"current"` // celkový počet gólů (po regulaci + případném OT)
}

// ── Hardcoded ligy ──────────────────────────────────────────────────────────

// asioHardcodedLeagues obsahuje IIHF MS a Olympiádu na api-sports.io.
// Platná ID ověř na: https://v1.hockey.api-sports.io/leagues
var asioHardcodedLeagues = []struct {
	ID   string
	Name string
}{
	{"111", "Mistrovství světa v hokeji (IIHF)"},
	{"131", "Zimní olympijské hry — lední hokej"},
}

// ── asioCall — HTTP volání api-sports.io ──────────────────────────────────────

func asioCall(path string, dst interface{}) error {
	if config.HockeySportsKey == "" {
		return fmt.Errorf("API_SPORTS_HOCKEY_KEY není nastaven")
	}
	url := "https://v1.hockey.api-sports.io/" + strings.TrimPrefix(path, "/")
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-apisports-key", config.HockeySportsKey)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		msg := string(body)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("API %d: %s", resp.StatusCode, msg)
	}
	return json.Unmarshal(body, dst)
}

// asioIsFinished vrátí true pokud je zápas odehraný.
func asioIsFinished(status asioStatus) bool {
	s := strings.ToLower(status.Short)
	return s == "ft" || s == "aot" || s == "pen" || s == "ap" || s == "awarded"
}

// ── GET /admin/api/apisports-leagues ─────────────────────────────────────────

func AdminAPISportsLeagues(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	admin := RequireAdmin(w, r)
	if admin == nil {
		w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
		return
	}

	// ?live=1 → zavolej api-sports.io a vrať raw odpověď (debug)
	// ?live=1&path=games?league=111&season=2026  → libovolný endpoint
	// ?live=1&search=world                        → /leagues?search=world
	if r.URL.Query().Get("live") == "1" {
		path := r.URL.Query().Get("path")
		if path == "" {
			search := r.URL.Query().Get("search")
			path = "leagues"
			if search != "" {
				path += "?search=" + search
			}
		}
		var raw interface{}
		if err := asioCall(path, &raw); err != nil {
			b, _ := json.Marshal(map[string]interface{}{"ok": false, "error": err.Error()})
			w.Write(b)
			return
		}
		b, _ := json.Marshal(raw)
		w.Write(b)
		return
	}

	type leagueItem struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	items := make([]leagueItem, 0, len(asioHardcodedLeagues))
	for _, l := range asioHardcodedLeagues {
		items = append(items, leagueItem{ID: l.ID, Name: l.Name})
	}
	b, _ := json.Marshal(map[string]interface{}{"ok": true, "leagues": items})
	w.Write(b)
}

// ── asioPreview — vrátí náhled zápasů pro frontend ───────────────────────────

func asioPreview(leagueID, season string, skipFinished bool) ([]previewMatchItem, int, error) {
	path := fmt.Sprintf("games?league=%s&season=%s", leagueID, season)
	var resp asioResponse
	if err := asioCall(path, &resp); err != nil {
		return nil, 0, err
	}

	var items []previewMatchItem
	skipped := 0
	for _, g := range resp.Response {
		finished := asioIsFinished(g.Status)
		if skipFinished && finished {
			skipped++
			continue
		}
		item := previewMatchItem{
			Home:   strings.TrimSpace(g.Teams.Home.Name),
			Away:   strings.TrimSpace(g.Teams.Away.Name),
			Status: g.Status.Long,
		}
		if g.Date != "" {
			if t, err := time.Parse(time.RFC3339, g.Date); err == nil {
				tp := t.In(pragueLocation)
				item.Date    = tp.Format("02.01.2006 15:04")
				item.RawDate = tp.Format("2006-01-02T15:04:05")
			}
		}
		if finished {
			item.ScoreH = g.Scores.Home.Current
			item.ScoreA = g.Scores.Away.Current
		}
		items = append(items, item)
	}
	if items == nil {
		items = []previewMatchItem{}
	}
	return items, skipped, nil
}

// ── asioUpdateResults — doplní výsledky z api-sports.io ──────────────────────

func asioUpdateResults(ctx context.Context, roundID, compID int, leagueID, season string) (updated, noScore, notFound int, err error) {
	path := fmt.Sprintf("games?league=%s&season=%s", leagueID, season)
	var resp asioResponse
	if ferr := asioCall(path, &resp); ferr != nil {
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

	for _, g := range resp.Response {
		if !asioIsFinished(g.Status) {
			noScore++
			continue
		}
		if g.Scores.Home.Current == nil || g.Scores.Away.Current == nil {
			noScore++
			continue
		}

		// Normalizuj jméno (api-sports.io nepoužívá suffix " Ice Hockey")
		homeLow := strings.ToLower(strings.TrimSpace(g.Teams.Home.Name))
		awayLow := strings.ToLower(strings.TrimSpace(g.Teams.Away.Name))

		matchID := 0
		for _, mr := range dbMatches {
			if normTeamNameASH(mr.Home) == homeLow && normTeamNameASH(mr.Away) == awayLow {
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
			*g.Scores.Home.Current, *g.Scores.Away.Current, matchID)
		RecalculateTips(ctx, matchID, *g.Scores.Home.Current, *g.Scores.Away.Current)
		updated++
	}

	RecalculateStandings(compID)
	return updated, noScore, notFound, nil
}
