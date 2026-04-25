// handlers/admin_teams.go — Tipovačka 2.0
// Správa týmů.
package handlers

import (
	"context"
	"encoding/csv"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
	"tipovacka/db"
	"tipovacka/middleware"
	"tipovacka/models"
)

// GET /admin/teams
func AdminTeamsList(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		ctx := context.Background()
		rows, _ := db.Pool.Query(ctx,
			`SELECT id, name, sport, alias, display_name, logo_url, category, competition_id FROM teams ORDER BY name`)
		var teams []*models.Team
		for rows.Next() {
			t := &models.Team{}
			_ = rows.Scan(&t.ID, &t.Name, &t.Sport, &t.Alias, &t.DisplayName, &t.LogoURL, &t.Category, &t.CompetitionID)
			teams = append(teams, t)
		}
		rows.Close()

		compRows, _ := db.Pool.Query(ctx, `SELECT id, name, season FROM competitions ORDER BY id DESC`)
		var comps []*models.Competition
		for compRows.Next() {
			c := &models.Competition{}
			_ = compRows.Scan(&c.ID, &c.Name, &c.Season)
			comps = append(comps, c)
		}
		compRows.Close()

		RenderTemplate(w, r, tmpl, "teams.html", TemplateData{
			"User":   admin,
			"Teams":  teams,
			"Comps":  comps,
			"Msg":    r.URL.Query().Get("msg"),
			"Error":  r.URL.Query().Get("error"),
			"Flash":  middleware.GetFlash(w, r),
		})
	}
}

// POST /admin/teams/new
func AdminTeamCreate(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	sport := r.FormValue("sport")
	if sport == "" {
		sport = "football"
	}
	displayName := strings.TrimSpace(r.FormValue("display_name"))
	category := strings.TrimSpace(r.FormValue("category"))

	_, _ = db.Pool.Exec(context.Background(),
		`INSERT INTO teams (name, sport, display_name, category) VALUES ($1,$2,$3,$4)
		 ON CONFLICT (name, sport) DO NOTHING`,
		name, sport, PtrStr(displayName), PtrStr(category))
	middleware.SetFlash(w, r, "ok", "Tým <b>"+name+"</b> byl přidán.")
	http.Redirect(w, r, "/admin/teams", http.StatusSeeOther)
}

// POST /admin/teams/{id}/edit
func AdminTeamEdit(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	teamID, _ := strconv.Atoi(r.PathValue("team_id"))
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	displayName := strings.TrimSpace(r.FormValue("display_name"))
	category := strings.TrimSpace(r.FormValue("category"))
	alias := strings.TrimSpace(r.FormValue("alias"))

	_, _ = db.Pool.Exec(context.Background(),
		`UPDATE teams SET name=$1, display_name=$2, category=$3, alias=$4 WHERE id=$5`,
		name, PtrStr(displayName), PtrStr(category), PtrStr(alias), teamID)
	http.Redirect(w, r, "/admin/teams", http.StatusSeeOther)
}

// POST /admin/teams/{id}/delete
func AdminTeamDelete(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	teamID, _ := strconv.Atoi(r.PathValue("team_id"))
	ctx := context.Background()

	// Zkontroluj zda tým nemá zápasy
	var count int
	_ = db.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM matches WHERE home_team_id=$1 OR away_team_id=$1`, teamID).Scan(&count)
	if count > 0 {
		middleware.SetFlash(w, r, "error", "Tým nelze smazat — má přiřazené zápasy.")
		http.Redirect(w, r, "/admin/teams", http.StatusSeeOther)
		return
	}

	_, _ = db.Pool.Exec(ctx, `DELETE FROM competition_teams WHERE team_id=$1`, teamID)
	_, _ = db.Pool.Exec(ctx, `DELETE FROM teams WHERE id=$1`, teamID)
	http.Redirect(w, r, "/admin/teams", http.StatusSeeOther)
}

// POST /admin/teams/{team_id}/merge
// Sloučí zdrojový tým (source) do cílového (target):
//   - přepíše home_team_id / away_team_id ve všech zápasech
//   - přenese competition_teams záznamy
//   - smaže zdrojový tým
//
// Ochrana: uživatel musí opsat přesný název cílového týmu do pole confirm_name.
func AdminTeamMerge(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	sourceID, _ := strconv.Atoi(r.PathValue("team_id"))
	targetID, _ := strconv.Atoi(r.FormValue("target_team_id"))
	confirmName := strings.TrimSpace(r.FormValue("confirm_name"))
	ctx := context.Background()

	flash := func(typ, msg string) {
		middleware.SetFlash(w, r, typ, msg)
		http.Redirect(w, r, "/admin/teams", http.StatusSeeOther)
	}

	if sourceID == 0 || targetID == 0 || sourceID == targetID {
		flash("error", "Neplatné ID týmů.")
		return
	}

	// Načti oba týmy
	var sourceName, targetName string
	if err := db.Pool.QueryRow(ctx, `SELECT name FROM teams WHERE id=$1`, sourceID).Scan(&sourceName); err != nil {
		flash("error", "Zdrojový tým nenalezen.")
		return
	}
	if err := db.Pool.QueryRow(ctx, `SELECT name FROM teams WHERE id=$1`, targetID).Scan(&targetName); err != nil {
		flash("error", "Cílový tým nenalezen.")
		return
	}

	// Bezpečnostní kontrola: opis názvu cílového týmu
	if !strings.EqualFold(confirmName, targetName) {
		flash("error", fmt.Sprintf("Název nesouhlasí — očekáváno „%s”.", targetName))
		return
	}

	// 1. Přepiš zápasy
	_, _ = db.Pool.Exec(ctx, `UPDATE matches SET home_team_id=$1 WHERE home_team_id=$2`, targetID, sourceID)
	_, _ = db.Pool.Exec(ctx, `UPDATE matches SET away_team_id=$1 WHERE away_team_id=$2`, targetID, sourceID)

	// 2. Přenes competition_teams (přeskoč konflikty — target tam už může být)
	_, _ = db.Pool.Exec(ctx, `
		INSERT INTO competition_teams (competition_id, team_id)
		SELECT competition_id, $1 FROM competition_teams WHERE team_id=$2
		ON CONFLICT DO NOTHING`, targetID, sourceID)

	// 3. Smaž zdroj
	_, _ = db.Pool.Exec(ctx, `DELETE FROM competition_teams WHERE team_id=$1`, sourceID)
	_, _ = db.Pool.Exec(ctx, `DELETE FROM teams WHERE id=$1`, sourceID)

	desc := fmt.Sprintf("Tým „%s” (ID %d) sloučen do „%s” (ID %d)", sourceName, sourceID, targetName, targetID)
	LogAction(&admin.ID, admin.Username, "team_merge", "team", &sourceID, desc, nil, nil)

	flash("ok", desc)
}

// TeamGroup — skupína týmů podle category pro šablonu competition_teams.html
type TeamGroup struct {
	Category string // "" → "Bez kategorie"
	Teams    []*models.Team
}

// GET /admin/competitions/{id}/teams
func AdminCompetitionTeamsForm(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		compID, _ := strconv.Atoi(r.PathValue("competition_id"))
		ctx := context.Background()

		comp := &models.Competition{}
		err := db.Pool.QueryRow(ctx,
			`SELECT id, name, season, is_active, sport, sort_order FROM competitions WHERE id=$1`, compID).
			Scan(&comp.ID, &comp.Name, &comp.Season, &comp.IsActive, &comp.Sport, &comp.SortOrder)
		if err != nil {
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}

		allTeamRows, _ := db.Pool.Query(ctx,
			`SELECT id, name, sport, alias, display_name, logo_url, category, competition_id
			   FROM teams ORDER BY LOWER(name)`)
		var allTeams []*models.Team
		for allTeamRows.Next() {
			t := &models.Team{}
			_ = allTeamRows.Scan(&t.ID, &t.Name, &t.Sport, &t.Alias, &t.DisplayName, &t.LogoURL, &t.Category, &t.CompetitionID)
			allTeams = append(allTeams, t)
		}
		allTeamRows.Close()

		// Vybrané týmy
		selectedIDs := map[int]bool{}
		selRows, _ := db.Pool.Query(ctx,
			`SELECT team_id FROM competition_teams WHERE competition_id=$1`, compID)
		for selRows.Next() {
			var tid int
			_ = selRows.Scan(&tid)
			selectedIDs[tid] = true
		}
		selRows.Close()

		// Mapa teamID → názvy soutěží kde je tým (kromě aktuální)
		teamCompNames := map[int][]string{}
		compNameRows, _ := db.Pool.Query(ctx, `
			SELECT ct.team_id, c.name, c.season
			FROM competition_teams ct
			JOIN competitions c ON c.id = ct.competition_id
			WHERE ct.competition_id != $1
			ORDER BY c.id DESC`, compID)
		for compNameRows.Next() {
			var tid int
			var cname, cseason string
			if err := compNameRows.Scan(&tid, &cname, &cseason); err == nil {
				label := cname
				if cseason != "" {
					label += " " + cseason
				}
				teamCompNames[tid] = append(teamCompNames[tid], label)
			}
		}
		compNameRows.Close()

		// Skupiny podle category (zachovává pořadí první výskytu)
		groupOrder := []string{}
		groupMap := map[string][]*models.Team{}
		for _, t := range allTeams {
			cat := ""
			if t.Category != nil {
				cat = *t.Category
			}
			if _, ok := groupMap[cat]; !ok {
				groupOrder = append(groupOrder, cat)
			}
			groupMap[cat] = append(groupMap[cat], t)
		}
		var groups []TeamGroup
		// Nejdřív pojmenované kategorie, pak "" (bez kategorie)
		for _, cat := range groupOrder {
			if cat != "" {
				groups = append(groups, TeamGroup{Category: cat, Teams: groupMap[cat]})
			}
		}
		if _, ok := groupMap[""]; ok {
			groups = append(groups, TeamGroup{Category: "", Teams: groupMap[""]})
		}

		RenderTemplate(w, r, tmpl, "competition_teams.html", TemplateData{
			"User":          admin,
			"Comp":          comp,
			"Groups":        groups,
			"AllTeams":      allTeams,
			"SelectedIDs":   selectedIDs,
			"TeamCompNames": teamCompNames,
			"Flash":         middleware.GetFlash(w, r),
		})
	}
}

// POST /admin/competitions/{id}/teams
func AdminCompetitionTeamsSave(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	compID, _ := strconv.Atoi(r.PathValue("competition_id"))
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	selectedIDs := map[int]bool{}
	for _, v := range r.Form["team_ids"] {
		id, _ := strconv.Atoi(v)
		if id > 0 {
			selectedIDs[id] = true
		}
	}

	ctx := context.Background()
	_, _ = db.Pool.Exec(ctx, `DELETE FROM competition_teams WHERE competition_id=$1`, compID)
	for teamID := range selectedIDs {
		_, _ = db.Pool.Exec(ctx,
			`INSERT INTO competition_teams (competition_id, team_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			compID, teamID)
	}
	http.Redirect(w, r, "/admin/competitions/"+strconv.Itoa(compID)+"/teams?msg=Roster+ulo%C5%BEen", http.StatusSeeOther)
}

// POST /admin/teams/import-csv
// Očekávaný formát CSV (s nebo bez hlavičky):
//   name, sport, display_name, alias, category
// Pokud sloupec sport chybí, použije se "football".
// Existující týmy (stejné name+sport) se aktualizují (display_name, alias, category).
func AdminTeamsImportCSV(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		middleware.SetFlash(w, r, "error", "Nelze načíst soubor: "+err.Error())
		http.Redirect(w, r, "/admin/io", http.StatusSeeOther)
		return
	}
	file, _, err := r.FormFile("csv_file")
	if err != nil {
		middleware.SetFlash(w, r, "error", "Soubor není přiložen.")
		http.Redirect(w, r, "/admin/io", http.StatusSeeOther)
		return
	}
	defer file.Close()

	// Přeskočit BOM pokud existuje
	bom := make([]byte, 3)
	n, _ := file.Read(bom)
	var reader io.Reader
	if n == 3 && bom[0] == 0xEF && bom[1] == 0xBB && bom[2] == 0xBF {
		reader = file // BOM přečten, pokračuj se zbytkem
	} else {
		reader = io.MultiReader(strings.NewReader(string(bom[:n])), file)
	}

	cr := csv.NewReader(reader)
	cr.TrimLeadingSpace = true
	cr.FieldsPerRecord = -1 // variabilní počet sloupců

	rows, err := cr.ReadAll()
	if err != nil {
		middleware.SetFlash(w, r, "error", "Chyba čtení CSV: "+err.Error())
		http.Redirect(w, r, "/admin/io", http.StatusSeeOther)
		return
	}
	if len(rows) == 0 {
		middleware.SetFlash(w, r, "error", "CSV je prázdné.")
		http.Redirect(w, r, "/admin/io", http.StatusSeeOther)
		return
	}

	// Detekuj hlavičku — pokud první řádek má "name" nebo "Name"
	startRow := 0
	if len(rows[0]) > 0 && strings.EqualFold(strings.TrimSpace(rows[0][0]), "name") {
		startRow = 1
	}

	ctx := context.Background()
	created, updated, skipped := 0, 0, 0

	for i := startRow; i < len(rows); i++ {
		row := rows[i]
		if len(row) == 0 {
			continue
		}
		col := func(idx int) string {
			if idx < len(row) {
				return strings.TrimSpace(row[idx])
			}
			return ""
		}

		name := col(0)
		if name == "" {
			skipped++
			continue
		}
		sport := col(1)
		if sport == "" {
			sport = "football"
		}
		displayName := PtrStr(col(2))
		alias := PtrStr(col(3))
		category := PtrStr(col(4))

		// Zkus UPDATE nejprve (tým se stejným name+sport)
		tag, _ := db.Pool.Exec(ctx,
			`UPDATE teams SET display_name=$1, alias=$2, category=$3
			  WHERE name=$4 AND sport=$5`,
			displayName, alias, category, name, sport)
		if tag.RowsAffected() > 0 {
			updated++
			continue
		}
		// Jinak INSERT
		_, err := db.Pool.Exec(ctx,
			`INSERT INTO teams (name, sport, display_name, alias, category)
			 VALUES ($1,$2,$3,$4,$5)`,
			name, sport, displayName, alias, category)
		if err != nil {
			skipped++
		} else {
			created++
		}
	}

	msg := fmt.Sprintf("Import dokončen: %d nových, %d aktualizovaných, %d přeskočených.", created, updated, skipped)
	middleware.SetFlash(w, r, "ok", msg)
	http.Redirect(w, r, "/admin/teams", http.StatusSeeOther)
}

// POST /admin/teams/import-xlsx
// Importuje týmy z XLSX souboru (stejná struktura jako CSV):
//   sloupec A: name, B: sport, C: display_name, D: alias, E: category
// První řádek se automaticky přeskočí pokud obsahuje záhlaví.
func AdminTeamsImportXLSX(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		middleware.SetFlash(w, r, "error", "Nelze načíst soubor: "+err.Error())
		http.Redirect(w, r, "/admin/io#import", http.StatusSeeOther)
		return
	}
	file, _, err := r.FormFile("xlsx_file")
	if err != nil {
		middleware.SetFlash(w, r, "error", "Soubor není přiložen.")
		http.Redirect(w, r, "/admin/io#import", http.StatusSeeOther)
		return
	}
	defer file.Close()

	f, err := excelize.OpenReader(file)
	if err != nil {
		middleware.SetFlash(w, r, "error", "Nelze otevřít XLSX soubor: "+err.Error())
		http.Redirect(w, r, "/admin/io#import", http.StatusSeeOther)
		return
	}
	defer f.Close()

	// Použij první sheet
	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		middleware.SetFlash(w, r, "error", "XLSX soubor neobsahuje žádný list.")
		http.Redirect(w, r, "/admin/io#import", http.StatusSeeOther)
		return
	}
	rows, err := f.GetRows(sheets[0])
	if err != nil {
		middleware.SetFlash(w, r, "error", "Chyba čtení XLSX: "+err.Error())
		http.Redirect(w, r, "/admin/io#import", http.StatusSeeOther)
		return
	}
	if len(rows) == 0 {
		middleware.SetFlash(w, r, "error", "XLSX soubor je prázdný.")
		http.Redirect(w, r, "/admin/io#import", http.StatusSeeOther)
		return
	}

	// Detekuj záhlaví — pokud první buňka je "name" nebo "Name"
	startRow := 0
	if len(rows[0]) > 0 && strings.EqualFold(strings.TrimSpace(rows[0][0]), "name") {
		startRow = 1
	}

	col := func(row []string, idx int) string {
		if idx < len(row) {
			return strings.TrimSpace(row[idx])
		}
		return ""
	}

	ctx := context.Background()
	created, updated, skipped := 0, 0, 0

	for i := startRow; i < len(rows); i++ {
		row := rows[i]
		name := col(row, 0)
		if name == "" {
			skipped++
			continue
		}
		sport := col(row, 1)
		if sport == "" {
			sport = "football"
		}
		displayName := PtrStr(col(row, 2))
		alias := PtrStr(col(row, 3))
		category := PtrStr(col(row, 4))

		tag, _ := db.Pool.Exec(ctx,
			`UPDATE teams SET display_name=$1, alias=$2, category=$3
			  WHERE name=$4 AND sport=$5`,
			displayName, alias, category, name, sport)
		if tag.RowsAffected() > 0 {
			updated++
			continue
		}
		_, err := db.Pool.Exec(ctx,
			`INSERT INTO teams (name, sport, display_name, alias, category)
			 VALUES ($1,$2,$3,$4,$5)`,
			name, sport, displayName, alias, category)
		if err != nil {
			skipped++
		} else {
			created++
		}
	}

	msg := fmt.Sprintf("Import XLSX dokončen: %d nových, %d aktualizovaných, %d přeskočených.", created, updated, skipped)
	middleware.SetFlash(w, r, "ok", msg)
	http.Redirect(w, r, "/admin/teams", http.StatusSeeOther)
}

// ─── Roster Matrix ────────────────────────────────────────────────────────────

// GET /admin/roster
// Matice: řádky = týmy (skupiny po kategoriích), sloupce = aktivní soutěže.
func AdminRosterMatrix(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		ctx := context.Background()

		// Aktivní soutěže jako sloupce
		compRows, _ := db.Pool.Query(ctx,
			`SELECT id, name, season FROM competitions WHERE is_active=true ORDER BY sort_order ASC NULLS LAST, id DESC`)
		type CompCol struct {
			ID     int
			Name   string
			Season string
		}
		var comps []CompCol
		for compRows.Next() {
			var c CompCol
			_ = compRows.Scan(&c.ID, &c.Name, &c.Season)
			comps = append(comps, c)
		}
		compRows.Close()

		// Všechny týmy seřazené podle jména
		teamRows, _ := db.Pool.Query(ctx,
			`SELECT id, name, sport, alias, display_name, logo_url, category, competition_id
			   FROM teams ORDER BY LOWER(name)`)
		var allTeams []*models.Team
		for teamRows.Next() {
			t := &models.Team{}
			_ = teamRows.Scan(&t.ID, &t.Name, &t.Sport, &t.Alias, &t.DisplayName, &t.LogoURL, &t.Category, &t.CompetitionID)
			allTeams = append(allTeams, t)
		}
		teamRows.Close()

		// Všechny přiřazení team↔competition → set (teamID, compID)
		type assignment struct{ TeamID, CompID int }
		assigned := map[assignment]bool{}
		aRows, _ := db.Pool.Query(ctx, `SELECT team_id, competition_id FROM competition_teams`)
		for aRows.Next() {
			var a assignment
			_ = aRows.Scan(&a.TeamID, &a.CompID)
			assigned[a] = true
		}
		aRows.Close()

		// Skupiny podle category
		type RosterTeam struct {
			*models.Team
			Assigned map[int]bool // compID → true/false
		}
		type RosterGroup struct {
			Category string
			Teams    []RosterTeam
		}

		groupOrder := []string{}
		groupMap := map[string][]RosterTeam{}
		for _, t := range allTeams {
			cat := ""
			if t.Category != nil {
				cat = *t.Category
			}
			if _, ok := groupMap[cat]; !ok {
				groupOrder = append(groupOrder, cat)
			}
			teamAssigned := map[int]bool{}
			for _, c := range comps {
				teamAssigned[c.ID] = assigned[assignment{t.ID, c.ID}]
			}
			groupMap[cat] = append(groupMap[cat], RosterTeam{Team: t, Assigned: teamAssigned})
		}
		var groups []RosterGroup
		for _, cat := range groupOrder {
			if cat != "" {
				groups = append(groups, RosterGroup{Category: cat, Teams: groupMap[cat]})
			}
		}
		if _, ok := groupMap[""]; ok {
			groups = append(groups, RosterGroup{Category: "", Teams: groupMap[""]})
		}

		RenderTemplate(w, r, tmpl, "admin/roster_matrix.html", TemplateData{
			"User":   admin,
			"Comps":  comps,
			"Groups": groups,
			"Flash":  middleware.GetFlash(w, r),
		})
	}
}

// POST /admin/roster/toggle (AJAX)
// Přepne přiřazení jednoho týmu do jedné soutěže.
func AdminRosterToggle(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		jsonError(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		jsonError(w, "bad_request", http.StatusBadRequest)
		return
	}
	teamID, _ := strconv.Atoi(r.FormValue("team_id"))
	compID, _ := strconv.Atoi(r.FormValue("comp_id"))
	checked := r.FormValue("checked") == "1"
	if teamID == 0 || compID == 0 {
		jsonError(w, "missing_params", http.StatusBadRequest)
		return
	}
	ctx := context.Background()
	if checked {
		_, _ = db.Pool.Exec(ctx,
			`INSERT INTO competition_teams (competition_id, team_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			compID, teamID)
	} else {
		_, _ = db.Pool.Exec(ctx,
			`DELETE FROM competition_teams WHERE competition_id=$1 AND team_id=$2`,
			compID, teamID)
	}
	_ = admin
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}
