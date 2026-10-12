// handlers/admin_odds.go — kurzy od The Odds API (jen pro Owner)
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"tipovacka/db"
	"tipovacka/middleware"
)

// POST /admin/odds/refresh — načte kurzy z The Odds API a uloží do match_odds
func AdminOddsRefresh(w http.ResponseWriter, r *http.Request) {
	user := GetCurrentUser(r)
	if user == nil || !user.IsOwner {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	apiKey := os.Getenv("ODDS_API_KEY")
	if apiKey == "" {
		middleware.SetFlash(w, r, "error", "ODDS_API_KEY není nastaven.")
		http.Redirect(w, r, r.Referer(), http.StatusSeeOther)
		return
	}

	fetched, err := fetchAndStoreOdds(apiKey)
	if err != nil {
		log.Printf("[odds] refresh error: %v", err)
		middleware.SetFlash(w, r, "error", "Chyba při načítání kurzů: "+err.Error())
	} else {
		middleware.SetFlash(w, r, "ok", fmt.Sprintf("Kurzy aktualizovány: %d zápasů.", fetched))
	}
	http.Redirect(w, r, r.Referer(), http.StatusSeeOther)
}

type oddsAPIEvent struct {
	ID           string `json:"id"`
	SportKey     string `json:"sport_key"`
	CommenceTime string `json:"commence_time"`
	HomeTeam     string `json:"home_team"`
	AwayTeam     string `json:"away_team"`
	Bookmakers   []struct {
		Key     string `json:"key"`
		Title   string `json:"title"`
		Markets []struct {
			Key      string `json:"key"`
			Outcomes []struct {
				Name  string  `json:"name"`
				Price float64 `json:"price"`
			} `json:"outcomes"`
		} `json:"markets"`
	} `json:"bookmakers"`
}

// sportKeys mapuje fd_code soutěže → sport key pro Odds API
var sportKeys = map[string]string{
	"CL":  "soccer_uefa_champs_league",
	"EL":  "soccer_uefa_europa_league",
	"ECL": "soccer_uefa_europa_conference_league",
	"PL":  "soccer_epl",
	"SA":  "soccer_italy_serie_a",
	"BL1": "soccer_germany_bundesliga",
	"PD":  "soccer_spain_la_liga",
	"FL1": "soccer_france_ligue_one",
}

func fetchAndStoreOdds(apiKey string) (int, error) {
	ctx := context.Background()

	// Zjisti fd_code aktivních soutěží
	rows, err := db.Pool.Query(ctx, `SELECT DISTINCT c.fd_code FROM competitions c WHERE c.is_active = true AND c.fd_code != ''`)
	if err != nil {
		return 0, err
	}
	var fdCodes []string
	for rows.Next() {
		var code string
		_ = rows.Scan(&code)
		fdCodes = append(fdCodes, code)
	}
	rows.Close()

	total := 0
	for _, fdCode := range fdCodes {
		sportKey, ok := sportKeys[fdCode]
		if !ok {
			continue
		}
		n, err := fetchSportOdds(ctx, apiKey, sportKey, fdCode)
		if err != nil {
			log.Printf("[odds] %s: %v", sportKey, err)
			continue
		}
		total += n
	}
	return total, nil
}

func fetchSportOdds(ctx context.Context, apiKey, sportKey, fdCode string) (int, error) {
	url := fmt.Sprintf(
		"https://api.the-odds-api.com/v4/sports/%s/odds/?apiKey=%s&regions=eu&markets=h2h&oddsFormat=decimal&bookmakers=bet365,unibet,pinnacle,betway",
		sportKey, apiKey,
	)
	resp, err := http.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("API %d: %s", resp.StatusCode, string(body))
	}

	var events []oddsAPIEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return 0, err
	}

	saved := 0
	for _, ev := range events {
		commenceTime, err := time.Parse(time.RFC3339, ev.CommenceTime)
		if err != nil {
			continue
		}

		// Najdi zápas v DB podle času ±3h
		matchID := findMatchByTeamsAndTime(ctx, fdCode, ev.HomeTeam, ev.AwayTeam, commenceTime)
		log.Printf("[odds] event %s vs %s @ %s → matchID=%d", ev.HomeTeam, ev.AwayTeam, commenceTime.Format("02.01 15:04"), matchID)
		if matchID == 0 {
			continue
		}

		// Vyber nejlepší bookmaker (první dostupný)
		for _, bm := range ev.Bookmakers {
			for _, mkt := range bm.Markets {
				if mkt.Key != "h2h" {
					continue
				}
				var home, draw, away float64
				for _, o := range mkt.Outcomes {
					switch o.Name {
					case ev.HomeTeam:
						home = o.Price
					case ev.AwayTeam:
						away = o.Price
					case "Draw":
						draw = o.Price
					}
				}
				if home == 0 || away == 0 {
					continue
				}
				_, _ = db.Pool.Exec(ctx, `
					INSERT INTO match_odds (match_id, bookmaker, home_odds, draw_odds, away_odds, fetched_at)
					VALUES ($1,$2,$3,$4,$5,NOW())
					ON CONFLICT (match_id) DO UPDATE
					  SET bookmaker=EXCLUDED.bookmaker, home_odds=EXCLUDED.home_odds,
					      draw_odds=EXCLUDED.draw_odds, away_odds=EXCLUDED.away_odds,
					      fetched_at=EXCLUDED.fetched_at`,
					matchID, bm.Title, home, draw, away)
				saved++
				goto nextEvent
			}
		}
	nextEvent:
	}
	return saved, nil
}

func findMatchByTeamsAndTime(ctx context.Context, fdCode, homeTeam, awayTeam string, t time.Time) int {
	var matchID int
	err := db.Pool.QueryRow(ctx, `
		SELECT m.id FROM matches m
		JOIN competitions c ON c.id = m.competition_id
		WHERE c.fd_code = $1
		  AND m.match_date BETWEEN $2 AND $3
		ORDER BY ABS(EXTRACT(EPOCH FROM (m.match_date - $4)))
		LIMIT 1`,
		fdCode,
		t.Add(-3*time.Hour),
		t.Add(3*time.Hour),
		t,
	).Scan(&matchID)
	if err != nil {
		log.Printf("[odds] findMatch %s vs %s @ %s: %v", homeTeam, awayTeam, t.Format("02.01 15:04"), err)
	}
	return matchID
}

// LoadMatchOdds načte kurzy pro dané match_id — vrátí mapu matchID → [home, draw, away, bookmaker]
func LoadMatchOdds(ctx context.Context, matchIDs []int) map[int][4]string {
	result := map[int][4]string{}
	if len(matchIDs) == 0 {
		return result
	}
	placeholders := make([]string, len(matchIDs))
	args := make([]interface{}, len(matchIDs))
	for i, id := range matchIDs {
		placeholders[i] = "$" + strconv.Itoa(i+1)
		args[i] = id
	}
	rows, err := db.Pool.Query(ctx,
		`SELECT match_id, home_odds, draw_odds, away_odds, bookmaker FROM match_odds WHERE match_id IN (`+strings.Join(placeholders, ",")+`)`,
		args...)
	if err != nil {
		return result
	}
	defer rows.Close()
	for rows.Next() {
		var mid int
		var h, d, a float64
		var bm string
		_ = rows.Scan(&mid, &h, &d, &a, &bm)
		result[mid] = [4]string{
			fmt.Sprintf("%.2f", h),
			fmt.Sprintf("%.2f", d),
			fmt.Sprintf("%.2f", a),
			bm,
		}
	}
	return result
}
