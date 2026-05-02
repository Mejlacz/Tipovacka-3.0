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

// matchImportLinePattern odpovídá řádkům ve tvaru:
//
//	"Tým A - Tým B  15.5. 18:00"
//	"Tým A – Tým B  15.5.2026 18:00"
//	"Tým A - Tým B  15.5.2026"          (bez času)
var matchImportLinePattern = regexp.MustCompile(
	`^([\pL\d .&'()/]+?)\s*[-–]\s*([\pL\d .&'()/]+?)\s+` +
		`(\d{1,2})\.(\d{1,2})\.(\d{4})?\s*` +
		`(?:(\d{1,2})[.:](\d{2}))?$`,
)

type importMatchParsed struct {
	HomeTeam   string  `json:"home_team"`
	AwayTeam   string  `json:"away_team"`
	DateStr    string  `json:"date_str"`    // "15.05.2026 18:00"
	ParsedDate *string `json:"parsed_date"` // "2026-05-15T18:00" pro vložení
	HasTime    bool    `json:"has_time"`
	RawLine    string  `json:"raw_line"`
	ParseError string  `json:"parse_error,omitempty"`
}

// parseMatchImportText parsuje vložený text a vrátí nalezené budoucí zápasy.
func parseMatchImportText(text string) []importMatchParsed {
	now := time.Now().In(pragueLocation)
	currentYear := now.Year()

	var results []importMatchParsed
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := matchImportLinePattern.FindStringSubmatch(line)
		if m == nil {
			results = append(results, importMatchParsed{
				RawLine:    line,
				ParseError: "nečitelný formát — očekáváno: Domácí - Hosté DD.MM. HH:MM",
			})
			continue
		}

		home := strings.TrimSpace(m[1])
		away := strings.TrimSpace(m[2])
		day, _ := strconv.Atoi(m[3])
		month, _ := strconv.Atoi(m[4])
		year := currentYear
		if m[5] != "" {
			year, _ = strconv.Atoi(m[5])
			if year < 100 {
				year += 2000
			}
		}

		hasTime := m[6] != ""
		hour, min := 0, 0
		if hasTime {
			hour, _ = strconv.Atoi(m[6])
			min, _ = strconv.Atoi(m[7])
		}

		// Validace data
		if month < 1 || month > 12 || day < 1 || day > 31 {
			results = append(results, importMatchParsed{
				RawLine:    line,
				ParseError: fmt.Sprintf("neplatné datum %02d.%02d.", day, month),
			})
			continue
		}

		dateStr := fmt.Sprintf("%02d.%02d.%04d", day, month, year)
		isoStr := fmt.Sprintf("%04d-%02d-%02dT%02d:%02d", year, month, day, hour, min)
		if hasTime {
			dateStr += fmt.Sprintf(" %02d:%02d", hour, min)
		}

		results = append(results, importMatchParsed{
			HomeTeam:   home,
			AwayTeam:   away,
			DateStr:    dateStr,
			ParsedDate: &isoStr,
			HasTime:    hasTime,
			RawLine:    line,
		})
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
