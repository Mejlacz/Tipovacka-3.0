// handlers/admin_api_teams.go — Tipovačka 2.0
// Endpoint pro ověření / přiřazení týmů před importem zápasů.
//
// GET /admin/api/team-resolve
//   Zkontroluje které týmy z preview nejsou v DB a vrátí
//   seznam existujících týmů pro ruční přiřazení.
package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"tipovacka/db"
)

// ── GET /admin/api/team-resolve ───────────────────────────────────────────────
// Query params:
//   names  — čárkou oddělené názvy týmů (z preview)
//   sport  — "hockey" | "football"
//
// Odpověď:
//   unknowns  — týmy které by import vytvořil jako nové
//   all_teams — všechny existující týmy daného sportu pro výběr

func AdminAPITeamResolve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	admin := RequireAdmin(w, r)
	if admin == nil {
		w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
		return
	}

	sport := strings.TrimSpace(r.URL.Query().Get("sport"))
	if sport == "" {
		sport = "football"
	}
	namesRaw := r.URL.Query().Get("names")
	if namesRaw == "" {
		b, _ := json.Marshal(map[string]interface{}{"ok": true, "unknowns": []interface{}{}, "all_teams": []interface{}{}})
		w.Write(b)
		return
	}

	ctx := context.Background()

	// ── Rozdělíme jména ──────────────────────────────────────────────────────
	namesList := splitTeamNames(namesRaw)

	// ── Pro každé jméno zjistíme, zda existuje v DB ───────────────────────────
	type unknownTeam struct {
		Name string `json:"name"`
	}
	var unknowns []unknownTeam
	for _, name := range namesList {
		if lookupExistingTeam(ctx, name, sport) == 0 {
			unknowns = append(unknowns, unknownTeam{Name: name})
		}
	}

	// ── Načteme všechny existující týmy daného sportu ─────────────────────────
	type teamOption struct {
		ID          int    `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}
	rows, err := db.Pool.Query(ctx,
		`SELECT id, name, COALESCE(display_name,'') FROM teams WHERE sport=$1 ORDER BY COALESCE(NULLIF(display_name,''), name)`,
		sport)
	if err != nil {
		b, _ := json.Marshal(map[string]interface{}{"ok": false, "error": err.Error()})
		w.Write(b)
		return
	}
	defer rows.Close()

	var allTeams []teamOption
	for rows.Next() {
		var t teamOption
		if err := rows.Scan(&t.ID, &t.Name, &t.DisplayName); err == nil {
			allTeams = append(allTeams, t)
		}
	}
	if allTeams == nil {
		allTeams = []teamOption{}
	}
	if unknowns == nil {
		unknowns = []unknownTeam{}
	}

	b, _ := json.Marshal(map[string]interface{}{
		"ok":        true,
		"unknowns":  unknowns,
		"all_teams": allTeams,
	})
	w.Write(b)
}

// ── lookupExistingTeam — read-only varianta upsertTeam ────────────────────────
// Vrátí ID pokud tým s tímto jménem/aliasem/display_name existuje, jinak 0.
func lookupExistingTeam(ctx context.Context, name, sport string) int {
	if name == "" {
		return 0
	}
	var id int
	// 1. Přesná shoda jména
	if err := db.Pool.QueryRow(ctx,
		`SELECT id FROM teams WHERE name=$1 AND sport=$2`, name, sport).Scan(&id); err == nil {
		return id
	}
	// 2. Alias (case-insensitive)
	if err := db.Pool.QueryRow(ctx,
		`SELECT id FROM teams WHERE LOWER(alias)=LOWER($1) AND sport=$2`, name, sport).Scan(&id); err == nil {
		return id
	}
	// 3. Display name (case-insensitive)
	if err := db.Pool.QueryRow(ctx,
		`SELECT id FROM teams WHERE LOWER(display_name)=LOWER($1) AND sport=$2`, name, sport).Scan(&id); err == nil {
		return id
	}
	return 0
}

// ── splitTeamNames — rozdělí čárkou oddělený seznam jmen ─────────────────────
func splitTeamNames(s string) []string {
	raw := strings.Split(s, ",")
	seen := map[string]bool{}
	var out []string
	for _, n := range raw {
		n = strings.TrimSpace(n)
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}
