// handlers/admin_match_import.go — Tipovačka 3.0
// Hromadný import budoucích zápasů (bez skóre) přes paste textu.
//
// Endpointy:
//   GET  /admin/matches/import          — formulář
//   POST /admin/matches/import/parse    — parsování, náhled
//   POST /admin/matches/import/confirm  — uložení do DB
//   POST /admin/matches/import/cancel   — zrušení
//
// Podporované formáty vstupu (datum a čas kdekoliv v řádku):
//   Domácí - Hosté  15.5. 18:00
//   2. 5. 2026  Domácí Hosté  18:00       (bez pomlčky → matchuj z DB nebo split)
//   2. 5. 2026  Domácí - Hosté  18:00
//   Excel tabulátor: datum⇥Domácí⇥Hosté⇥čas  (libovolné pořadí sloupců)
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"tipovacka/db"
	"tipovacka/middleware"
)

// ── regexpy ──────────────────────────────────────────────────────────────────

// miDateRe: DD.MM., DD. MM., DD.MM.YYYY, DD. MM. YYYY
var miDateRe = regexp.MustCompile(`(\d{1,2})\.[ \t]*(\d{1,2})\.[ \t]*(\d{4})?`)

// miTimeColonRe: HH:MM kdekoliv (dvojtečka = bezpečné, nezaměnitelné s datem)
var miTimeColonRe = regexp.MustCompile(`(\d{1,2}):(\d{2})`)

// miTimeDotEndRe: HH.MM na konci řádku (fallback pokud není dvojtečka)
var miTimeDotEndRe = regexp.MustCompile(`(\d{1,2})\.(\d{2})[ \t]*$`)

// mi2PlusSpaces: 2+ mezer/tabulátorů
var mi2PlusSpaces = regexp.MustCompile(`[ \t]{2,}`)

// ── typy ─────────────────────────────────────────────────────────────────────

type importMatchParsed struct {
	HomeTeam   string  `json:"home_team"`
	AwayTeam   string  `json:"away_team"`
	DateStr    string  `json:"date_str"`    // "15.05.2026 18:00"
	ParsedDate *string `json:"parsed_date"` // "2026-05-15T18:04" pro vložení
	RawLine    string  `json:"raw_line"`
	ParseError string  `json:"parse_error,omitempty"`
	FromDB     bool    `json:"from_db"` // true = oba týmy nalezeny v DB soutěže
}

// ── normalizace názvů týmů pro porovnání ─────────────────────────────────────

func normTeamName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimRight(s, ".")
	// odstraň interpunkci kromě písmen, číslic a mezer
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' {
			return r
		}
		return -1
	}, s)
}

// ── matchování týmů z DB ──────────────────────────────────────────────────────

// scoreTeam vrátí shodu kandidáta s normalizovaným názvem týmu.
// 3 = přesná shoda, 2 = prefix (zkratka), 1 = podřetězec, 0 = neshoda.
func scoreTeam(candidate string, normKnown []string, origKnown []string) (string, int) {
	c := normTeamName(candidate)
	if c == "" {
		return "", 0
	}
	best, bestScore := "", 0
	for i, nk := range normKnown {
		var sc int
		switch {
		case nk == c:
			sc = 3
		case strings.HasPrefix(nk, c):
			sc = 2
		case strings.Contains(nk, c):
			sc = 1
		}
		if sc > bestScore {
			bestScore = sc
			best = origKnown[i]
		}
	}
	return best, bestScore
}

// matchTeamsWithKnown zkusí rozpoznat oba týmy z textu pomocí DB.
// Projde všechna možná rozdělení slov a vybere to s nejvyšším skóre.
// Vrátí home, away, fromDB.
func matchTeamsWithKnown(teamsPart string, knownTeams []string) (home, away string, fromDB bool) {
	if len(knownTeams) == 0 {
		h, a := splitMITeams(teamsPart)
		return h, a, false
	}

	// Připrav normalizované verze
	normKnown := make([]string, len(knownTeams))
	for i, t := range knownTeams {
		normKnown[i] = normTeamName(t)
	}

	// Pokud je pomlčka / tabulátor / 2+mezery → split bez hledání v DB
	for _, sep := range []string{" - ", " – ", " — "} {
		if idx := strings.Index(teamsPart, sep); idx >= 0 {
			h := strings.TrimSpace(teamsPart[:idx])
			a := strings.TrimSpace(teamsPart[idx+len(sep):])
			// Pokus o zpřesnění názvů z DB
			if hm, hs := scoreTeam(h, normKnown, knownTeams); hs >= 2 {
				h = hm
				fromDB = true
			}
			if am, as := scoreTeam(a, normKnown, knownTeams); as >= 2 {
				a = am
				fromDB = true
			}
			return h, a, fromDB
		}
	}
	if idx := strings.IndexByte(teamsPart, '\t'); idx >= 0 {
		return strings.TrimSpace(teamsPart[:idx]), strings.TrimSpace(teamsPart[idx+1:]), false
	}
	if loc := mi2PlusSpaces.FindStringIndex(teamsPart); loc != nil {
		h := strings.TrimSpace(teamsPart[:loc[0]])
		a := strings.TrimSpace(teamsPart[loc[1]:])
		if hm, hs := scoreTeam(h, normKnown, knownTeams); hs >= 2 {
			h = hm; fromDB = true
		}
		if am, as := scoreTeam(a, normKnown, knownTeams); as >= 2 {
			a = am; fromDB = true
		}
		return h, a, fromDB
	}

	// Žádný explicitní oddělovač → projdi všechna rozdělení slov
	words := strings.Fields(teamsPart)
	if len(words) < 2 {
		if len(words) == 1 {
			return teamsPart, "", false
		}
		return "", "", false
	}

	bestScore := -1
	var bestHome, bestAway string
	bestFromDB := false

	for split := 1; split < len(words); split++ {
		hCand := strings.Join(words[:split], " ")
		aCand := strings.Join(words[split:], " ")
		hMatch, hSc := scoreTeam(hCand, normKnown, knownTeams)
		aMatch, aSc := scoreTeam(aCand, normKnown, knownTeams)
		total := hSc + aSc
		if hSc > 0 && aSc > 0 && total > bestScore {
			bestScore = total
			bestHome = hMatch
			bestAway = aMatch
			bestFromDB = true
		}
	}

	if bestFromDB {
		return bestHome, bestAway, true
	}

	// Fallback: split na první mezeře
	h, a := splitMITeams(teamsPart)
	return h, a, false
}

// splitMITeams jednoduché rozdělení bez DB (pomlčka → tab → 2+mezery → 1. mezera).
func splitMITeams(s string) (home, away string) {
	for _, sep := range []string{" - ", " – ", " — "} {
		if idx := strings.Index(s, sep); idx >= 0 {
			return strings.TrimSpace(s[:idx]), strings.TrimSpace(s[idx+len(sep):])
		}
	}
	if idx := strings.IndexByte(s, '\t'); idx >= 0 {
		return strings.TrimSpace(s[:idx]), strings.TrimSpace(s[idx+1:])
	}
	if loc := mi2PlusSpaces.FindStringIndex(s); loc != nil {
		return strings.TrimSpace(s[:loc[0]]), strings.TrimSpace(s[loc[1]:])
	}
	if idx := strings.IndexByte(s, ' '); idx >= 0 {
		return strings.TrimSpace(s[:idx]), strings.TrimSpace(s[idx+1:])
	}
	return s, ""
}

// ── parsování jednoho řádku ───────────────────────────────────────────────────

func parseMILine(line string, currentYear int, knownTeams []string) importMatchParsed {
	orig := line
	line = strings.TrimSpace(line)

	// ── 1. Najdi čas kdekoliv (přednost: HH:MM s dvojtečkou) ────────────────
	var hour, min, timeStart, timeEnd int
	found := false

	// Projdi všechny HH:MM (colon) matchey, vyber poslední platný
	for _, m := range miTimeColonRe.FindAllStringSubmatchIndex(line, -1) {
		h, _ := strconv.Atoi(line[m[2]:m[3]])
		mn, _ := strconv.Atoi(line[m[4]:m[5]])
		if h <= 23 && mn <= 59 {
			hour, min = h, mn
			timeStart, timeEnd = m[0], m[1]
			found = true
		}
	}
	if !found {
		// Fallback: HH.MM na konci řádku
		if m := miTimeDotEndRe.FindStringSubmatchIndex(line); m != nil {
			h, _ := strconv.Atoi(line[m[2]:m[3]])
			mn, _ := strconv.Atoi(line[m[4]:m[5]])
			if h <= 23 && mn <= 59 {
				hour, min = h, mn
				timeStart, timeEnd = m[0], m[1]
				found = true
			}
		}
	}
	if !found {
		return importMatchParsed{RawLine: orig,
			ParseError: "čas nenalezen — přidej čas ve formátu HH:MM (např. 18:00)"}
	}

	// Odstraň čas → zbytek = datum + týmy
	rest := strings.TrimSpace(line[:timeStart] + " " + line[timeEnd:])
	rest = strings.TrimSpace(rest)

	// ── 2. Najdi datum kdekoliv v zbytku ────────────────────────────────────
	dtIdx := miDateRe.FindStringSubmatchIndex(rest)
	if dtIdx == nil {
		return importMatchParsed{RawLine: orig,
			ParseError: "datum nenalezeno — použij formát DD.MM. nebo DD. MM. YYYY"}
	}
	day, _ := strconv.Atoi(rest[dtIdx[2]:dtIdx[3]])
	month, _ := strconv.Atoi(rest[dtIdx[4]:dtIdx[5]])
	year := currentYear
	if dtIdx[6] >= 0 && dtIdx[7] >= 0 {
		year, _ = strconv.Atoi(rest[dtIdx[6]:dtIdx[7]])
		if year < 100 {
			year += 2000
		}
	}
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return importMatchParsed{RawLine: orig,
			ParseError: fmt.Sprintf("neplatné datum %02d.%02d.", day, month)}
	}

	// Odstraň datum → zbydou týmy
	before := strings.TrimSpace(rest[:dtIdx[0]])
	after := strings.TrimSpace(rest[dtIdx[1]:])
	teamsPart := before
	if teamsPart == "" {
		teamsPart = after
	} else if after != "" {
		teamsPart = before + "  " + after
	}
	teamsPart = strings.TrimSpace(teamsPart)
	if teamsPart == "" {
		return importMatchParsed{RawLine: orig, ParseError: "chybí názvy týmů"}
	}

	// ── 3. Rozpoznej týmy (DB nebo split) ────────────────────────────────────
	homeTeam, awayTeam, fromDB := matchTeamsWithKnown(teamsPart, knownTeams)
	if homeTeam == "" || awayTeam == "" {
		return importMatchParsed{RawLine: orig,
			ParseError: "nepodařilo se rozpoznat oba týmy — zkus formát: Domácí - Hosté"}
	}

	dateStr := fmt.Sprintf("%02d.%02d.%04d %02d:%02d", day, month, year, hour, min)
	isoStr := fmt.Sprintf("%04d-%02d-%02dT%02d:%02d", year, month, day, hour, min)
	return importMatchParsed{
		HomeTeam: homeTeam, AwayTeam: awayTeam,
		DateStr: dateStr, ParsedDate: &isoStr,
		RawLine: orig, FromDB: fromDB,
	}
}

// parseMatchImportText parsuje celý vložený text.
func parseMatchImportText(text string, knownTeams []string) []importMatchParsed {
	currentYear := time.Now().In(pragueLocation).Year()
	var results []importMatchParsed
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		results = append(results, parseMILine(line, currentYear, knownTeams))
	}
	return results
}

// ── loadCompTeams: týmy soutěže pro daný round ────────────────────────────────

func loadCompTeams(ctx context.Context, roundID int) []string {
	rows, _ := db.Pool.Query(ctx,
		`SELECT COALESCE(t.display_name, t.name)
		   FROM teams t
		   JOIN competition_teams ct ON ct.team_id = t.id
		   JOIN rounds r ON r.competition_id = ct.competition_id
		  WHERE r.id = $1
		  ORDER BY t.name`, roundID)
	var teams []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			teams = append(teams, name)
		}
	}
	rows.Close()
	return teams
}

// ── session helpers ───────────────────────────────────────────────────────────

func miSetSession(w http.ResponseWriter, r *http.Request, matches []importMatchParsed, roundID int) {
	sess := middleware.GetSession(r)
	b, _ := json.Marshal(matches)
	sess.Values["mi_parsed"] = string(b)
	sess.Values["mi_round_id"] = roundID
	_ = sess.Save(r, w)
}

func miGetSession(r *http.Request) ([]importMatchParsed, int) {
	sess := middleware.GetSession(r)
	var matches []importMatchParsed
	if v, ok := sess.Values["mi_parsed"].(string); ok {
		_ = json.Unmarshal([]byte(v), &matches)
	}
	roundID := 0
	if v, ok := sess.Values["mi_round_id"].(int); ok {
		roundID = v
	}
	return matches, roundID
}

func miClearSession(w http.ResponseWriter, r *http.Request) {
	sess := middleware.GetSession(r)
	delete(sess.Values, "mi_parsed")
	delete(sess.Values, "mi_round_id")
	_ = sess.Save(r, w)
}

// ── loadActiveRounds ─ sdílený helper pro dropdown ────────────────────────────

func loadActiveRounds(ctx context.Context) []ocrRoundItem {
	rows, _ := db.Pool.Query(ctx,
		`SELECT r.id, r.name, c.name, c.season
		   FROM rounds r
		   JOIN competitions c ON c.id = r.competition_id
		  WHERE COALESCE(c.is_active, false) = true
		  ORDER BY c.sort_order ASC NULLS LAST, c.id DESC, r.id DESC`)
	var rounds []ocrRoundItem
	for rows.Next() {
		var ri ocrRoundItem
		_ = rows.Scan(&ri.ID, &ri.Name, &ri.CompName, &ri.CompSeason)
		rounds = append(rounds, ri)
	}
	rows.Close()
	return rounds
}

// ── GET /admin/matches/import ─────────────────────────────────────────────────

func AdminMatchImportForm(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		RenderTemplate(w, r, tmpl, "admin/match_import.html", TemplateData{
			"User":   admin,
			"Rounds": loadActiveRounds(context.Background()),
			"Error":  nil,
		})
	}
}

// ── POST /admin/matches/import/parse ─────────────────────────────────────────

func AdminMatchImportParse(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}

		ctx := context.Background()
		rounds := loadActiveRounds(ctx)

		showError := func(msg string) {
			RenderTemplate(w, r, tmpl, "admin/match_import.html", TemplateData{
				"User":   admin,
				"Rounds": rounds,
				"Error":  msg,
			})
		}

		roundID, _ := strconv.Atoi(r.FormValue("round_id"))
		text := strings.TrimSpace(r.FormValue("text"))

		if roundID == 0 {
			showError("Vyber kolo.")
			return
		}
		if text == "" {
			showError("Vložte text se zápasy.")
			return
		}

		// Ověř kolo
		var roundName, compName string
		err := db.Pool.QueryRow(ctx,
			`SELECT r.name, c.name FROM rounds r JOIN competitions c ON c.id = r.competition_id WHERE r.id = $1`,
			roundID).Scan(&roundName, &compName)
		if err != nil {
			showError("Kolo nenalezeno.")
			return
		}

		// Načti týmy soutěže pro chytré rozpoznávání
		knownTeams := loadCompTeams(ctx, roundID)

		parsed := parseMatchImportText(text, knownTeams)
		if len(parsed) == 0 {
			showError("Nepodařilo se rozpoznat žádné zápasy.")
			return
		}

		miSetSession(w, r, parsed, roundID)

		RenderTemplate(w, r, tmpl, "admin/match_import_preview.html", TemplateData{
			"User":      admin,
			"Parsed":    parsed,
			"RoundID":   roundID,
			"RoundName": roundName,
			"CompName":  compName,
		})
	}
}

// ── POST /admin/matches/import/confirm ────────────────────────────────────────

func AdminMatchImportConfirm(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}

	parsed, roundID := miGetSession(r)
	miClearSession(w, r)

	if len(parsed) == 0 || roundID == 0 {
		http.Redirect(w, r, "/admin/matches/import", http.StatusSeeOther)
		return
	}

	ctx := context.Background()

	var compID int
	sport := "football"
	if err := db.Pool.QueryRow(ctx,
		`SELECT r.competition_id, COALESCE(c.sport,'football')
		   FROM rounds r JOIN competitions c ON c.id = r.competition_id
		  WHERE r.id = $1`, roundID).Scan(&compID, &sport); err != nil {
		middleware.SetFlash(w, r, "error", "Kolo nenalezeno.")
		http.Redirect(w, r, "/admin/matches/import", http.StatusSeeOther)
		return
	}

	created, skipped, errCount := 0, 0, 0
	for _, m := range parsed {
		if m.ParseError != "" {
			errCount++
			continue
		}

		homeID, _ := upsertTeamByName(ctx, m.HomeTeam, sport)
		if homeID == 0 {
			errCount++
			continue
		}
		awayID, _ := upsertTeamByName(ctx, m.AwayTeam, sport)
		if awayID == 0 {
			errCount++
			continue
		}

		_, _ = db.Pool.Exec(ctx,
			`INSERT INTO competition_teams (competition_id, team_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			compID, homeID)
		_, _ = db.Pool.Exec(ctx,
			`INSERT INTO competition_teams (competition_id, team_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			compID, awayID)

		var existingID int
		_ = db.Pool.QueryRow(ctx,
			`SELECT id FROM matches WHERE round_id=$1 AND home_team_id=$2 AND away_team_id=$3`,
			roundID, homeID, awayID).Scan(&existingID)
		if existingID > 0 {
			if m.ParsedDate != nil && *m.ParsedDate != "" {
				t, err := time.ParseInLocation("2006-01-02T15:04", *m.ParsedDate, pragueLocation)
				if err == nil {
					_, _ = db.Pool.Exec(ctx, `UPDATE matches SET match_date=$1 WHERE id=$2`, t, existingID)
				}
			}
			skipped++
			continue
		}

		var matchDate *time.Time
		if m.ParsedDate != nil && *m.ParsedDate != "" {
			t, err := time.ParseInLocation("2006-01-02T15:04", *m.ParsedDate, pragueLocation)
			if err == nil {
				matchDate = &t
			}
		}
		_, err := db.Pool.Exec(ctx,
			`INSERT INTO matches (round_id, home_team_id, away_team_id, match_date, is_finished)
			 VALUES ($1,$2,$3,$4,false)`,
			roundID, homeID, awayID, matchDate)
		if err != nil {
			errCount++
			continue
		}
		created++
	}

	msg := fmt.Sprintf("Import dokončen: <b>%d</b> nových zápasů", created)
	if skipped > 0 {
		msg += fmt.Sprintf(", %d přeskočeno (duplicity)", skipped)
	}
	if errCount > 0 {
		msg += fmt.Sprintf(", %d chyb", errCount)
	}
	msg += "."
	middleware.SetFlash(w, r, "ok", msg)
	http.Redirect(w, r, "/admin/rounds/"+strconv.Itoa(roundID)+"/matches", http.StatusSeeOther)
}

// ── POST /admin/matches/import/cancel ────────────────────────────────────────

func AdminMatchImportCancel(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	miClearSession(w, r)
	http.Redirect(w, r, "/admin/matches/import", http.StatusSeeOther)
}
