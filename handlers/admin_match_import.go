// handlers/admin_match_import.go — Tipovačka 3.0
// Hromadný import budoucích zápasů (bez skóre) přes paste textu.
//
// Endpointy:
//   GET  /admin/matches/import          — formulář
//   POST /admin/matches/import/parse    — parsování, náhled
//   POST /admin/matches/import/confirm  — uložení do DB
//   POST /admin/matches/import/cancel   — zrušení
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

	"tipovacka/db"
	"tipovacka/middleware"
)

// ── multi-format parsovací vzory ─────────────────────────────────────────────
//
// Podporované formáty (datum může být kdekoliv, čas vždy na konci řádku):
//   Domácí - Hosté  15.5. 18:00
//   Domácí - Hosté  15.5.2026 18:00
//   15.5. Domácí - Hosté 18:00
//   2. 5. 2026 Domácí Hosté 18:00          (bez pomlčky → split na 1. mezeře)
//   2. 5. 2026 Domácí - Hosté 18:00
//   TAB-oddělené z Excelu: datum\tDomácí\tHosté\tčas
//
// Datum i čas jsou povinné.

// miDateRe hledá datum DD.MM. nebo DD. MM. nebo DD.MM.YYYY nebo DD. MM. YYYY
var miDateRe = regexp.MustCompile(`(\d{1,2})\.[ \t]*(\d{1,2})\.[ \t]*(\d{4})?`)

// miTimeEndRe hledá čas HH:MM nebo HH.MM na KONCI řádku
var miTimeEndRe = regexp.MustCompile(`(\d{1,2})[.:](\d{2})[ \t]*$`)

// mi2PlusSpaces pro detekci vícenásobných mezer
var mi2PlusSpaces = regexp.MustCompile(`[ \t]{2,}`)

type importMatchParsed struct {
	HomeTeam   string  `json:"home_team"`
	AwayTeam   string  `json:"away_team"`
	DateStr    string  `json:"date_str"`    // "15.05.2026 18:00"
	ParsedDate *string `json:"parsed_date"` // "2026-05-15T18:00" pro vložení
	RawLine    string  `json:"raw_line"`
	ParseError string  `json:"parse_error,omitempty"`
}

// splitMITeams rozdělí řetězec "DomácíHosté" na dva týmy.
// Pořadí pokusů: pomlčka → tab → 2+ mezery → první mezera.
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

// parseMILine parsuje jeden řádek a vrátí importMatchParsed.
func parseMILine(line string, currentYear int) importMatchParsed {
	orig := line

	// ── 1. Najdi čas na konci řádku ─────────────────────────────────────────
	tmLoc := miTimeEndRe.FindStringSubmatchIndex(line)
	if tmLoc == nil {
		return importMatchParsed{RawLine: orig,
			ParseError: "čas nenalezen — zadej čas na konci řádku ve formátu HH:MM"}
	}
	hour, _ := strconv.Atoi(line[tmLoc[2]:tmLoc[3]])
	min, _ := strconv.Atoi(line[tmLoc[4]:tmLoc[5]])
	if hour > 23 || min > 59 {
		return importMatchParsed{RawLine: orig,
			ParseError: fmt.Sprintf("neplatný čas %02d:%02d", hour, min)}
	}

	// Odstraň čas z konce → zbytek obsahuje datum + týmy
	rest := strings.TrimRight(line[:tmLoc[0]], " \t")

	// ── 2. Najdi datum kdekoliv v zbytku ────────────────────────────────────
	dtIdx := miDateRe.FindStringSubmatchIndex(rest)
	if dtIdx == nil {
		return importMatchParsed{RawLine: orig,
			ParseError: "datum nenalezeno — použij formát DD.MM. nebo DD. MM. YYYY"}
	}
	day, _ := strconv.Atoi(rest[dtIdx[2]:dtIdx[3]])
	month, _ := strconv.Atoi(rest[dtIdx[4]:dtIdx[5]])
	year := currentYear
	if dtIdx[6] >= 0 {
		year, _ = strconv.Atoi(rest[dtIdx[6]:dtIdx[7]])
		if year < 100 {
			year += 2000
		}
	}
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return importMatchParsed{RawLine: orig,
			ParseError: fmt.Sprintf("neplatné datum %02d.%02d.", day, month)}
	}

	// Odstraň datum → zbydou týmy (před datem nebo za ním)
	before := strings.TrimSpace(rest[:dtIdx[0]])
	after := strings.TrimSpace(rest[dtIdx[1]:])
	teamsPart := before
	if teamsPart == "" {
		teamsPart = after
	} else if after != "" {
		// datum uprostřed (neobvyklé) — spoj s 2+ mezerami jako oddělovač
		teamsPart = before + "  " + after
	}
	if teamsPart == "" {
		return importMatchParsed{RawLine: orig, ParseError: "chybí názvy týmů"}
	}

	// ── 3. Rozděl týmy ───────────────────────────────────────────────────────
	home, away := splitMITeams(teamsPart)
	if home == "" || away == "" {
		return importMatchParsed{RawLine: orig,
			ParseError: "nepodařilo se rozpoznat oba týmy — pro vícesvlovné názvy použij: Domácí - Hosté"}
	}

	dateStr := fmt.Sprintf("%02d.%02d.%04d %02d:%02d", day, month, year, hour, min)
	isoStr := fmt.Sprintf("%04d-%02d-%02dT%02d:%02d", year, month, day, hour, min)
	return importMatchParsed{
		HomeTeam: home, AwayTeam: away,
		DateStr: dateStr, ParsedDate: &isoStr,
		RawLine: orig,
	}
}

// parseMatchImportText parsuje celý vložený text.
func parseMatchImportText(text string) []importMatchParsed {
	now := time.Now().In(pragueLocation)
	currentYear := now.Year()
	var results []importMatchParsed
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		results = append(results, parseMILine(line, currentYear))
	}
	return results
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

		parsed := parseMatchImportText(text)
		if len(parsed) == 0 {
			showError("Nepodařilo se rozpoznat žádné zápasy. Zkontroluj formát (např.: Arsenal - Chelsea 15.5. 18:00).")
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

	// Ověř kolo a zjisti sport soutěže
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

		// Přiřaď týmy ke soutěži
		_, _ = db.Pool.Exec(ctx,
			`INSERT INTO competition_teams (competition_id, team_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			compID, homeID)
		_, _ = db.Pool.Exec(ctx,
			`INSERT INTO competition_teams (competition_id, team_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			compID, awayID)

		// Zkontroluj duplicitu
		var existingID int
		_ = db.Pool.QueryRow(ctx,
			`SELECT id FROM matches WHERE round_id=$1 AND home_team_id=$2 AND away_team_id=$3`,
			roundID, homeID, awayID).Scan(&existingID)
		if existingID > 0 {
			// Aktualizuj jen datum pokud je zadáno
			if m.ParsedDate != nil && *m.ParsedDate != "" {
				t, err := time.ParseInLocation("2006-01-02T15:04", *m.ParsedDate, pragueLocation)
				if err == nil {
					_, _ = db.Pool.Exec(ctx, `UPDATE matches SET match_date=$1 WHERE id=$2`, t, existingID)
				}
			}
			skipped++
			continue
		}

		// Vlož nový zápas (bez skóre)
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
