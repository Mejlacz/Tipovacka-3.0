// handlers/admin_teams.go - Tipovačka 2.0
// Správa týmů.
package handlers

import (
	"context"
	"encoding/csv"
	"encoding/json"
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

// TeamRow holds a team with extra display data for the admin teams page.
type TeamRow struct {
	models.Team
	MatchCount int
	CompNames  []string
	IsOrphan   bool
}

// GET /admin/teams
func AdminTeamsList(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		ctx := context.Background()

		// Load all teams with match count and orphan flag
		rows, err := db.Pool.Query(ctx, `
			SELECT t.id, t.name, t.sport,
			       COALESCE(t.alias,''), COALESCE(t.display_name,''),
			       COALESCE(t.logo_url,''), COALESCE(t.category,''),
			       (SELECT COUNT(*) FROM matches m WHERE m.home_team_id=t.id OR m.away_team_id=t.id),
			       EXISTS(SELECT 1 FROM competition_teams ct WHERE ct.team_id=t.id)
			FROM teams t
			ORDER BY t.sport, LOWER(COALESCE(NULLIF(t.display_name,''), t.name))`)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		var teamRows []*TeamRow
		teamByID := map[int]*TeamRow{}
		for rows.Next() {
			tr := &TeamRow{}
			var alias, displayName, logoURL, category string
			var hasComp bool
			if err := rows.Scan(&tr.ID, &tr.Name, &tr.Sport, &alias, &displayName,
				&logoURL, &category, &tr.MatchCount, &hasComp); err != nil {
				continue
			}
			if alias != "" {
				tr.Alias = &alias
			}
			if displayName != "" {
				tr.DisplayName = &displayName
			}
			if logoURL != "" {
				tr.LogoURL = &logoURL
			}
			if category != "" {
				tr.Category = &category
			}
			tr.IsOrphan = !hasComp
			teamRows = append(teamRows, tr)
			teamByID[tr.ID] = tr
		}
		rows.Close()

		// Load competition names per team
		compRows, _ := db.Pool.Query(ctx, `
			SELECT ct.team_id, c.name, c.season
			FROM competition_teams ct
			JOIN competitions c ON c.id = ct.competition_id
			ORDER BY c.id DESC`)
		if compRows != nil {
			for compRows.Next() {
				var tid int
				var cname, cseason string
				if err := compRows.Scan(&tid, &cname, &cseason); err == nil {
					if tr, ok := teamByID[tid]; ok {
						label := cname
						if cseason != "" {
							label += " " + cseason
						}
						tr.CompNames = append(tr.CompNames, label)
					}
				}
			}
			compRows.Close()
		}

		// Count orphans
		orphanCount := 0
		for _, tr := range teamRows {
			if tr.IsOrphan {
				orphanCount++
			}
		}

		// Build JSON for merge modal
		type teamJSON struct {
			ID          int    `json:"id"`
			Name        string `json:"name"`
			DisplayName string `json:"display_name"`
			Sport       string `json:"sport"`
		}
		var teamsForJSON []teamJSON
		for _, tr := range teamRows {
			dn := ""
			if tr.DisplayName != nil {
				dn = *tr.DisplayName
			}
			teamsForJSON = append(teamsForJSON, teamJSON{
				ID: tr.ID, Name: tr.Name, DisplayName: dn, Sport: tr.Sport,
			})
		}
		teamsJSON, _ := json.Marshal(teamsForJSON)

		RenderTemplate(w, r, tmpl, "teams.html", TemplateData{
			"User":        admin,
			"Teams":       teamRows,
			"OrphanCount": orphanCount,
			"TeamsJSON":   string(teamsJSON),
			"Flash":       middleware.GetFlash(w, r),
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
		middleware.SetFlash(w, r, "error", "Tým nelze smazat - má přiřazené zápasy.")
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

	// Safety check: confirm_name is optional - empty string skips the check
	if confirmName != "" && !strings.EqualFold(confirmName, targetName) {
		flash("error", "Name mismatch - expected: "+targetName)
		return
	}

	// 1. Přepiš zápasy
	_, _ = db.Pool.Exec(ctx, `UPDATE matches SET home_team_id=$1 WHERE home_team_id=$2`, targetID, sourceID)
	_, _ = db.Pool.Exec(ctx, `UPDATE matches SET away_team_id=$1 WHERE away_team_id=$2`, targetID, sourceID)

	// 2. Přenes competition_teams (přeskoč konflikty - target tam už může být)
	_, _ = db.Pool.Exec(ctx, `
		INSERT INTO competition_teams (competition_id, team_id)
		SELECT competition_id, $1 FROM competition_teams WHERE team_id=$2
		ON CONFLICT DO NOTHING`, targetID, sourceID)

	// 3. Ulož jméno zdroje jako alias na cílovém týmu (pro budoucí API matching)
	appendTeamAlias(ctx, sourceName, targetID)

	// 4. Smaž zdroj
	_, _ = db.Pool.Exec(ctx, `DELETE FROM competition_teams WHERE team_id=$1`, sourceID)
	_, _ = db.Pool.Exec(ctx, `DELETE FROM teams WHERE id=$1`, sourceID)

	desc := fmt.Sprintf("Tym '%s' (ID %d) sloucen do '%s' (ID %d)", sourceName, sourceID, targetName, targetID)
	LogAction(&admin.ID, admin.Username, "team_merge", "team", &sourceID, desc, nil, nil)

	flash("ok", desc)
}

// ── GET /admin/teams/orphans ──────────────────────────────────────────────────
// Zobrazí týmy, které nejsou přiřazeny k žádné soutěži.

type OrphanTeam struct {
	ID          int
	Name        string
	Sport       string
	DisplayName string
	Alias       string
	MatchCount  int // počet zápasů kde tým hraje
}

func AdminTeamOrphans(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		ctx := context.Background()

		rows, err := db.Pool.Query(ctx, `
			SELECT t.id, t.name, t.sport,
			       COALESCE(t.display_name,''), COALESCE(t.alias,''),
			       (SELECT COUNT(*) FROM matches m WHERE m.home_team_id=t.id OR m.away_team_id=t.id)
			FROM teams t
			WHERE NOT EXISTS (
			    SELECT 1 FROM competition_teams ct WHERE ct.team_id = t.id
			)
			ORDER BY t.sport, LOWER(COALESCE(t.display_name, t.name))`)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()

		var orphans []OrphanTeam
		for rows.Next() {
			var o OrphanTeam
			if err := rows.Scan(&o.ID, &o.Name, &o.Sport, &o.DisplayName, &o.Alias, &o.MatchCount); err == nil {
				orphans = append(orphans, o)
			}
		}

		// Načti všechny ostatní týmy (cíle pro merge), seskupené po sportu
		type teamOpt struct {
			ID          int    `json:"id"`
			Name        string `json:"name"`
			DisplayName string `json:"display_name"`
			Sport       string `json:"sport"`
		}
		optRows, _ := db.Pool.Query(ctx, `
			SELECT t.id, t.name, COALESCE(t.display_name,''), t.sport
			FROM teams t
			WHERE EXISTS (SELECT 1 FROM competition_teams ct WHERE ct.team_id = t.id)
			ORDER BY t.sport, LOWER(COALESCE(NULLIF(t.display_name,''), t.name))`)
		var allTeams []teamOpt
		if optRows != nil {
			for optRows.Next() {
				var t teamOpt
				if err := optRows.Scan(&t.ID, &t.Name, &t.DisplayName, &t.Sport); err == nil {
					allTeams = append(allTeams, t)
				}
			}
			optRows.Close()
		}
		allTeamsJSON, _ := json.Marshal(allTeams)

		RenderTemplate(w, r, tmpl, "admin/team_orphans.html", TemplateData{
			"User":         admin,
			"Orphans":      orphans,
			"AllTeamsJSON": string(allTeamsJSON),
			"Flash":        middleware.GetFlash(w, r),
		})
	}
}

// ── POST /admin/teams/orphans/bulk ────────────────────────────────────────────
// Hromadné zpracování orphan týmů.
// Tělo: JSON pole [{id:X, action:"merge", target_id:Y} | {id:X, action:"delete"}]

func AdminTeamOrphansBulk(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	admin := RequireAdmin(w, r)
	if admin == nil {
		w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
		return
	}
	if err := r.ParseForm(); err != nil {
		w.Write([]byte(`{"ok":false,"error":"bad form"}`))
		return
	}

	type action struct {
		ID       int    `json:"id"`
		Action   string `json:"action"`    // "merge" | "delete" | "skip"
		TargetID int    `json:"target_id"` // pro merge
	}
	var actions []action
	if raw := strings.TrimSpace(r.FormValue("actions")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &actions); err != nil {
			b, _ := json.Marshal(map[string]interface{}{"ok": false, "error": "bad JSON: " + err.Error()})
			w.Write(b)
			return
		}
	}

	ctx := context.Background()
	merged, deleted, skipped := 0, 0, 0
	var errs []string

	for _, a := range actions {
		if a.Action == "skip" || a.ID == 0 {
			skipped++
			continue
		}

		// Ověř že tým je stále orphan (nemá competition_teams)
		var sourceID = a.ID
		var sourceName string
		if err := db.Pool.QueryRow(ctx, `SELECT name FROM teams WHERE id=$1`, sourceID).Scan(&sourceName); err != nil {
			errs = append(errs, fmt.Sprintf("Tým ID %d nenalezen", sourceID))
			continue
		}

		if a.Action == "delete" {
			// Smažeme jen pokud nemá žádné zápasy
			var mc int
			_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM matches WHERE home_team_id=$1 OR away_team_id=$1`, sourceID).Scan(&mc)
			if mc > 0 {
				errs = append(errs, fmt.Sprintf("Tym '%s' ma %d zapasu - nelze smazat, pouzij slouceni", sourceName, mc))
				continue
			}
			_, _ = db.Pool.Exec(ctx, `DELETE FROM competition_teams WHERE team_id=$1`, sourceID)
			_, _ = db.Pool.Exec(ctx, `DELETE FROM teams WHERE id=$1`, sourceID)
			LogAction(&admin.ID, admin.Username, "team_delete", "team", &sourceID,
				fmt.Sprintf("Orphan tym '%s' (ID %d) smazan z cleanup stranky", sourceName, sourceID), nil, nil)
			deleted++
			continue
		}

		if a.Action == "merge" {
			if a.TargetID == 0 || a.TargetID == sourceID {
				errs = append(errs, fmt.Sprintf("Tym '%s': neplatny cil slouceni", sourceName))
				continue
			}
			var targetName string
			if err := db.Pool.QueryRow(ctx, `SELECT name FROM teams WHERE id=$1`, a.TargetID).Scan(&targetName); err != nil {
				errs = append(errs, fmt.Sprintf("Tym '%s': cilovy tym ID %d nenalezen", sourceName, a.TargetID))
				continue
			}
			// Merge: přepiš zápasy, přenes competition_teams, ulož alias, smaž zdroj
			_, _ = db.Pool.Exec(ctx, `UPDATE matches SET home_team_id=$1 WHERE home_team_id=$2`, a.TargetID, sourceID)
			_, _ = db.Pool.Exec(ctx, `UPDATE matches SET away_team_id=$1 WHERE away_team_id=$2`, a.TargetID, sourceID)
			_, _ = db.Pool.Exec(ctx, `
				INSERT INTO competition_teams (competition_id, team_id)
				SELECT competition_id, $1 FROM competition_teams WHERE team_id=$2
				ON CONFLICT DO NOTHING`, a.TargetID, sourceID)
			appendTeamAlias(ctx, sourceName, a.TargetID)
			_, _ = db.Pool.Exec(ctx, `DELETE FROM competition_teams WHERE team_id=$1`, sourceID)
			_, _ = db.Pool.Exec(ctx, `DELETE FROM teams WHERE id=$1`, sourceID)
			desc := fmt.Sprintf("Orphan '%s' (ID %d) sloučen do '%s' (ID %d)", sourceName, sourceID, targetName, a.TargetID)
			LogAction(&admin.ID, admin.Username, "team_merge", "team", &sourceID, desc, nil, nil)
			merged++
		}
	}

	msg := fmt.Sprintf("Hotovo: %d sloučeno, %d smazáno, %d přeskočeno.", merged, deleted, skipped)
	b, _ := json.Marshal(map[string]interface{}{"ok": true, "message": msg, "errors": errs})
	w.Write(b)
}

// TeamGroup - skupína týmů podle category pro šablonu competition_teams.html
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

		// Filtruj týmy podle sportu soutěže (hokejová soutěž → hokejové týmy)
		teamSport := comp.Sport
		if teamSport == "" {
			teamSport = "football"
		}
		allTeamRows, _ := db.Pool.Query(ctx,
			`SELECT id, name, sport, alias, display_name, logo_url, category, competition_id
			   FROM teams WHERE sport=$1 ORDER BY LOWER(name)`, teamSport)
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

	// Detekuj hlavičku - pokud první řádek má "name" nebo "Name"
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

	// Detekuj záhlaví - pokud první buňka je "name" nebo "Name"
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

// POST /admin/teams/bulk-delete (AJAX)
// Tělo: URL-encoded, pole "ids" = JSON pole intů
func AdminTeamBulkDelete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	admin := RequireAdmin(w, r)
	if admin == nil {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
		return
	}
	// r.Form already parsed by CSRF middleware
	raw := r.FormValue("ids")
	if raw == "" {
		w.Write([]byte(`{"ok":false,"error":"missing ids"}`))
		return
	}
	var idStrs []string
	if err := json.Unmarshal([]byte(raw), &idStrs); err != nil {
		b, _ := json.Marshal(map[string]interface{}{"ok": false, "error": "bad JSON: " + err.Error()})
		w.Write(b)
		return
	}
	ctx := context.Background()
	deleted, skipped := 0, 0
	for _, idStr := range idStrs {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}
		var mc int
		_ = db.Pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM matches WHERE home_team_id=$1 OR away_team_id=$1`, id).Scan(&mc)
		if mc > 0 {
			skipped++
			continue
		}
		_, _ = db.Pool.Exec(ctx, `DELETE FROM competition_teams WHERE team_id=$1`, id)
		res, _ := db.Pool.Exec(ctx, `DELETE FROM teams WHERE id=$1`, id)
		if res.RowsAffected() > 0 {
			deleted++
		}
	}
	b, _ := json.Marshal(map[string]interface{}{"ok": true, "deleted": deleted, "skipped": skipped})
	w.Write(b)
}

// ── appendTeamAlias ────────────────────────────────────────────────────────────
// Appends name as an alias on a team (comma-separated in the alias column).
// If the name is already present (case-insensitive), it is skipped.
// This is called after every merge and after every import resolve so that
// future API imports can find the team without showing the modal again.
func appendTeamAlias(ctx context.Context, name string, teamID int) {
	if name == "" || teamID == 0 {
		return
	}
	_, _ = db.Pool.Exec(ctx, `
		UPDATE teams SET alias = CASE
		    WHEN COALESCE(alias,'') = '' THEN $1
		    WHEN LOWER($1) = ANY(string_to_array(LOWER(alias), ',')) THEN alias
		    ELSE alias || ',' || $1
		END
		WHERE id=$2`, name, teamID)
}

// ── GET /admin/teams/dupes ─────────────────────────────────────────────────────
// Zobrazí páry týmů které jsou pravděpodobně duplicitní.

type DupePair struct {
	ID1, ID2     int
	Name1, Name2 string
	DN1, DN2     string
	Sport        string
	MC1, MC2     int
}

func AdminTeamDupes(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		ctx := context.Background()

		rows, err := db.Pool.Query(ctx, `
			SELECT
			    t1.id, t1.name, COALESCE(t1.display_name,''), t1.sport,
			    (SELECT COUNT(*) FROM matches m WHERE m.home_team_id=t1.id OR m.away_team_id=t1.id),
			    t2.id, t2.name, COALESCE(t2.display_name,''),
			    (SELECT COUNT(*) FROM matches m WHERE m.home_team_id=t2.id OR m.away_team_id=t2.id)
			FROM teams t1
			JOIN teams t2 ON t1.id < t2.id AND t1.sport = t2.sport
			WHERE (
			    LOWER(t1.name) = LOWER(t2.name)
			    OR (COALESCE(t1.display_name,'') != '' AND LOWER(t1.display_name) = LOWER(COALESCE(t2.display_name,'')))
			    OR (LENGTH(t1.name) >= 4 AND (
			        LOWER(t2.name) LIKE LOWER(t1.name) || ' %'
			        OR LOWER(t1.name) LIKE LOWER(t2.name) || ' %'
			    ))
			)
			ORDER BY t1.sport, t1.name`)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()

		var pairs []DupePair
		for rows.Next() {
			var p DupePair
			if err := rows.Scan(
				&p.ID1, &p.Name1, &p.DN1, &p.Sport, &p.MC1,
				&p.ID2, &p.Name2, &p.DN2, &p.MC2,
			); err == nil {
				pairs = append(pairs, p)
			}
		}

		// All teams JSON for merge UI — includes comp_names and match_count.
		// ID is a string to avoid JS Number precision loss for large CockroachDB int64 IDs.
		type dupeTeamJSON struct {
			ID          string   `json:"id"`
			Name        string   `json:"name"`
			DisplayName string   `json:"display_name"`
			Sport       string   `json:"sport"`
			CompNames   []string `json:"comp_names"`
			MatchCount  int      `json:"match_count"`
		}
		atRows, _ := db.Pool.Query(ctx, `
			SELECT t.id, t.name, COALESCE(t.display_name,''), t.sport,
			       (SELECT COUNT(*) FROM matches m WHERE m.home_team_id=t.id OR m.away_team_id=t.id)
			FROM teams t
			ORDER BY t.sport, LOWER(COALESCE(NULLIF(t.display_name,''), t.name))`)
		allTeams := []dupeTeamJSON{} // never nil — marshals as [] not null
		teamIdxByID := map[int64]int{}
		if atRows != nil {
			for atRows.Next() {
				var rawID int64
				var t dupeTeamJSON
				if err := atRows.Scan(&rawID, &t.Name, &t.DisplayName, &t.Sport, &t.MatchCount); err == nil {
					t.ID = strconv.FormatInt(rawID, 10)
					t.CompNames = []string{}
					teamIdxByID[rawID] = len(allTeams)
					allTeams = append(allTeams, t)
				}
			}
			atRows.Close()
		}
		// Load competition names per team
		dtCompRows, _ := db.Pool.Query(ctx, `
			SELECT ct.team_id, c.name, c.season
			FROM competition_teams ct
			JOIN competitions c ON c.id = ct.competition_id
			ORDER BY c.id DESC`)
		if dtCompRows != nil {
			for dtCompRows.Next() {
				var tid int64
				var cname, cseason string
				if err := dtCompRows.Scan(&tid, &cname, &cseason); err == nil {
					if idx, ok := teamIdxByID[tid]; ok {
						label := cname
						if cseason != "" {
							label += " " + cseason
						}
						allTeams[idx].CompNames = append(allTeams[idx].CompNames, label)
					}
				}
			}
			dtCompRows.Close()
		}
		allTeamsJSONBytes, _ := json.Marshal(allTeams)

		RenderTemplate(w, r, tmpl, "admin/team_dupes.html", TemplateData{
			"User":         admin,
			"Pairs":        pairs,
			"AllTeamsJSON": template.JS(allTeamsJSONBytes), // template.JS = no escaping in <script>
			"Flash":        middleware.GetFlash(w, r),
		})
	}
}

// ── POST /admin/teams/merge-bulk ──────────────────────────────────────────────
// JSON API: provede hromadné sloučení páru {source_id, target_id}.
// Tělo: JSON pole [{source_id:X, target_id:Y}, ...]
// Odpověď: {"ok":true,"merged":N,"errors":[...]}
func AdminTeamMergeBulk(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	admin := RequireAdmin(w, r)
	if admin == nil {
		w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
		return
	}
	// IDs are sent as strings from JS to avoid JS Number precision loss for large int64 CockroachDB IDs.
	type mergePair struct {
		SourceID string `json:"source_id"`
		TargetID string `json:"target_id"`
	}
	var pairs []mergePair
	if err := json.NewDecoder(r.Body).Decode(&pairs); err != nil {
		b, _ := json.Marshal(map[string]interface{}{"ok": false, "error": "bad JSON: " + err.Error()})
		w.Write(b)
		return
	}

	ctx := context.Background()
	merged := 0
	var errs []string

	for _, p := range pairs {
		sourceID, srcErr := strconv.ParseInt(p.SourceID, 10, 64)
		targetID, tgtErr := strconv.ParseInt(p.TargetID, 10, 64)
		if srcErr != nil || tgtErr != nil || sourceID == 0 || targetID == 0 || sourceID == targetID {
			errs = append(errs, fmt.Sprintf("neplatny par '%s'->'%s'", p.SourceID, p.TargetID))
			continue
		}
		var sourceName, targetName string
		if err := db.Pool.QueryRow(ctx, `SELECT name FROM teams WHERE id=$1`, sourceID).Scan(&sourceName); err != nil {
			errs = append(errs, fmt.Sprintf("zdrojovy tym ID %s nenalezen", p.SourceID))
			continue
		}
		if err := db.Pool.QueryRow(ctx, `SELECT name FROM teams WHERE id=$1`, targetID).Scan(&targetName); err != nil {
			errs = append(errs, fmt.Sprintf("cilovy tym ID %s nenalezen", p.TargetID))
			continue
		}
		// 1. Přepiš zápasy
		if _, err := db.Pool.Exec(ctx, `UPDATE matches SET home_team_id=$1 WHERE home_team_id=$2`, targetID, sourceID); err != nil {
			errs = append(errs, fmt.Sprintf("'%s'->home_team update: %v", sourceName, err))
			continue
		}
		if _, err := db.Pool.Exec(ctx, `UPDATE matches SET away_team_id=$1 WHERE away_team_id=$2`, targetID, sourceID); err != nil {
			errs = append(errs, fmt.Sprintf("'%s'->away_team update: %v", sourceName, err))
			continue
		}
		// 2. Přenes competition_teams
		if _, err := db.Pool.Exec(ctx, `
			INSERT INTO competition_teams (competition_id, team_id)
			SELECT competition_id, $1 FROM competition_teams WHERE team_id=$2
			ON CONFLICT DO NOTHING`, targetID, sourceID); err != nil {
			errs = append(errs, fmt.Sprintf("'%s'->competition_teams transfer: %v", sourceName, err))
			continue
		}
		// 3. Ulož alias
		appendTeamAlias(ctx, sourceName, int(targetID))
		// 4. Smaž zdroj
		if _, err := db.Pool.Exec(ctx, `DELETE FROM competition_teams WHERE team_id=$1`, sourceID); err != nil {
			errs = append(errs, fmt.Sprintf("'%s'->delete competition_teams: %v", sourceName, err))
			continue
		}
		if _, err := db.Pool.Exec(ctx, `DELETE FROM teams WHERE id=$1`, sourceID); err != nil {
			errs = append(errs, fmt.Sprintf("'%s'->delete team: %v", sourceName, err))
			continue
		}

		desc := fmt.Sprintf("Bulk merge: '%s' (ID %s) sloucen do '%s' (ID %s)", sourceName, p.SourceID, targetName, p.TargetID)
		sourceIDInt := int(sourceID)
		LogAction(&admin.ID, admin.Username, "team_merge", "team", &sourceIDInt, desc, nil, nil)
		merged++
	}

	b, _ := json.Marshal(map[string]interface{}{"ok": true, "merged": merged, "errors": errs})
	w.Write(b)
}

// ── GET /admin/teams/assign ────────────────────────────────────────────────────
// Hromadné přiřazování týmů do soutěží.

type TeamAssignRow struct {
	ID          int
	Name        string
	DisplayName string
	Sport       string
	MatchCount  int
	CompNames   []string
	IsOrphan    bool
}

func AdminTeamBulkAssign(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		ctx := context.Background()

		rows, err := db.Pool.Query(ctx, `
			SELECT t.id, t.name, COALESCE(t.display_name,''), t.sport,
			       (SELECT COUNT(*) FROM matches m WHERE m.home_team_id=t.id OR m.away_team_id=t.id),
			       EXISTS(SELECT 1 FROM competition_teams ct WHERE ct.team_id=t.id)
			FROM teams t
			ORDER BY t.sport, LOWER(COALESCE(NULLIF(t.display_name,''), t.name))`)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		var teams []*TeamAssignRow
		teamByID := map[int]*TeamAssignRow{}
		for rows.Next() {
			tr := &TeamAssignRow{}
			var hasComp bool
			if err := rows.Scan(&tr.ID, &tr.Name, &tr.DisplayName, &tr.Sport,
				&tr.MatchCount, &hasComp); err == nil {
				tr.IsOrphan = !hasComp
				teams = append(teams, tr)
				teamByID[tr.ID] = tr
			}
		}
		rows.Close()

		compRows, _ := db.Pool.Query(ctx, `
			SELECT ct.team_id, c.id, c.name, c.season
			FROM competition_teams ct
			JOIN competitions c ON c.id = ct.competition_id
			ORDER BY c.id DESC`)
		if compRows != nil {
			for compRows.Next() {
				var tid, cid int
				var cname, cseason string
				if err := compRows.Scan(&tid, &cid, &cname, &cseason); err == nil {
					if tr, ok := teamByID[tid]; ok {
						label := cname
						if cseason != "" {
							label += " " + cseason
						}
						tr.CompNames = append(tr.CompNames, label)
					}
				}
			}
			compRows.Close()
		}

		type CompOption struct {
			ID     int
			Name   string
			Season string
			Sport  string
		}
		coptRows, _ := db.Pool.Query(ctx,
			`SELECT id, name, COALESCE(season,''), COALESCE(sport,'') FROM competitions ORDER BY id DESC`)
		var comps []CompOption
		if coptRows != nil {
			for coptRows.Next() {
				var c CompOption
				if err := coptRows.Scan(&c.ID, &c.Name, &c.Season, &c.Sport); err == nil {
					comps = append(comps, c)
				}
			}
			coptRows.Close()
		}

		RenderTemplate(w, r, tmpl, "admin/team_bulk_assign.html", TemplateData{
			"User":  admin,
			"Teams": teams,
			"Comps": comps,
			"Flash": middleware.GetFlash(w, r),
		})
	}
}

// POST /admin/teams/assign
func AdminTeamBulkAssignPost(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	action := r.FormValue("action") // "add" or "remove"
	compID, _ := strconv.Atoi(r.FormValue("competition_id"))
	teamIDStrs := r.Form["team_ids"]

	ctx := context.Background()
	count := 0
	for _, idStr := range teamIDStrs {
		teamID, _ := strconv.Atoi(idStr)
		if teamID == 0 {
			continue
		}
		if action == "add" && compID > 0 {
			_, _ = db.Pool.Exec(ctx,
				`INSERT INTO competition_teams (competition_id, team_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
				compID, teamID)
			count++
		} else if action == "remove" && compID > 0 {
			_, _ = db.Pool.Exec(ctx,
				`DELETE FROM competition_teams WHERE competition_id=$1 AND team_id=$2`,
				compID, teamID)
			count++
		}
	}

	middleware.SetFlash(w, r, "ok", fmt.Sprintf("Hotovo: %d tymu zpracovano.", count))
	http.Redirect(w, r, "/admin/teams/assign", http.StatusSeeOther)
}
