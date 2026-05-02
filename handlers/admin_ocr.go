// handlers/admin_ocr.go — Tipovačka 2.0
// Import výsledků vložením textu (paste z Flashscore nebo jiného zdroje).
//
// Endpointy:
//   GET  /admin/ocr          — formulář (textarea + výběr kola)
//   POST /admin/ocr/parse    — parsování, uložení do session, preview
//   POST /admin/ocr/confirm  — uložení zápasů do DB
//   POST /admin/ocr/cancel   — zrušení, clear session
package handlers

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"tipovacka/db"
	"tipovacka/middleware"
)

// scorePattern odpovídá řádkům ve tvaru "Tým A - Tým B  2:1" nebo "Tým A – Tým B 2 : 1"
var scorePattern = regexp.MustCompile(
	`([\pL\d .&'()/]+?)\s*[-–]\s*([\pL\d .&'()/]+?)\s+(\d+)\s*:\s*(\d+)`,
)

type ocrMatch struct {
	HomeTeam  string `json:"home_team"`
	AwayTeam  string `json:"away_team"`
	HomeScore int    `json:"home_score"`
	AwayScore int    `json:"away_score"`
	RawLine   string `json:"raw_line"`
}

// parseOCRText parsuje vložený text a vrátí nalezené zápasy.
func parseOCRText(text string) []ocrMatch {
	var results []ocrMatch
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := scorePattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		homeScore, _ := strconv.Atoi(m[3])
		awayScore, _ := strconv.Atoi(m[4])
		results = append(results, ocrMatch{
			HomeTeam:  strings.TrimSpace(m[1]),
			AwayTeam:  strings.TrimSpace(m[2]),
			HomeScore: homeScore,
			AwayScore: awayScore,
			RawLine:   line,
		})
	}
	return results
}

// ── session helpers ───────────────────────────────────────────────────────────

func ocrSetSession(w http.ResponseWriter, r *http.Request, matches []ocrMatch, roundID int, sport string) {
	sess := middleware.GetSession(r)
	b, _ := json.Marshal(matches)
	sess.Values["ocr_parsed"] = string(b)
	sess.Values["ocr_round_id"] = roundID
	sess.Values["ocr_sport"] = sport
	_ = sess.Save(r, w)
}

func ocrGetSession(r *http.Request) ([]ocrMatch, int, string) {
	sess := middleware.GetSession(r)
	var matches []ocrMatch
	if v, ok := sess.Values["ocr_parsed"].(string); ok {
		_ = json.Unmarshal([]byte(v), &matches)
	}
	roundID := 0
	if v, ok := sess.Values["ocr_round_id"].(int); ok {
		roundID = v
	}
	sport := "football"
	if v, ok := sess.Values["ocr_sport"].(string); ok && v != "" {
		sport = v
	}
	return matches, roundID, sport
}

func ocrClearSession(w http.ResponseWriter, r *http.Request) {
	sess := middleware.GetSession(r)
	delete(sess.Values, "ocr_parsed")
	delete(sess.Values, "ocr_round_id")
	delete(sess.Values, "ocr_sport")
	_ = sess.Save(r, w)
}

// ── GET /admin/ocr ────────────────────────────────────────────────────────────

type ocrRoundItem struct {
	ID          int
	Name        string
	CompName    string
	CompSeason  string
}

func AdminOCRForm(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		ctx := context.Background()

		// Načti kola seřazená po soutěžích — pouze aktivní soutěže
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

		RenderTemplate(w, r, tmpl, "admin/ocr_upload.html", TemplateData{
			"User":   admin,
			"Rounds": rounds,
			"Error":  nil,
		})
	}
}

// ── POST /admin/ocr/parse ─────────────────────────────────────────────────────
// Form: round_id, text

func AdminOCRParse(tmpl *template.Template) http.HandlerFunc {
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

		// Načti kola pro formulář (pro případ chyby) — pouze aktivní soutěže
		roundRows, _ := db.Pool.Query(ctx,
			`SELECT r.id, r.name, c.name, c.season
			   FROM rounds r
			   JOIN competitions c ON c.id = r.competition_id
			  WHERE COALESCE(c.is_active, false) = true
			  ORDER BY c.sort_order ASC NULLS LAST, c.id DESC, r.id DESC`)
		var rounds []ocrRoundItem
		for roundRows.Next() {
			var ri ocrRoundItem
			_ = roundRows.Scan(&ri.ID, &ri.Name, &ri.CompName, &ri.CompSeason)
			rounds = append(rounds, ri)
		}
		roundRows.Close()

		showError := func(msg string) {
			RenderTemplate(w, r, tmpl, "admin/ocr_upload.html", TemplateData{
				"User":   admin,
				"Rounds": rounds,
				"Error":  msg,
			})
		}

		roundID, _ := strconv.Atoi(r.FormValue("round_id"))

		// Sport se načte automaticky ze soutěže kola
		sport := "football"
		if roundID > 0 {
			_ = db.Pool.QueryRow(ctx,
				`SELECT COALESCE(c.sport, 'football')
				   FROM rounds r JOIN competitions c ON c.id = r.competition_id
				  WHERE r.id = $1`, roundID).Scan(&sport)
		}

		text := strings.TrimSpace(r.FormValue("text"))

		if roundID == 0 {
			showError("Vyber kolo.")
			return
		}
		if text == "" {
			showError("Vložte text s výsledky.")
			return
		}

		parsed := parseOCRText(text)
		if len(parsed) == 0 {
			showError("Nepodařilo se rozpoznat žádné výsledky. Zkontroluj formát textu (očekáváme: Tým A - Tým B  2:1).")
			return
		}

		ocrSetSession(w, r, parsed, roundID, sport)

		RenderTemplate(w, r, tmpl, "admin/ocr_preview.html", TemplateData{
			"User":    admin,
			"Parsed":  parsed,
			"RoundID": roundID,
		})
	}
}

// ── POST /admin/ocr/confirm ───────────────────────────────────────────────────

func AdminOCRConfirm(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}

	parsed, roundID, sport := ocrGetSession(r)
	ocrClearSession(w, r)

	if len(parsed) == 0 || roundID == 0 {
		http.Redirect(w, r, "/admin/ocr", http.StatusSeeOther)
		return
	}

	ctx := context.Background()

	// Ověř kolo
	var compID int
	if err := db.Pool.QueryRow(ctx,
		`SELECT competition_id FROM rounds WHERE id=$1`, roundID).Scan(&compID); err != nil {
		middleware.SetFlash(w, r, "error", "Kolo nenalezeno.")
		http.Redirect(w, r, "/admin/ocr", http.StatusSeeOther)
		return
	}

	created, skipped := 0, 0
	for _, m := range parsed {
		homeID, _ := upsertTeamByName(ctx, m.HomeTeam, sport)
		if homeID == 0 {
			skipped++
			continue
		}
		awayID, _ := upsertTeamByName(ctx, m.AwayTeam, sport)
		if awayID == 0 {
			skipped++
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
			// Aktualizuj skóre
			_, _ = db.Pool.Exec(ctx,
				`UPDATE matches SET home_score=$1, away_score=$2, is_finished=true WHERE id=$3`,
				m.HomeScore, m.AwayScore, existingID)
			RecalculateTips(ctx, existingID, m.HomeScore, m.AwayScore)
			skipped++
			continue
		}

		// Vlož nový zápas
		var newID int
		err := db.Pool.QueryRow(ctx,
			`INSERT INTO matches (round_id, home_team_id, away_team_id, home_score, away_score, is_finished)
			 VALUES ($1,$2,$3,$4,$5,true) RETURNING id`,
			roundID, homeID, awayID, m.HomeScore, m.AwayScore).Scan(&newID)
		if err != nil {
			skipped++
			continue
		}
		RecalculateTips(ctx, newID, m.HomeScore, m.AwayScore)
		created++
	}

	RecalculateStandings(compID)

	msg := "Import dokončen: <b>" + strconv.Itoa(created) + "</b> nových zápasů"
	if skipped > 0 {
		msg += ", " + strconv.Itoa(skipped) + " aktualizováno/přeskočeno"
	}
	msg += "."
	middleware.SetFlash(w, r, "ok", msg)
	http.Redirect(w, r, "/admin/rounds/"+strconv.Itoa(roundID)+"/matches", http.StatusSeeOther)
}

// ── POST /admin/ocr/cancel ────────────────────────────────────────────────────

func AdminOCRCancel(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	ocrClearSession(w, r)
	http.Redirect(w, r, "/admin/ocr", http.StatusSeeOther)
}

// ── upsertTeamByName ─────────────────────────────────────────────────────────
// Najde nebo vytvoří tým podle jména + sport.

func upsertTeamByName(ctx context.Context, name, sport string) (int, bool) {
	if name == "" {
		return 0, false
	}
	var id int
	err := db.Pool.QueryRow(ctx,
		`SELECT id FROM teams WHERE name=$1 AND sport=$2`, name, sport).Scan(&id)
	if err == nil {
		return id, false
	}
	var newID int
	if err := db.Pool.QueryRow(ctx,
		`INSERT INTO teams (name, sport) VALUES ($1,$2) RETURNING id`,
		name, sport).Scan(&newID); err != nil {
		return 0, false
	}
	return newID, true
}

