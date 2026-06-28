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
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/xuri/excelize/v2"

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

// ── čtení nahraného souboru (XLSX / CSV) ──────────────────────────────────────

// miTimeSecRe: HH:MM:SS → pro normalizaci na HH:MM (Excel buňky času často mají sekundy)
var miTimeSecRe = regexp.MustCompile(`^(\d{1,2}):(\d{2}):\d{2}$`)

// readImportFileRows načte řádky z nahraného XLSX nebo CSV souboru.
// XLSX: první list. CSV: autodetekce oddělovače (',' nebo ';').
func readImportFileRows(file io.Reader, filename string) ([][]string, error) {
	lower := strings.ToLower(strings.TrimSpace(filename))
	switch {
	case strings.HasSuffix(lower, ".xlsx"):
		f, err := excelize.OpenReader(file)
		if err != nil {
			return nil, fmt.Errorf("nelze otevřít XLSX soubor: %w", err)
		}
		defer f.Close()
		sheets := f.GetSheetList()
		if len(sheets) == 0 {
			return nil, fmt.Errorf("soubor neobsahuje žádný list")
		}
		// Vyber list s nejvíc tabulkovými řádky (≥2 neprázdné buňky) —
		// soubor může mít víc listů a ten datový nemusí být první.
		best, bestScore := sheets[0], -1
		for _, s := range sheets {
			rr, _ := f.GetRows(s)
			score := 0
			for _, row := range rr {
				nonEmpty := 0
				for _, c := range row {
					if strings.TrimSpace(c) != "" {
						nonEmpty++
					}
				}
				if nonEmpty >= 2 {
					score++
				}
			}
			if score > bestScore {
				best, bestScore = s, score
			}
		}
		rows, err := f.GetRows(best)
		if err != nil {
			return nil, err
		}
		// Datum bývá uložené jako Excel "serial" číslo a zobrazí se podle
		// locale (např. americky 06-30-26) → nejednoznačné. Přepiš takové
		// buňky z RAW hodnoty na jednoznačné DD.MM.YYYY. Časy (03:00) a
		// názvy týmů zůstávají z formátovaného výstupu beze změny.
		rawRows, _ := f.GetRows(best, excelize.Options{RawCellValue: true})
		for i := range rows {
			for j := range rows[i] {
				if i >= len(rawRows) || j >= len(rawRows[i]) {
					continue
				}
				v, perr := strconv.ParseFloat(strings.TrimSpace(rawRows[i][j]), 64)
				if perr != nil || v < 20000 || v >= 90000 {
					continue // není to datumový serial
				}
				tm, terr := excelize.ExcelDateToTime(v, false)
				if terr != nil {
					continue
				}
				if v != math.Floor(v) {
					rows[i][j] = tm.Format("02.01.2006 15:04")
				} else {
					rows[i][j] = tm.Format("02.01.2006")
				}
			}
		}
		return rows, nil
	case strings.HasSuffix(lower, ".csv"):
		data, err := io.ReadAll(file)
		if err != nil {
			return nil, err
		}
		// Detekce oddělovače — český Excel exportuje CSV se středníkem
		delim := ','
		if bytes.Count(data, []byte(";")) > bytes.Count(data, []byte(",")) {
			delim = ';'
		}
		rd := csv.NewReader(bytes.NewReader(data))
		rd.Comma = delim
		rd.FieldsPerRecord = -1
		rd.LazyQuotes = true
		return rd.ReadAll()
	default:
		return nil, fmt.Errorf("nepodporovaný formát — použij .xlsx nebo .csv")
	}
}

// xlsxRowsToText převede řádky tabulky na text pro řádkový parser.
// Buňky jednoho řádku spojí tabulátorem, vynechá prázdné a hlavičkové řádky
// (řádek bez jediné číslice = hlavička jako "Datum / Domácí / Hosté / Čas").
func xlsxRowsToText(rows [][]string) string {
	var sb strings.Builder
	for _, row := range rows {
		var cells []string
		hasDigit := false
		for _, c := range row {
			c = strings.TrimSpace(c)
			if c == "" {
				continue
			}
			// Normalizuj čas HH:MM:SS → HH:MM
			if m := miTimeSecRe.FindStringSubmatch(c); m != nil {
				c = m[1] + ":" + m[2]
			}
			if !hasDigit && strings.ContainsAny(c, "0123456789") {
				hasDigit = true
			}
			cells = append(cells, c)
		}
		if len(cells) == 0 || !hasDigit {
			continue // prázdný nebo hlavičkový řádek
		}
		sb.WriteString(strings.Join(cells, "\t"))
		sb.WriteByte('\n')
	}
	return sb.String()
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

// ── loadCompTeams: týmy soutěže ──────────────────────────────────────────────

func loadCompTeams(ctx context.Context, compID int) []string {
	rows, _ := db.Pool.Query(ctx,
		`SELECT COALESCE(t.display_name, t.name)
		   FROM teams t
		   JOIN competition_teams ct ON ct.team_id = t.id
		  WHERE ct.competition_id = $1
		  ORDER BY t.name`, compID)
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

func miSetSession(w http.ResponseWriter, r *http.Request, matches []importMatchParsed, compID int) {
	sess := middleware.GetSession(r)
	b, _ := json.Marshal(matches)
	sess.Values["mi_parsed"]  = string(b)
	sess.Values["mi_comp_id"] = compID
	_ = sess.Save(r, w)
}

func miGetSession(r *http.Request) ([]importMatchParsed, int) {
	sess := middleware.GetSession(r)
	var matches []importMatchParsed
	if v, ok := sess.Values["mi_parsed"].(string); ok {
		_ = json.Unmarshal([]byte(v), &matches)
	}
	compID := 0
	if v, ok := sess.Values["mi_comp_id"].(int); ok {
		compID = v
	}
	return matches, compID
}

func miClearSession(w http.ResponseWriter, r *http.Request) {
	sess := middleware.GetSession(r)
	delete(sess.Values, "mi_parsed")
	delete(sess.Values, "mi_comp_id")
	_ = sess.Save(r, w)
}

// ── loadActiveComps ─ sdílený helper pro dropdown ────────────────────────────

func loadActiveComps(ctx context.Context) []ocrCompItem {
	rows, _ := db.Pool.Query(ctx,
		`SELECT id, name, season, COALESCE(sport,'football')
		   FROM competitions
		  WHERE COALESCE(is_active, false) = true
		  ORDER BY COALESCE(sort_order,9999) ASC, id DESC`)
	var comps []ocrCompItem
	for rows.Next() {
		var ci ocrCompItem
		_ = rows.Scan(&ci.ID, &ci.Name, &ci.Season, &ci.Sport)
		comps = append(comps, ci)
	}
	rows.Close()
	return comps
}

// Deprecated: kept for any remaining callers — wraps loadActiveComps
func loadActiveRounds(ctx context.Context) []ocrCompItem {
	return loadActiveComps(ctx)
}

// ── GET /admin/matches/import/template ─ stažení vzorového XLSX ───────────────

func AdminMatchImportTemplate(w http.ResponseWriter, r *http.Request) {
	if admin := RequireAdmin(w, r); admin == nil {
		return
	}

	f := excelize.NewFile()
	defer f.Close()
	sheet := f.GetSheetName(0)

	headers := []string{"Datum", "Domácí", "Hosté", "Čas"}
	for i, h := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		_ = f.SetCellValue(sheet, cell, h)
	}
	// Ukázkové řádky — admin je přepíše svými zápasy
	examples := [][]string{
		{"15.5.2026", "Arsenal", "Chelsea", "18:00"},
		{"16.5.2026", "Baník Ostrava", "Sigma Olomouc", "15:30"},
	}
	for ri, row := range examples {
		for ci, val := range row {
			cell, _ := excelize.CoordinatesToCellName(ci+1, ri+2)
			_ = f.SetCellValue(sheet, cell, val)
		}
	}
	// Tučná hlavička + šířky sloupců
	if style, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}}); err == nil {
		_ = f.SetCellStyle(sheet, "A1", "D1", style)
	}
	_ = f.SetColWidth(sheet, "A", "A", 14)
	_ = f.SetColWidth(sheet, "B", "C", 22)
	_ = f.SetColWidth(sheet, "D", "D", 10)

	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="vzor_import_zapasu.xlsx"`)
	_ = f.Write(w)
}

// ── GET /admin/matches/import ─────────────────────────────────────────────────

func AdminMatchImportForm(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		RenderTemplate(w, r, tmpl, "admin/match_import.html", TemplateData{
			"User":  admin,
			"Comps": loadActiveComps(context.Background()),
			"Error": nil,
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

		ctx := context.Background()
		comps := loadActiveComps(ctx)

		showError := func(msg string) {
			RenderTemplate(w, r, tmpl, "admin/match_import.html", TemplateData{
				"User":  admin,
				"Comps": comps,
				"Error": msg,
			})
		}

		// Podpora textu (paste) i nahrání souboru (multipart)
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			if err := r.ParseMultipartForm(16 << 20); err != nil {
				showError("Nelze načíst formulář: " + err.Error())
				return
			}
		} else if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}

		compID, _ := strconv.Atoi(r.FormValue("competition_id"))
		text := strings.TrimSpace(r.FormValue("text"))

		// Pokud byl nahrán soubor (.xlsx/.csv), má přednost před textem
		if file, header, ferr := r.FormFile("import_file"); ferr == nil {
			defer file.Close()
			fileRows, rerr := readImportFileRows(file, header.Filename)
			if rerr != nil {
				showError("Chyba souboru: " + rerr.Error())
				return
			}
			text = strings.TrimSpace(xlsxRowsToText(fileRows))
			if text == "" {
				showError("V souboru se nenašly žádné zápasy. Zkontroluj, že obsahuje sloupce s datem, časem a názvy týmů.")
				return
			}
		}

		if compID == 0 {
			showError("Vyber soutěž.")
			return
		}
		if text == "" {
			showError("Vlož zápasy textem nebo nahraj soubor (.xlsx / .csv).")
			return
		}

		// Ověř soutěž
		var compName string
		err := db.Pool.QueryRow(ctx,
			`SELECT name FROM competitions WHERE id = $1`, compID).Scan(&compName)
		if err != nil {
			showError("Soutěž nenalezena.")
			return
		}

		// Načti týmy soutěže pro chytré rozpoznávání
		knownTeams := loadCompTeams(ctx, compID)

		parsed := parseMatchImportText(text, knownTeams)
		if len(parsed) == 0 {
			showError("Nepodařilo se rozpoznat žádné zápasy.")
			return
		}

		// Pozn.: data NEukládáme do session (cookie store má limit ~4 KB a
		// velký rozpis by přetekl → confirm by nedostal nic). Místo toho
		// pošleme původní text + compID skrytými poli a na confirm znovu
		// naparsujeme.
		RenderTemplate(w, r, tmpl, "admin/match_import_preview.html", TemplateData{
			"User":     admin,
			"Parsed":   parsed,
			"CompID":   compID,
			"CompName": compName,
			"Text":     text,
		})
	}
}

// ── POST /admin/matches/import/confirm ────────────────────────────────────────

func AdminMatchImportConfirm(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/admin/matches/import", http.StatusSeeOther)
		return
	}
	compID, _ := strconv.Atoi(r.FormValue("competition_id"))
	text := strings.TrimSpace(r.FormValue("text"))

	if text == "" || compID == 0 {
		LogAction(&admin.ID, admin.Username, "match_import", "competition", nil,
			"Import zápasů NEPROVEDEN — chybí data (prázdný text nebo nevybraná soutěž)", nil, nil)
		middleware.SetFlash(w, r, "error", "Chybí data k importu — zkus to prosím znovu.")
		http.Redirect(w, r, "/admin/matches/import", http.StatusSeeOther)
		return
	}

	ctx := context.Background()

	var compName string
	_ = db.Pool.QueryRow(ctx, `SELECT name FROM competitions WHERE id=$1`, compID).Scan(&compName)

	// Znovu naparsuj (stejně jako v náhledu) — bez round-tripu přes cookie
	knownTeams := loadCompTeams(ctx, compID)
	parsed := parseMatchImportText(text, knownTeams)
	if len(parsed) == 0 {
		LogAction(&admin.ID, admin.Username, "match_import", "competition", &compID,
			"Import zápasů NEPROVEDEN — nerozpoznán žádný zápas (soutěž "+compName+")", nil, nil)
		middleware.SetFlash(w, r, "error", "Nepodařilo se rozpoznat žádné zápasy.")
		http.Redirect(w, r, "/admin/matches/import", http.StatusSeeOther)
		return
	}

	sport := "football"
	_ = db.Pool.QueryRow(ctx,
		`SELECT COALESCE(sport,'football') FROM competitions WHERE id=$1`, compID).Scan(&sport)

	created, skipped, errCount := 0, 0, 0
	for _, m := range parsed {
		if m.ParseError != "" {
			errCount++
			LogAction(&admin.ID, admin.Username, "match_import", "competition", &compID,
				"Import zápasu PŘESKOČEN (chyba parsování): '"+m.RawLine+"' — "+m.ParseError, nil, nil)
			continue
		}

		homeID, _ := upsertTeamByName(ctx, m.HomeTeam, sport)
		if homeID == 0 {
			errCount++
			LogAction(&admin.ID, admin.Username, "match_import", "competition", &compID,
				"Import zápasu CHYBA — nepodařilo se vytvořit/najít domácí tým '"+m.HomeTeam+"'", nil, nil)
			continue
		}
		awayID, _ := upsertTeamByName(ctx, m.AwayTeam, sport)
		if awayID == 0 {
			errCount++
			LogAction(&admin.ID, admin.Username, "match_import", "competition", &compID,
				"Import zápasu CHYBA — nepodařilo se vytvořit/najít hostující tým '"+m.AwayTeam+"'", nil, nil)
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
			`SELECT id FROM matches WHERE competition_id=$1 AND home_team_id=$2 AND away_team_id=$3`,
			compID, homeID, awayID).Scan(&existingID)
		if existingID > 0 {
			if m.ParsedDate != nil && *m.ParsedDate != "" {
				t, err := time.ParseInLocation("2006-01-02T15:04", *m.ParsedDate, pragueLocation)
				if err == nil {
					_, _ = db.Pool.Exec(ctx, `UPDATE matches SET match_date=$1 WHERE id=$2`, t, existingID)
				}
			}
			skipped++
			LogAction(&admin.ID, admin.Username, "match_import", "match", &existingID,
				"Import zápasu PŘESKOČEN (duplicita): "+m.HomeTeam+" vs "+m.AwayTeam+" ("+m.DateStr+")", nil, nil)
			continue
		}

		var matchDate *time.Time
		if m.ParsedDate != nil && *m.ParsedDate != "" {
			t, err := time.ParseInLocation("2006-01-02T15:04", *m.ParsedDate, pragueLocation)
			if err == nil {
				matchDate = &t
			}
		}
		var newID int
		err := db.Pool.QueryRow(ctx,
			`INSERT INTO matches (competition_id, home_team_id, away_team_id, match_date, is_finished)
			 VALUES ($1,$2,$3,$4,false) RETURNING id`,
			compID, homeID, awayID, matchDate).Scan(&newID)
		if err != nil {
			errCount++
			LogAction(&admin.ID, admin.Username, "match_import", "competition", &compID,
				"Import zápasu CHYBA při zápisu do DB: "+m.HomeTeam+" vs "+m.AwayTeam+" ("+m.DateStr+") — "+err.Error(), nil, nil)
			continue
		}
		created++
		newVal := fmt.Sprintf(`{"competition_id":%d,"home_team_id":%d,"away_team_id":%d,"date":%q}`,
			compID, homeID, awayID, m.DateStr)
		LogAction(&admin.ID, admin.Username, "match_import", "match", &newID,
			"Import zápasu: "+m.HomeTeam+" vs "+m.AwayTeam+" ("+m.DateStr+", soutěž "+compName+")", nil, &newVal)
	}

	// Souhrnný záznam celého importu
	summary := fmt.Sprintf("Import zápasů do soutěže %s: %d vytvořeno, %d přeskočeno (duplicity), %d chyb (z %d řádků)",
		compName, created, skipped, errCount, len(parsed))
	LogAction(&admin.ID, admin.Username, "match_import", "competition", &compID, summary, nil, nil)

	msg := fmt.Sprintf("Import dokončen: <b>%d</b> nových zápasů", created)
	if skipped > 0 {
		msg += fmt.Sprintf(", %d přeskočeno (duplicity)", skipped)
	}
	if errCount > 0 {
		msg += fmt.Sprintf(", %d chyb", errCount)
	}
	msg += "."
	middleware.SetFlash(w, r, "ok", msg)
	http.Redirect(w, r, "/admin/competitions/"+strconv.Itoa(compID)+"/matches", http.StatusSeeOther)
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
