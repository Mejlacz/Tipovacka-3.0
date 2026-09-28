// handlers/admin_rounds.go — Tipovačka 3.0
// Kola jsou odstraněna — všechny endpointy přesměrují na admin.
package handlers

import (
	"html/template"
	"net/http"
	"strconv"
)

// GET /admin/competitions/{id}/rounds → redirect na matches
func AdminRoundsList(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		RequireAdmin(w, r)
		compID, _ := strconv.Atoi(r.PathValue("competition_id"))
		http.Redirect(w, r, "/admin/competitions/"+strconv.Itoa(compID)+"/matches", http.StatusMovedPermanently)
	}
}

// POST /admin/competitions/{id}/rounds/new → redirect na matches
func AdminRoundCreate(w http.ResponseWriter, r *http.Request) {
	RequireAdmin(w, r)
	compID, _ := strconv.Atoi(r.PathValue("competition_id"))
	http.Redirect(w, r, "/admin/competitions/"+strconv.Itoa(compID)+"/matches", http.StatusSeeOther)
}

// POST /admin/rounds/quick-new → redirect na přidání zápasů
func AdminRoundQuickNew(w http.ResponseWriter, r *http.Request) {
	RequireAdmin(w, r)
	if err := r.ParseForm(); err == nil {
		compID, _ := strconv.Atoi(r.FormValue("competition_id"))
		if compID > 0 {
			http.Redirect(w, r, "/admin/competitions/"+strconv.Itoa(compID)+"/matches", http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, "/admin/add-matches", http.StatusSeeOther)
}

// POST /admin/rounds/{id}/edit → redirect na admin
func AdminRoundEdit(w http.ResponseWriter, r *http.Request) {
	RequireAdmin(w, r)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// POST /admin/rounds/{id}/toggle → redirect na admin
func AdminRoundToggle(w http.ResponseWriter, r *http.Request) {
	RequireAdmin(w, r)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// POST /admin/rounds/{id}/notify-new → redirect na admin
func AdminRoundNotifyNew(w http.ResponseWriter, r *http.Request) {
	RequireAdmin(w, r)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}
