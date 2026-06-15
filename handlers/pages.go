// handlers/pages.go — Tipovačka 2.0
// Statické stránky s editovatelným obsahem (Pravidla).
package handlers

import (
	"context"
	"html/template"
	"net/http"
	"strings"

	"tipovacka/db"
	"tipovacka/middleware"
)

const _rulesKey = "page_pravidla"

const _defaultRules = `<h2>Jak to funguje</h2>
<p>
  Tipujeme výsledky zápasů. Před každým kolem zadáš svůj tip na přesné skóre.
  Tipy musí dorazit <strong>nejméně 5 hodin před prvním utkáním daného kola</strong>.
  Po uzávěrce se tipy zamknou a nelze je měnit.
</p>

<h2>Bodování</h2>
<ul>
  <li><strong>3 body</strong> — přesný výsledek (správné skóre)</li>
  <li><strong>1 bod</strong> — správný vítěz nebo remíza (skóre není přesné)</li>
  <li><strong>0 bodů</strong> — netrefíš ani výsledek</li>
</ul>

<h2>Extra otázky — bonusové body</h2>
<ul>
  <li><strong>7 bodů</strong> — správně tipnutý vítěz turnaje</li>
  <li><strong>4 body</strong> — vítěz kanadského bodování</li>
  <li><strong>4 body</strong> — nejlepší střelec</li>
</ul>

<h2>Pořadí a výhra</h2>
<p>
  Body se průběžně sčítají. Při shodě bodů rozhoduje <strong>počet přesně trefených výsledků</strong>.
  Výhra se dělí v poměru <strong>50 : 30 : 20</strong> pro první tři místa.
</p>`

func getRules(ctx context.Context) string {
	var val string
	err := db.Pool.QueryRow(ctx, `SELECT value FROM site_config WHERE key=$1`, _rulesKey).Scan(&val)
	if err != nil {
		return _defaultRules
	}
	return val
}

// GET /pravidla
// GET /info — uživatelský manuál
func InfoPage(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := RequireLogin(w, r)
		if user == nil {
			return
		}
		RenderTemplate(w, r, tmpl, "info.html", TemplateData{
			"User":  user,
			"Title": "Jak funguje Tipovačka",
		})
	}
}

func PravidlaPage(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := RequireLogin(w, r)
		if user == nil {
			return
		}
		flash := middleware.GetFlash(w, r)
		content := getRules(context.Background())
		RenderTemplate(w, r, tmpl, "pravidla.html", TemplateData{
			"User":    user,
			"Content": template.HTML(content),
			"Flash":   flash,
		})
	}
}

// GET /admin/pravidla/edit
func AdminPravidlaEditForm(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		content := getRules(context.Background())
		RenderTemplate(w, r, tmpl, "pravidla_edit.html", TemplateData{
			"User":    admin,
			"Content": content,
		})
	}
}

// POST /admin/pravidla/edit
func AdminPravidlaEditSubmit(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	content := strings.TrimSpace(r.FormValue("content"))
	ctx := context.Background()
	_, _ = db.Pool.Exec(ctx,
		`INSERT INTO site_config (key, value) VALUES ($1,$2)
		 ON CONFLICT (key) DO UPDATE SET value=$2`, _rulesKey, content)
	middleware.SetFlash(w, r, "ok", "Pravidla byla uložena.")
	http.Redirect(w, r, "/pravidla", http.StatusSeeOther)
}

// POST /admin/pravidla/reset
func AdminPravidlaReset(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	ctx := context.Background()
	_, _ = db.Pool.Exec(ctx,
		`INSERT INTO site_config (key, value) VALUES ($1,$2)
		 ON CONFLICT (key) DO UPDATE SET value=$2`, _rulesKey, _defaultRules)
	middleware.SetFlash(w, r, "ok", "Pravidla obnovena na výchozí obsah.")
	http.Redirect(w, r, "/pravidla", http.StatusSeeOther)
}

// GET /admin/code-map
func AdminCodeMap(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		RenderTemplate(w, r, tmpl, "admin/code_map.html", TemplateData{"User": admin})
	}
}

// GET /admin/error-map
func AdminErrorMap(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		// Tabulka action kódů pro audit log
		auditActions := [][3]string{
			{"login", "Úspěšné přihlášení uživatele", "auth.go"},
			{"login_fail", "Neúspěšný pokus o přihlášení", "auth.go"},
			{"register", "Nová registrace uživatele", "auth.go"},
			{"register_fail", "Chyba při registraci", "auth.go"},
			{"user_create", "Admin vytvořil uživatele", "admin.go"},
			{"user_edit", "Admin upravil profil uživatele", "admin.go"},
			{"user_delete", "Admin smazal uživatele", "admin.go"},
			{"user_approve", "Admin schválil uživatele", "admin.go"},
			{"user_block", "Admin zablokoval uživatele", "admin.go"},
			{"user_unblock", "Admin odblokoval uživatele", "admin.go"},
			{"user_role", "Změna role přes set-role", "admin.go"},
			{"user_toggle_admin", "Přepnutí admin role", "admin.go"},
			{"user_toggle_owner", "Přepnutí owner role", "admin.go"},
			{"user_toggle_inactive", "Přepnutí inactive stavu", "admin.go"},
			{"user_toggle_hidden", "Přepnutí hidden stavu", "admin.go"},
			{"user_reset_password", "Admin resetoval heslo", "admin.go"},
			{"user_set_password", "Admin nastavil heslo", "admin.go"},
			{"user_send_welcome", "Odeslán uvítací email", "admin.go"},
			{"merge_user", "Sloučení dvou uživatelských účtů", "admin_user_merge.go"},
			{"comp_create", "Vytvoření soutěže", "admin.go"},
			{"comp_edit", "Úprava soutěže", "admin.go"},
			{"comp_toggle", "Aktivace/archivace soutěže", "admin.go"},
			{"comp_deadline", "Nastavení/smazání deadline", "admin.go"},
			{"comp_delete", "Smazání soutěže", "admin.go"},
			{"match_create", "Přidání zápasu", "admin_matches.go"},
			{"match_edit", "Editace zápasu", "admin_matches.go"},
			{"match_score", "Zadání výsledku zápasu", "admin_matches.go"},
			{"match_score_clear", "Smazání výsledku zápasu", "admin_matches.go"},
			{"match_delete", "Smazání zápasu", "admin_matches.go"},
			{"match_date_change", "Změna data zápasu", "admin_matches.go"},
			{"match_add_quick", "Rychlé přidání zápasu (AJAX)", "admin_matches.go"},
			{"admin_set_tip", "Admin přepsal tip uživatele", "admin_matches.go"},
			{"user_tips_delete", "Smazání všech tipů uživatele", "admin_matches.go"},
			{"tip_save", "Uživatel uložil tip", "tips.go"},
			{"extra_save", "Uživatel uložil bonusovou odpověď", "extra.go"},
			{"admin_set_extra_answer", "Admin nastavil bonusovou odpověď", "admin_extra.go"},
			{"extra_answer", "Vyhodnocení bonusové otázky", "admin_extra.go"},
			{"extra_import", "Import bonusových otázek", "admin_extra.go"},
			{"team_merge", "Sloučení týmů", "admin_teams.go"},
			{"team_delete", "Smazání týmu", "admin_teams.go"},
			{"api_update_results", "Stažení výsledků z API", "admin_api_import.go"},
			{"auto_fetch_results", "Automatické stažení výsledků (cron)", "admin_api_import.go"},
			{"tz_override_set", "Nastaven TZ offset override", "admin_time.go"},
			{"tz_override_clear", "TZ offset override smazán", "admin_time.go"},
			{"undo", "Vrácení admin akce zpět", "admin_audit.go"},
		}
		RenderTemplate(w, r, tmpl, "admin/error_map.html", TemplateData{
			"User":         admin,
			"AuditActions": auditActions,
		})
	}
}
