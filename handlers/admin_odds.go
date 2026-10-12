// handlers/admin_odds.go — kurzy od The Odds API (jen pro Owner)
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"sort"
	"strings"
	"time"

	"tipovacka/db"
	"tipovacka/middleware"
)

type oddsDbTeam struct {
	ID    int
	Name  string
	Alias string
}

type oddsApiTeam struct {
	ApiName string
	DbID    int
	DbName  string
}

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

	fetched, unmatched, err := fetchAndStoreOdds(apiKey)
	if err != nil {
		log.Printf("[odds] refresh error: %v", err)
		middleware.SetFlash(w, r, "error", "Chyba při načítání kurzů: "+err.Error())
		http.Redirect(w, r, r.Referer(), http.StatusSeeOther)
		return
	}
	middleware.SetFlash(w, r, "ok", fmt.Sprintf("Kurzy aktualizovány: %d zápasů.", fetched))
	if len(unmatched) > 0 {
		q := strings.Join(unmatched, "|")
		http.Redirect(w, r, "/admin/odds/map?teams="+strings.ReplaceAll(q, " ", "+"), http.StatusSeeOther)
		return
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

func fetchAndStoreOdds(apiKey string) (int, []string, error) {
	ctx := context.Background()

	rows, err := db.Pool.Query(ctx, `SELECT DISTINCT c.fd_code FROM competitions c WHERE c.is_active = true AND c.fd_code != ''`)
	if err != nil {
		return 0, nil, err
	}
	var fdCodes []string
	for rows.Next() {
		var code string
		_ = rows.Scan(&code)
		fdCodes = append(fdCodes, code)
	}
	rows.Close()

	total := 0
	var allUnmatched []string
	for _, fdCode := range fdCodes {
		sportKey, ok := sportKeys[fdCode]
		if !ok {
			continue
		}
		n, unmatched, err := fetchSportOdds(ctx, apiKey, sportKey, fdCode)
		if err != nil {
			log.Printf("[odds] %s: %v", sportKey, err)
			continue
		}
		total += n
		allUnmatched = append(allUnmatched, unmatched...)
	}
	return total, allUnmatched, nil
}

func fetchSportOdds(ctx context.Context, apiKey, sportKey, fdCode string) (int, []string, error) {
	url := fmt.Sprintf(
		"https://api.the-odds-api.com/v4/sports/%s/odds/?apiKey=%s&regions=eu&markets=h2h&oddsFormat=decimal",
		sportKey, apiKey,
	)
	resp, err := http.Get(url)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return 0, nil, fmt.Errorf("API %d: %s", resp.StatusCode, string(body))
	}

	var events []oddsAPIEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return 0, nil, err
	}

	saved := 0
	var unmatched []string
	for _, ev := range events {
		commenceTime, err := time.Parse(time.RFC3339, ev.CommenceTime)
		if err != nil {
			continue
		}

		matchID := findMatchByTeamsAndTime(ctx, fdCode, ev.HomeTeam, ev.AwayTeam, commenceTime)
		log.Printf("[odds] event %s vs %s @ %s UTC → matchID=%d", ev.HomeTeam, ev.AwayTeam, commenceTime.Format("02.01 15:04"), matchID)
		if matchID == 0 {
			unmatched = append(unmatched, fmt.Sprintf("%s vs %s (UTC %s)", ev.HomeTeam, ev.AwayTeam, commenceTime.Format("02.01 15:04")))
			continue
		}

		// Zkus najít kurzy — loguj pokud žádný bookmaker nemá h2h
		hasOdds := false

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
				log.Printf("[odds] saved matchID=%d %s vs %s %.2f/%.2f/%.2f", matchID, ev.HomeTeam, ev.AwayTeam, home, draw, away)
				saved++
				hasOdds = true
				goto nextEvent
			}
		}
	nextEvent:
		if !hasOdds {
			log.Printf("[odds] matchID=%d (%s vs %s): no bookmaker has h2h odds (bookmakers: %d)", matchID, ev.HomeTeam, ev.AwayTeam, len(ev.Bookmakers))
			unmatched = append(unmatched, fmt.Sprintf("%s vs %s (matched, no h2h odds)", ev.HomeTeam, ev.AwayTeam))
		}
	}
	return saved, unmatched, nil
}

func findMatchByTeamsAndTime(ctx context.Context, fdCode, homeTeam, awayTeam string, t time.Time) int {
	homeLow := strings.ToLower(homeTeam)

	// Primárně: najdi domácí tým přes alias nebo jméno v dané soutěži,
	// pak ověř datum (±3h). Domácí tým je v dané soutěži unikátní pro dané datum.
	rows, err := db.Pool.Query(ctx, `
		SELECT m.id, ht.name, COALESCE(ht.display_name,''), COALESCE(ht.alias,''),
		              at.name, COALESCE(at.display_name,''), COALESCE(at.alias,'')
		FROM matches m
		JOIN competitions c ON c.id = m.competition_id
		JOIN teams ht ON ht.id = m.home_team_id
		JOIN teams at ON at.id = m.away_team_id
		WHERE c.fd_code = $1
		  AND m.match_date BETWEEN $2 AND $3`,
		fdCode,
		t.Add(-4*time.Hour),
		t.Add(4*time.Hour),
	)
	if err != nil {
		log.Printf("[odds] findMatch query error: %v", err)
		return 0
	}
	defer rows.Close()

	awayLow := strings.ToLower(awayTeam)
	bestID := 0
	bestScore := -1
	for rows.Next() {
		var id int
		var hn, hd, ha, an, ad, aa string
		_ = rows.Scan(&id, &hn, &hd, &ha, &an, &ad, &aa)
		// Domácí tým musí skórovat alespoň 10 — pak přidáme skóre hostů
		hs := teamScore(homeLow, hn, hd, ha)
		if hs < 10 {
			continue
		}
		score := hs + teamScore(awayLow, an, ad, aa)
		if score > bestScore {
			bestScore = score
			bestID = id
		}
	}
	if bestID == 0 {
		log.Printf("[odds] no home-team match for %s vs %s @ %s", homeTeam, awayTeam, t.Format("02.01 15:04"))
		return 0
	}
	log.Printf("[odds] matched %s vs %s → matchID=%d (score=%d)", homeTeam, awayTeam, bestID, bestScore)
	return bestID
}

// teamScore vrátí skóre podobnosti mezi API jménem a jmény/aliasem týmu v DB.
// Alias (anglické jméno z Odds API) má nejvyšší prioritu.
func teamScore(apiLow, dbName, dbDisplay, dbAlias string) int {
	// Přesná shoda aliasu — nejvyšší priorita
	if dbAlias != "" && strings.ToLower(dbAlias) == apiLow {
		return 20
	}
	dbLow := strings.ToLower(dbName)
	dbDispLow := strings.ToLower(dbDisplay)
	if apiLow == dbLow || apiLow == dbDispLow {
		return 10
	}
	if strings.Contains(dbLow, apiLow) || strings.Contains(apiLow, dbLow) {
		return 5
	}
	if strings.Contains(dbDispLow, apiLow) || strings.Contains(apiLow, dbDispLow) {
		return 5
	}
	// Alias substring
	if dbAlias != "" {
		aliasLow := strings.ToLower(dbAlias)
		if strings.Contains(aliasLow, apiLow) || strings.Contains(apiLow, aliasLow) {
			return 8
		}
	}
	// Shoda slova (např. "Barcelona" vs "FC Barcelona")
	for _, aw := range strings.Fields(apiLow) {
		if len(aw) < 3 {
			continue
		}
		for _, dw := range strings.Fields(dbLow) {
			if aw == dw {
				return 3
			}
		}
	}
	return 0
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

// GET /admin/odds/list — přehled uložených kurzů z match_odds
func AdminOddsList(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := GetCurrentUser(r)
		if user == nil || !user.IsOwner {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ctx := context.Background()

		type oddsRow struct {
			MatchID    int64
			HomeTeam   string
			AwayTeam   string
			MatchDate  string
			CompName   string
			HomeOdds   string
			DrawOdds   string
			AwayOdds   string
			Bookmaker  string
			FetchedAt  string
			MatchFound bool
		}

		rows, err := db.Pool.Query(ctx, `
			SELECT mo.match_id,
			       COALESCE(ht.display_name, ht.name, '???'),
			       COALESCE(at.display_name, at.name, '???'),
			       COALESCE(TO_CHAR(m.match_date AT TIME ZONE 'Europe/Prague', 'DD.MM.YYYY HH24:MI'), '— zápas nenalezen —'),
			       COALESCE(c.name, '— soutěž nenalezena —'),
			       mo.home_odds, mo.draw_odds, mo.away_odds,
			       mo.bookmaker,
			       TO_CHAR(mo.fetched_at AT TIME ZONE 'Europe/Prague', 'DD.MM HH24:MI'),
			       m.id IS NOT NULL
			FROM match_odds mo
			LEFT JOIN matches m ON m.id = mo.match_id
			LEFT JOIN teams ht ON ht.id = m.home_team_id
			LEFT JOIN teams at ON at.id = m.away_team_id
			LEFT JOIN competitions c ON c.id = m.competition_id
			ORDER BY mo.fetched_at DESC
			LIMIT 200`)
		if err != nil {
			http.Error(w, "db error: "+err.Error(), 500)
			return
		}
		defer rows.Close()

		var data []oddsRow
		for rows.Next() {
			var row oddsRow
			var h, d, a float64
			_ = rows.Scan(&row.MatchID, &row.HomeTeam, &row.AwayTeam, &row.MatchDate, &row.CompName, &h, &d, &a, &row.Bookmaker, &row.FetchedAt, &row.MatchFound)
			row.HomeOdds = fmt.Sprintf("%.2f", h)
			row.DrawOdds = fmt.Sprintf("%.2f", d)
			row.AwayOdds = fmt.Sprintf("%.2f", a)
			data = append(data, row)
		}

		RenderTemplate(w, r, tmpl, "admin/odds_list.html", TemplateData{
			"User":      user,
			"OddsRows":  data,
			"CSRFToken": middleware.GetCSRFToken(w, r),
		})
	}
}

// GET /admin/odds/raw — surová data z Odds API bez párování
func AdminOddsRaw(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := GetCurrentUser(r)
		if user == nil || !user.IsOwner {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		apiKey := os.Getenv("ODDS_API_KEY")
		if apiKey == "" {
			http.Error(w, "ODDS_API_KEY není nastaven", 500)
			return
		}

		type rawRow struct {
			Sport     string
			HomeTeam  string
			AwayTeam  string
			StartUTC  string
			HomeOdds  string
			DrawOdds  string
			AwayOdds  string
			Bookmaker string
		}

		var rows []rawRow
		for fdCode, sportKey := range sportKeys {
			url := fmt.Sprintf(
				"https://api.the-odds-api.com/v4/sports/%s/odds/?apiKey=%s&regions=eu&markets=h2h&oddsFormat=decimal",
				sportKey, apiKey,
			)
			resp, err := http.Get(url)
			if err != nil || resp.StatusCode != 200 {
				if resp != nil { resp.Body.Close() }
				continue
			}
			var events []oddsAPIEvent
			json.NewDecoder(resp.Body).Decode(&events)
			resp.Body.Close()

			for _, ev := range events {
				t, _ := time.Parse(time.RFC3339, ev.CommenceTime)
				row := rawRow{
					Sport:    fdCode,
					HomeTeam: ev.HomeTeam,
					AwayTeam: ev.AwayTeam,
					StartUTC: t.UTC().Format("02.01.2006 15:04"),
				}
				for _, bm := range ev.Bookmakers {
					for _, mkt := range bm.Markets {
						if mkt.Key != "h2h" { continue }
						var home, draw, away float64
						for _, o := range mkt.Outcomes {
							switch o.Name {
							case ev.HomeTeam: home = o.Price
							case ev.AwayTeam: away = o.Price
							case "Draw": draw = o.Price
							}
						}
						if home > 0 && away > 0 {
							row.HomeOdds = fmt.Sprintf("%.2f", home)
							row.DrawOdds = fmt.Sprintf("%.2f", draw)
							row.AwayOdds = fmt.Sprintf("%.2f", away)
							row.Bookmaker = bm.Title
							goto nextRaw
						}
					}
				}
			nextRaw:
				rows = append(rows, row)
			}
		}

		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Sport != rows[j].Sport { return rows[i].Sport < rows[j].Sport }
			return rows[i].StartUTC < rows[j].StartUTC
		})

		RenderTemplate(w, r, tmpl, "admin/odds_raw.html", TemplateData{
			"User": user,
			"Rows": rows,
		})
	}
}

type dbCandidate struct {
	MatchID   int
	HomeTeam  string
	AwayTeam  string
	MatchDate string
	Score     int
}

func findMatchCandidates(ctx context.Context, fdCode, homeTeam, awayTeam string, t time.Time) []dbCandidate {
	rows, err := db.Pool.Query(ctx, `
		SELECT m.id, ht.name, ht.display_name, COALESCE(ht.alias,''), at.name, at.display_name, COALESCE(at.alias,''),
		       TO_CHAR(m.match_date AT TIME ZONE 'Europe/Prague', 'DD.MM HH24:MI')
		FROM matches m
		JOIN competitions c ON c.id = m.competition_id
		JOIN teams ht ON ht.id = m.home_team_id
		JOIN teams at ON at.id = m.away_team_id
		WHERE c.fd_code = $1
		  AND m.match_date BETWEEN $2 AND $3`,
		fdCode, t.Add(-3*time.Hour), t.Add(3*time.Hour),
	)
	if err != nil { return nil }
	defer rows.Close()

	homeLow := strings.ToLower(homeTeam)
	awayLow := strings.ToLower(awayTeam)
	var result []dbCandidate
	for rows.Next() {
		var id int
		var hn, hd, ha, an, ad, aa, dt string
		_ = rows.Scan(&id, &hn, &hd, &ha, &an, &ad, &aa, &dt)
		score := teamScore(homeLow, hn, hd, ha) + teamScore(awayLow, an, ad, aa)
		result = append(result, dbCandidate{id, hn + " / " + hd, an + " / " + ad, dt, score})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Score > result[j].Score })
	return result
}

// GET /admin/odds/preview — živý náhled všech eventů z Odds API + stav párování
func AdminOddsPreview(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := GetCurrentUser(r)
		if user == nil || !user.IsOwner {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		apiKey := os.Getenv("ODDS_API_KEY")
		if apiKey == "" {
			http.Error(w, "ODDS_API_KEY není nastaven", 500)
			return
		}
		ctx := context.Background()

		type previewRow struct {
			HomeTeam   string
			AwayTeam   string
			StartUTC   string
			HomeOdds   string
			DrawOdds   string
			AwayOdds   string
			Bookmaker  string
			MatchID    int64
			Status     string
			Candidates []dbCandidate
		}

		var rows []previewRow
		for fdCode, sportKey := range sportKeys {
			url := fmt.Sprintf(
				"https://api.the-odds-api.com/v4/sports/%s/odds/?apiKey=%s&regions=eu&markets=h2h&oddsFormat=decimal",
				sportKey, apiKey,
			)
			resp, err := http.Get(url)
			if err != nil || resp.StatusCode != 200 {
				if resp != nil { resp.Body.Close() }
				continue
			}
			var events []oddsAPIEvent
			json.NewDecoder(resp.Body).Decode(&events)
			resp.Body.Close()

			for _, ev := range events {
				t, err := time.Parse(time.RFC3339, ev.CommenceTime)
				if err != nil { continue }
				row := previewRow{
					HomeTeam:   ev.HomeTeam,
					AwayTeam:   ev.AwayTeam,
					StartUTC:   t.UTC().Format("02.01.2006 15:04"),
					Candidates: findMatchCandidates(ctx, fdCode, ev.HomeTeam, ev.AwayTeam, t),
				}
				mid := findMatchByTeamsAndTime(ctx, fdCode, ev.HomeTeam, ev.AwayTeam, t)
				if mid == 0 {
					row.Status = "no_match"
				} else {
					row.MatchID = int64(mid)
					for _, bm := range ev.Bookmakers {
						for _, mkt := range bm.Markets {
							if mkt.Key != "h2h" { continue }
							var home, draw, away float64
							for _, o := range mkt.Outcomes {
								switch o.Name {
								case ev.HomeTeam: home = o.Price
								case ev.AwayTeam: away = o.Price
								case "Draw": draw = o.Price
								}
							}
							if home > 0 && away > 0 {
								row.HomeOdds = fmt.Sprintf("%.2f", home)
								row.DrawOdds = fmt.Sprintf("%.2f", draw)
								row.AwayOdds = fmt.Sprintf("%.2f", away)
								row.Bookmaker = bm.Title
								row.Status = "ok"
								goto nextEv
							}
						}
					}
					row.Status = "no_odds"
				}
			nextEv:
				rows = append(rows, row)
			}
		}

		sort.Slice(rows, func(i, j int) bool {
			order := map[string]int{"ok": 0, "no_odds": 1, "no_match": 2}
			if order[rows[i].Status] != order[rows[j].Status] {
				return order[rows[i].Status] < order[rows[j].Status]
			}
			return rows[i].StartUTC < rows[j].StartUTC
		})

		RenderTemplate(w, r, tmpl, "admin/odds_preview.html", TemplateData{
			"User":      user,
			"Rows":      rows,
			"CSRFToken": middleware.GetCSRFToken(w, r),
		})
	}
}

// GET /admin/odds/map — přehled aliasů týmů pro Odds API
func AdminOddsMapForm(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := GetCurrentUser(r)
		if user == nil || !user.IsOwner {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		// Nespárované z query param (z redirectu po refresh)
		teamsParam := r.URL.Query().Get("teams")
		var unmatched []string
		for _, t := range strings.Split(teamsParam, "|") {
			t = strings.TrimSpace(t)
			if t != "" {
				unmatched = append(unmatched, t)
			}
		}

		ctx := context.Background()

		// Načti soutěže s fd_code
		type compRow struct {
			ID     int
			Name   string
			FdCode string
		}
		var comps []compRow
		crows, _ := db.Pool.Query(ctx, `SELECT id, name, fd_code FROM competitions WHERE fd_code != '' AND fd_code IS NOT NULL ORDER BY name`)
		defer crows.Close()
		for crows.Next() {
			var c compRow
			_ = crows.Scan(&c.ID, &c.Name, &c.FdCode)
			comps = append(comps, c)
		}

		// Vybraná soutěž
		compID, _ := strconv.Atoi(r.URL.Query().Get("competition_id"))
		var selectedComp compRow
		for _, c := range comps {
			if c.ID == compID {
				selectedComp = c
				break
			}
		}
		if compID == 0 && len(comps) > 0 {
			selectedComp = comps[0]
			compID = selectedComp.ID
		}

		var dbTeams []oddsDbTeam
		if compID > 0 {
			trows, _ := db.Pool.Query(ctx, `
				SELECT DISTINCT t.id, COALESCE(t.display_name, t.name), COALESCE(t.alias, '')
				FROM teams t
				JOIN matches m ON (m.home_team_id=t.id OR m.away_team_id=t.id)
				WHERE m.competition_id=$1
				ORDER BY COALESCE(t.display_name, t.name)`, compID)
			defer trows.Close()
			for trows.Next() {
				var tr oddsDbTeam
				_ = trows.Scan(&tr.ID, &tr.Name, &tr.Alias)
				dbTeams = append(dbTeams, tr)
			}
		}

		var apiTeams []oddsApiTeam
		apiKey := os.Getenv("ODDS_API_KEY")
		if apiKey != "" && selectedComp.FdCode != "" {
			sportKey := sportKeys[selectedComp.FdCode]
			if sportKey != "" {
				apiTeams = fetchApiTeamNames(apiKey, sportKey, dbTeams)
			}
		}

		RenderTemplate(w, r, tmpl, "admin/odds_map.html", TemplateData{
			"User":      user,
			"DbTeams":   dbTeams,
			"ApiTeams":  apiTeams,
			"Comps":     comps,
			"CompID":    compID,
			"Unmatched": unmatched,
			"CSRFToken": middleware.GetCSRFToken(w, r),
			"Flash":     middleware.GetFlash(w, r),
		})
	}
}

// POST /admin/odds/map — uloží aliasy
func AdminOddsMapSubmit(w http.ResponseWriter, r *http.Request) {
	user := GetCurrentUser(r)
	if user == nil || !user.IsOwner {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := context.Background()
	saved := 0
	for key, vals := range r.Form {
		if !strings.HasPrefix(key, "map_") || len(vals) == 0 {
			continue
		}
		apiName := strings.TrimPrefix(key, "map_")
		dbIDStr := strings.TrimSpace(vals[0])
		if dbIDStr == "" {
			continue
		}
		teamID, err := strconv.Atoi(dbIDStr)
		if err != nil || teamID == 0 {
			continue
		}
		_, _ = db.Pool.Exec(ctx, `UPDATE teams SET alias=$1 WHERE id=$2`, apiName, teamID)
		saved++
	}
	compIDStr := r.FormValue("competition_id")
	middleware.SetFlash(w, r, "ok", fmt.Sprintf("Uloženo %d aliasů.", saved))
	http.Redirect(w, r, "/admin/odds/map?competition_id="+compIDStr, http.StatusSeeOther)
}

// fetchApiTeamNames načte unikátní jména týmů z Odds API a doplní aktuální DB mapování
func fetchApiTeamNames(apiKey, sportKey string, dbTeams []oddsDbTeam) []oddsApiTeam {

	url := fmt.Sprintf(
		"https://api.the-odds-api.com/v4/sports/%s/odds/?apiKey=%s&regions=eu&markets=h2h&oddsFormat=decimal",
		sportKey, apiKey,
	)
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != 200 {
		return nil
	}
	defer resp.Body.Close()

	var events []oddsAPIEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil
	}

	// Sbírej unikátní jména
	seen := map[string]bool{}
	var result []oddsApiTeam
	for _, ev := range events {
		for _, name := range []string{ev.HomeTeam, ev.AwayTeam} {
			if seen[name] {
				continue
			}
			seen[name] = true
			row := oddsApiTeam{ApiName: name}
			nameLow := strings.ToLower(name)
			for _, dt := range dbTeams {
				if strings.ToLower(dt.Alias) == nameLow {
					row.DbID = dt.ID
					row.DbName = dt.Name
					break
				}
			}
			result = append(result, row)
		}
	}
	// Seřaď: nejdřív nemapované, pak abecedně
	sort.Slice(result, func(i, j int) bool {
		if (result[i].DbID == 0) != (result[j].DbID == 0) {
			return result[i].DbID == 0
		}
		return result[i].ApiName < result[j].ApiName
	})
	return result
}
