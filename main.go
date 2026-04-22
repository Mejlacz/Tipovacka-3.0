// main.go — Tipovačka 2.0
// Vstupní bod Go aplikace: chi router, middleware, všechny routes.
package main

import (
	"context"
	"encoding/gob"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"tipovacka/config"
	"tipovacka/db"
	"tipovacka/handlers"
	"tipovacka/middleware"
	"tipovacka/scheduler"
)

func init() {
	// gorilla/sessions uses gob to encode session values.
	// Register all types that are stored as interface{} in the session.
	gob.Register(map[string]string{}) // flash messages
	gob.Register(map[string]interface{}{})
}

func main() {
	// Connect to DB (config loads via init() automatically)
	db.Init()
	defer db.Close()

	// Detect which optional columns exist in the users table at runtime
	handlers.InitUserSchema()

	// Ensure admin exists (schema-aware — uses only columns that exist in DB)
	handlers.EnsureAdmin(config.AdminUsername, config.AdminPassword)

	// Init Neon sync tables
	handlers.InitNeonSyncTables()

	// Parse all templates
	tmpl := parseTemplates()

	// Init session store
	middleware.InitStore()

	// Spusť scheduler na pozadí
	ctx := context.Background()
	go scheduler.Start(ctx, db.Pool)

	// Set up router
	r := chi.NewRouter()

	// ── Middleware ──────────────────────────────────────────────────────────
	r.Use(chimw.Logger)
	r.Use(chimw.Recoverer)
	r.Use(middleware.CSRFMiddleware)

	// ── Static files ────────────────────────────────────────────────────────
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	// ── System endpoints ────────────────────────────────────────────────────
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","app":"Tipovacka 2.0"}`))
	})

	r.Get("/service-worker.js", serviceWorkerHandler)
	r.Get("/manifest.json", manifestHandler)
	r.Get("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/icon.png", http.StatusMovedPermanently)
	})

	r.Get("/set-lang/{lang}", setLangHandler)

	// ── Auth ─────────────────────────────────────────────────────────────────
	r.Get("/register", handlers.RegisterForm(tmpl))
	r.Post("/register", handlers.RegisterSubmit(tmpl))
	r.Get("/login", handlers.LoginForm(tmpl))
	r.Post("/login", handlers.LoginSubmit(tmpl))
	r.Post("/logout", handlers.Logout)
	r.Get("/pending", handlers.PendingApproval(tmpl))
	r.Get("/forgot-password", handlers.ForgotPasswordForm(tmpl))
	r.Post("/forgot-password", handlers.ForgotPasswordSubmit(tmpl))
	r.Get("/reset-password/{token}", handlers.ResetPasswordForm(tmpl))
	r.Post("/reset-password/{token}", handlers.ResetPasswordSubmit(tmpl))

	// ── Tips ─────────────────────────────────────────────────────────────────
	r.Get("/", handlers.Index(tmpl))
	r.Get("/competition/{competition_id}", handlers.CompetitionDetail(tmpl))
	r.Get("/round/{round_id}", handlers.RoundRedirect)
	r.Post("/tip/submit", handlers.SubmitTip)
	r.Post("/tip/submit-ajax", handlers.SubmitTipAjax)

	// ── Leaderboard ───────────────────────────────────────────────────────────
	r.Get("/leaderboard", handlers.Leaderboard(tmpl))
	r.Get("/leaderboard/{competition_id}", func(w http.ResponseWriter, r *http.Request) {
		compID := chi.URLParam(r, "competition_id")
		http.Redirect(w, r, "/leaderboard?competition_id="+compID, http.StatusMovedPermanently)
	})

	// ── Archive ───────────────────────────────────────────────────────────────
	r.Get("/archive", handlers.ArchiveIndex(tmpl))
	r.Get("/archive/competition/{competition_id}", handlers.ArchiveCompetition(tmpl))
	r.Get("/archive/round/{round_id}", handlers.ArchiveRoundRedirect)

	// ── Extra ─────────────────────────────────────────────────────────────────
	r.Get("/extra", handlers.ExtraView(tmpl))
	r.Post("/extra/save-ajax", handlers.ExtraSaveAjax)

	// ── Profile ───────────────────────────────────────────────────────────────
	r.Get("/profile", handlers.ProfilePage(tmpl))
	r.Post("/profile/update", handlers.ProfileUpdate)
	r.Post("/profile/password", handlers.ProfilePassword)
	r.Post("/profile/notifications", handlers.ProfileNotifications)
	r.Post("/profile/push-subscribe", handlers.ProfilePushSubscribe)
	r.Post("/profile/push-unsubscribe", handlers.ProfilePushUnsubscribe)
	r.Post("/profile/push-test", handlers.ProfilePushTest)
	r.Post("/profile/push-delete/{sub_id}", handlers.ProfilePushDelete)
	r.Post("/profile/avatar", handlers.ProfileAvatarUpload)
	r.Post("/profile/avatar/delete", handlers.ProfileAvatarDelete)

	// ── Stats ─────────────────────────────────────────────────────────────────
	r.Get("/stats", handlers.StatsRedirect)
	r.Get("/stats/{competition_id}", handlers.StatsDetail(tmpl))
	r.Get("/stats/{competition_id}/extended", handlers.StatsExtended(tmpl))
	r.Get("/stats/{competition_id}/vs/{other_user_id}", handlers.StatsVs(tmpl))

	// ── Achievements ──────────────────────────────────────────────────────────
	r.Get("/achievements", handlers.AchievementsPage(tmpl))

	// ── Pages (Pravidla) ──────────────────────────────────────────────────────
	r.Get("/pravidla", handlers.PravidlaPage(tmpl))

	// ── Admin ─────────────────────────────────────────────────────────────────
	r.Get("/admin", handlers.AdminDashboard(tmpl))
	r.Get("/admin/competitions", handlers.AdminCompetitionsList(tmpl))
	r.Get("/admin/competitions/new", handlers.AdminCompetitionNewForm(tmpl))
	r.Post("/admin/competitions/new", handlers.AdminCompetitionCreate)
	r.Get("/admin/competitions/{competition_id}/edit", handlers.AdminCompetitionEditForm(tmpl))
	r.Post("/admin/competitions/{competition_id}/edit", handlers.AdminCompetitionEditSubmit)
	r.Post("/admin/competitions/{competition_id}/toggle", handlers.AdminCompetitionToggle)
	r.Post("/admin/competitions/{competition_id}/delete", handlers.AdminCompetitionDelete)
	r.Post("/admin/competitions/{competition_id}/sort-order", handlers.AdminCompetitionSortOrder)

	// Admin users
	r.Get("/admin/users", handlers.AdminUsersList(tmpl))
	r.Get("/admin/users/new", handlers.AdminUserNewForm(tmpl))
	r.Post("/admin/users/new", handlers.AdminUserNewSubmit(tmpl))
	r.Get("/admin/users/{user_id}/edit", handlers.AdminUserEditForm(tmpl))
	r.Post("/admin/users/{user_id}/edit", handlers.AdminUserEditSubmit(tmpl))
	r.Post("/admin/users/{user_id}/approve", handlers.AdminUserApprove)
	r.Post("/admin/users/{user_id}/block", handlers.AdminUserBlock)
	r.Post("/admin/users/{user_id}/unblock", handlers.AdminUserUnblock)
	r.Post("/admin/users/{user_id}/toggle-admin", handlers.AdminUserToggleAdmin)
	r.Post("/admin/users/{user_id}/toggle-owner", handlers.AdminUserToggleOwner)
	r.Post("/admin/users/{user_id}/toggle-inactive", handlers.AdminUserToggleInactive)
	r.Post("/admin/users/{user_id}/toggle-hidden", handlers.AdminUserToggleHidden)
	r.Post("/admin/users/{user_id}/delete", handlers.AdminUserDelete)
	r.Post("/admin/users/{user_id}/reset-password", handlers.AdminUserResetPassword)
	r.Post("/admin/users/{user_id}/set-role", handlers.AdminUserSetRole)

	// Admin manual page
	r.Get("/admin/manual", handlers.AdminManual(tmpl))

	// Admin rounds
	r.Get("/admin/competitions/{competition_id}/rounds", handlers.AdminRoundsList(tmpl))
	r.Post("/admin/competitions/{competition_id}/rounds/new", handlers.AdminRoundCreate)
	r.Post("/admin/rounds/{round_id}/edit", handlers.AdminRoundEdit)
	r.Post("/admin/rounds/{round_id}/toggle", handlers.AdminRoundToggle)
	r.Post("/admin/rounds/{round_id}/notify-new", handlers.AdminRoundNotifyNew)

	// Admin matches
	r.Get("/admin/rounds/{round_id}/matches", handlers.AdminMatchesList(tmpl))
	r.Post("/admin/rounds/{round_id}/matches/new", handlers.AdminMatchCreate)
	r.Post("/admin/matches/{match_id}/edit", handlers.AdminMatchEdit)
	r.Post("/admin/matches/{match_id}/set-result", handlers.AdminMatchSetResult)
	r.Post("/admin/matches/{match_id}/clear-result", handlers.AdminMatchClearResult)
	r.Post("/admin/matches/{match_id}/delete", handlers.AdminMatchDelete)
	r.Post("/admin/matches/{match_id}/set-date", handlers.AdminMatchSetDate)
	r.Post("/admin/matches/{match_id}/set-tip", handlers.AdminSetTip)
	r.Post("/admin/tips/set-ajax", handlers.AdminSetTip) // alias (Python URL)
	r.Get("/admin/unscored", handlers.AdminUnscored(tmpl))
	r.Get("/admin/unscored-count", handlers.AdminUnscoredCount)
	r.Get("/admin/rounds/{round_id}/bulk-results", handlers.AdminBulkResultsForm(tmpl))
	r.Post("/admin/rounds/{round_id}/bulk-results", handlers.AdminBulkResultsSubmit)
	// Global bulk results view (all active competitions)
	r.Get("/admin/results", handlers.AdminBulkResultsForm(tmpl))
	r.Post("/admin/results", handlers.AdminBulkResultsSubmit)
	r.Post("/admin/competitions/{competition_id}/add-match", handlers.AdminQuickAddMatchAjax)

	// Admin teams
	r.Get("/admin/teams", handlers.AdminTeamsList(tmpl))
	r.Post("/admin/teams/new", handlers.AdminTeamCreate)
	r.Post("/admin/teams/{team_id}/edit", handlers.AdminTeamEdit)
	r.Post("/admin/teams/{team_id}/delete", handlers.AdminTeamDelete)
	r.Post("/admin/teams/import-csv", handlers.AdminTeamsImportCSV)
	r.Get("/admin/competitions/{competition_id}/teams", handlers.AdminCompetitionTeamsForm(tmpl))
	r.Post("/admin/competitions/{competition_id}/teams", handlers.AdminCompetitionTeamsSave)

	// Admin OCR (text paste import)
	r.Get("/admin/ocr", handlers.AdminOCRForm(tmpl))
	r.Post("/admin/ocr/parse", handlers.AdminOCRParse(tmpl))
	r.Post("/admin/ocr/confirm", handlers.AdminOCRConfirm)
	r.Post("/admin/ocr/cancel", handlers.AdminOCRCancel)

	// Admin API import
	r.Get("/admin/api/rounds", handlers.AdminAPIRounds)
	r.Get("/admin/api/preview", handlers.AdminAPIPreview)
	r.Post("/admin/api/import", handlers.AdminAPIImport)

	// Admin audit
	r.Get("/admin/history", handlers.AdminHistory(tmpl))
	r.Get("/admin/audit", handlers.AdminAuditLog(tmpl))
	r.Post("/admin/audit/{entry_id}/undo", handlers.AdminAuditUndo)

	// Admin pravidla
	r.Get("/admin/pravidla/edit", handlers.AdminPravidlaEditForm(tmpl))
	r.Post("/admin/pravidla/edit", handlers.AdminPravidlaEditSubmit)
	r.Post("/admin/pravidla/reset", handlers.AdminPravidlaReset)

	// Admin IO (import/export overview)
	r.Get("/admin/io", handlers.AdminIO(tmpl))

	// Admin CSV exports
	r.Get("/admin/tips/{competition_id}/export", handlers.AdminTipsExportCSV)
	r.Get("/admin/export/csv", handlers.AdminGeneralExportCSV)

	// Admin XLSX exports
	r.Get("/admin/export/xlsx", handlers.AdminGeneralExportXLSX)

	// Admin health dashboard
	r.Get("/admin/health-dashboard", handlers.AdminHealthDashboard(tmpl))

	// Keep Alive
	r.Get("/admin/keepalive", handlers.AdminKeepalive(tmpl))
	r.Post("/admin/keepalive/enable", handlers.AdminKeepaliveEnable)
	r.Post("/admin/keepalive/disable", handlers.AdminKeepaliveDisable)

	// Neon Sync
	r.Get("/admin/neon-sync", handlers.AdminNeonSync(tmpl))
	r.Post("/admin/neon-sync/run", handlers.AdminNeonSyncRun)
	r.Post("/admin/neon-sync/settings", handlers.AdminNeonSyncSettings)

	// Hromadný email
	r.Get("/admin/email", handlers.AdminEmailForm(tmpl))
	r.Post("/admin/email/send", handlers.AdminEmailSend(tmpl))

	// User merge
	r.Get("/admin/users/merge", handlers.AdminUserMergeForm(tmpl))
	r.Post("/admin/users/merge", handlers.AdminUserMerge)

	// User import
	r.Get("/admin/users/import", handlers.AdminUserImportForm(tmpl))
	r.Post("/admin/users/import", handlers.AdminUserImportSubmit)

	// Admin extra questions
	r.Get("/admin/extra/{competition_id}/questions/new", handlers.AdminExtraQuestionNewForm(tmpl))
	r.Post("/admin/extra/{competition_id}/questions/new", handlers.AdminExtraQuestionNewSubmit)
	r.Post("/admin/extra/questions/{question_id}/delete", handlers.AdminExtraQuestionDelete)
	r.Post("/admin/extra/questions/{question_id}/toggle-close", handlers.AdminExtraQuestionToggleClose)
	r.Get("/admin/extra/{competition_id}/answers", handlers.AdminExtraAnswersView(tmpl))
	r.Post("/admin/extra/{competition_id}/answers", handlers.AdminExtraAnswersSave)
	r.Post("/admin/extra/{competition_id}/auto-evaluate", handlers.AdminExtraAutoEvaluate)
	r.Get("/admin/extra/{competition_id}/export", handlers.AdminExtraExport)
	r.Post("/admin/extra/answers/set-ajax", handlers.AdminSetExtraAnswerAjax)

	// Admin groups (Owner only)
	r.Get("/admin/groups", handlers.AdminGroupsList(tmpl))
	r.Post("/admin/groups/new", handlers.AdminGroupsNew)
	r.Post("/admin/groups/{group_id}/delete", handlers.AdminGroupsDelete)
	r.Post("/admin/groups/{group_id}/toggle-hidden", handlers.AdminGroupsToggleHidden)
	r.Post("/admin/groups/{group_id}/add-member", handlers.AdminGroupsAddMember)
	r.Post("/admin/groups/{group_id}/remove-member/{user_id}", handlers.AdminGroupsRemoveMember)

	// ── Start server ─────────────────────────────────────────────────────────
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	fmt.Printf("[Tipovačka 3.0] Listening on %s\n", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatal(err)
	}
}

// ensureAdmin is now handled by handlers.EnsureAdmin (schema-aware version).

// parseTemplates loads all templates from the templates/ directory recursively.
// Template names use forward-slash relative paths, e.g. "auth/login.html".
func parseTemplates() *template.Template {
	allFiles := globRecursive("templates", "*.html")
	t := template.New("root").Funcs(templateFuncs())
	for _, f := range allFiles {
		content, err := os.ReadFile(f)
		if err != nil {
			log.Printf("[templates] Cannot read %s: %v", f, err)
			continue
		}
		// Relativní cesta od templates/ — použij jako název šablony
		rel, err := filepath.Rel("templates", f)
		if err != nil {
			rel = filepath.Base(f)
		}
		name := filepath.ToSlash(rel) // vždy lomítka (i na Windows)
		_, err = t.New(name).Parse(string(content))
		if err != nil {
			log.Printf("[templates] Parse error in %s: %v", name, err)
		}
	}
	return t
}

func globRecursive(root, pattern string) []string {
	var results []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		matched, _ := filepath.Match(pattern, filepath.Base(path))
		if matched {
			results = append(results, path)
		}
		return nil
	})
	return results
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"safeHTML": func(s string) template.HTML { return template.HTML(s) },
		"add":      func(a, b int) int { return a + b },
		"sub":      func(a, b int) int { return a - b },
		"mul":      func(a, b int) int { return a * b },
		// intVal dereferences *int safely; returns 0 if nil
		"intVal": func(p *int) int {
			if p == nil {
				return 0
			}
			return *p
		},
		// strVal dereferences *string safely; returns "" if nil
		"strVal": func(p *string) string {
			if p == nil {
				return ""
			}
			return *p
		},
		// isNilInt reports whether a *int pointer is nil
		"isNilInt": func(p *int) bool { return p == nil },
		// coalesce returns b if a is nil or empty, otherwise dereferences a
		"coalesce": func(a *string, b string) string {
			if a != nil && *a != "" {
				return *a
			}
			return b
		},
		// absInt returns the absolute value of an int
		"absInt": func(n int) int {
			if n < 0 {
				return -n
			}
			return n
		},
		// sameWinner checks if tipH:tipA predicts the same outcome (home/draw/away) as matchH:matchA
		"sameWinner": func(tipH, tipA, matchH, matchA int) bool {
			tipRes := 0
			if tipH > tipA {
				tipRes = 1
			} else if tipA > tipH {
				tipRes = -1
			}
			matchRes := 0
			if matchH > matchA {
				matchRes = 1
			} else if matchA > matchH {
				matchRes = -1
			}
			return tipRes == matchRes
		},
		// truncate shortens a string to max n runes and adds "…" if longer
		"truncate": func(s string, n int) string {
			r := []rune(s)
			if len(r) <= n {
				return s
			}
			return string(r[:n]) + "…"
		},
	}
}

func setLangHandler(w http.ResponseWriter, r *http.Request) {
	lang := chi.URLParam(r, "lang")
	if lang == "cs" || lang == "en" {
		sess := middleware.GetSession(r)
		sess.Values["lang"] = lang
		_ = sess.Save(r, w)
		// Also update in DB if logged in
		user := handlers.GetCurrentUser(r)
		if user != nil {
			_, _ = db.Pool.Exec(context.Background(),
				`UPDATE users SET lang=$1 WHERE id=$2`, lang, user.ID)
		}
	}
	referer := r.Header.Get("Referer")
	if referer == "" {
		referer = "/"
	}
	http.Redirect(w, r, referer, http.StatusSeeOther)
}

const serviceWorkerContent = `/* Tipovačka 3.0 — PWA service worker */
const CACHE = 'tipovacka-pwa-v4';

self.addEventListener('install', e => { self.skipWaiting(); });

self.addEventListener('activate', e => {
  e.waitUntil(
    caches.keys().then(keys =>
      Promise.all(keys.filter(k => k !== CACHE).map(k => caches.delete(k)))
    ).then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', e => {
  if (e.request.url.includes('/static/') || e.request.url.includes('/icon.png')) {
    e.respondWith(
      caches.open(CACHE).then(cache =>
        cache.match(e.request).then(cached =>
          cached || fetch(e.request).then(resp => {
            if (resp.ok) cache.put(e.request, resp.clone());
            return resp;
          })
        )
      )
    );
    return;
  }
  e.respondWith(fetch(e.request).catch(() => caches.match(e.request)));
});

self.addEventListener('push', e => {
  let d = {};
  try { d = e.data ? e.data.json() : {}; } catch(err) { d = {}; }
  const title = d.title || 'Tipovačka';
  const options = {
    body: d.body || '',
    icon: '/static/icon.png',
    data: { url: d.url || '/' },
  };
  e.waitUntil(self.registration.showNotification(title, options));
});

self.addEventListener('notificationclick', e => {
  e.notification.close();
  e.waitUntil(clients.openWindow(e.notification.data.url || '/'));
});
`

func serviceWorkerHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write([]byte(serviceWorkerContent))
}

func manifestHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write([]byte(`{
  "name": "Tipovačka 3.0",
  "short_name": "Tipovačka",
  "description": "Tipuj fotbalové zápasy se přáteli",
  "start_url": "/",
  "scope": "/",
  "display": "standalone",
  "background_color": "#0a1628",
  "theme_color": "#131f2e",
  "orientation": "any",
  "icons": [
    {"src": "/static/icon.png", "sizes": "192x192", "type": "image/png"},
    {"src": "/static/icon.png", "sizes": "512x512", "type": "image/png", "purpose": "any maskable"}
  ]
}`))
}
