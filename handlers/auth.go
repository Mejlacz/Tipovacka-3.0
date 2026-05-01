// handlers/auth.go — Tipovačka 2.0
// Registrace, login, logout, reset hesla.
package handlers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"tipovacka/config"
	"tipovacka/db"
	"tipovacka/middleware"
	"tipovacka/models"
)

var authTmpl *template.Template

func InitAuthTemplates(t *template.Template) {
	authTmpl = t
}

// ─── Bcrypt helpers ───────────────────────────────────────────────────────────

func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(b), err
}

func VerifyPassword(plain, hashed string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hashed), []byte(plain)) == nil
}

// ─── GET /register ────────────────────────────────────────────────────────────

func RegisterForm(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		RenderTemplate(w, r, tmpl, "auth/register.html", TemplateData{"Error": nil})
	}
}

// ─── POST /register ───────────────────────────────────────────────────────────

func RegisterSubmit(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		username := strings.TrimSpace(r.FormValue("username"))
		email := strings.TrimSpace(strings.ToLower(r.FormValue("email")))
		password := r.FormValue("password")

		renderErr := func(msg string) {
			w.WriteHeader(http.StatusBadRequest)
			RenderTemplate(w, r, tmpl, "auth/register.html", TemplateData{"Error": msg})
		}

		if len(password) < 8 {
			renderErr("Heslo musí mít alespoň 8 znaků.")
			return
		}

		ctx := context.Background()
		// Zkontroluj unikátnost username
		var existCount int
		_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE username = $1`, username).Scan(&existCount)
		if existCount > 0 {
			renderErr("Nick je již obsazen.")
			return
		}
		_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE email = $1`, email).Scan(&existCount)
		if existCount > 0 {
			renderErr("E-mail je již registrován.")
			return
		}

		hash, err := HashPassword(password)
		if err != nil {
			renderErr("Chyba serveru.")
			return
		}

		// Build INSERT dynamically — jen existující sloupce
		sql, vals := buildUserInsertSQL([]UserInsertField{
			{Col: "username", Val: username, Include: true},
			{Col: "password_hash", Val: hash, Include: true},
			{Col: "email", Val: email, Include: userCols.Email && email != ""},
			{Col: "lang", Val: "cs", Include: userCols.Lang},
			{Col: "created_at", Val: time.Now().UTC(), Include: userCols.CreatedAt},
			{Col: "is_approved", Val: false, Include: userCols.IsApproved},
		})
		var userID int
		err = db.Pool.QueryRow(ctx, sql, vals...).Scan(&userID)
		if err != nil {
			renderErr("Chyba při registraci.")
			return
		}

		middleware.SetUserID(w, r, userID)
		http.Redirect(w, r, "/pending", http.StatusSeeOther)
	}
}

// ─── GET /login ───────────────────────────────────────────────────────────────

func LoginForm(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flash := middleware.GetFlash(w, r)
		RenderTemplate(w, r, tmpl, "auth/login.html", TemplateData{
			"Error": nil,
			"Flash": flash,
		})
	}
}

// ─── POST /login ──────────────────────────────────────────────────────────────

func LoginSubmit(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		login := strings.TrimSpace(r.FormValue("email"))
		password := r.FormValue("password")

		renderErr := func(msg string) {
			w.WriteHeader(http.StatusUnauthorized)
			RenderTemplate(w, r, tmpl, "auth/login.html", TemplateData{"Error": msg})
		}

		ctx := context.Background()
		// Přihlášení emailem nebo username — použij schema-aware select
		cols, _ := buildUserSelect()
		// WHERE zahrnuje email pouze pokud sloupec existuje
		var whereClause string
		if userCols.Email {
			whereClause = "WHERE LOWER(email) = LOWER($1) OR LOWER(username) = LOWER($1)"
		} else {
			whereClause = "WHERE LOWER(username) = LOWER($1)"
		}
		u := &models.User{}
		row := db.Pool.QueryRow(ctx,
			"SELECT "+cols+" FROM users "+whereClause+" LIMIT 1", login)
		if err := scanUser(u, row); err != nil {
			log.Printf("[login] chyba pro '%s': %v", login, err)
			renderErr("Špatné přihlašovací údaje.")
			return
		}

		if !VerifyPassword(password, u.PasswordHash) {
			log.Printf("[login] špatné heslo pro '%s'", login)
			renderErr("Špatné přihlašovací údaje.")
			return
		}

		middleware.SetUserID(w, r, u.ID)
		middleware.SetLang(w, r, u.Lang)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// ─── POST /logout ─────────────────────────────────────────────────────────────

func Logout(w http.ResponseWriter, r *http.Request) {
	middleware.ClearSession(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ─── GET /pending ─────────────────────────────────────────────────────────────

func PendingApproval(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := GetCurrentUser(r)
		if u == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if u.IsApproved || u.IsAdmin {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		RenderTemplate(w, r, tmpl, "auth/pending.html", TemplateData{"User": u})
	}
}

// ─── GET /forgot-password ─────────────────────────────────────────────────────

func ForgotPasswordForm(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		RenderTemplate(w, r, tmpl, "auth/forgot_password.html", TemplateData{"Sent": false, "Error": nil})
	}
}

// ─── POST /forgot-password ────────────────────────────────────────────────────

func ForgotPasswordSubmit(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		email := strings.TrimSpace(strings.ToLower(r.FormValue("email")))

		ctx := context.Background()

		if !userCols.Email {
			// Email column neexistuje — reset hesla přes email není dostupný
			RenderTemplate(w, r, tmpl, "auth/forgot_password.html", TemplateData{"Sent": true, "Error": nil})
			return
		}

		var userID int
		var userEmail string
		err := db.Pool.QueryRow(ctx,
			`SELECT id, email FROM users WHERE email = $1 LIMIT 1`, email).Scan(&userID, &userEmail)
		if err == nil && userEmail != "" {
			// Generuj token
			tokBytes := make([]byte, 32)
			_, _ = rand.Read(tokBytes)
			token := base64.URLEncoding.EncodeToString(tokBytes)
			expires := time.Now().UTC().Add(time.Hour)

			// Zneplatni staré tokeny
			_, _ = db.Pool.Exec(ctx,
				`UPDATE password_reset_tokens SET used = true WHERE user_id = $1 AND used = false`, userID)
			_, _ = db.Pool.Exec(ctx,
				`INSERT INTO password_reset_tokens (user_id, token, expires_at, used, created_at)
				 VALUES ($1, $2, $3, false, NOW())`, userID, token, expires)

			// Odešli email s odkazem na reset hesla
			resetURL := config.AppURL + "/reset-password/" + token
			subject := "🔑 Reset hesla — Tipovačka"
			body := buildResetPasswordEmail(resetURL, config.AppURL)
			if err := sendEmailHTML(userEmail, subject, body); err != nil {
				log.Printf("[auth] Reset email chyba → %s: %v", userEmail, err)
			} else {
				log.Printf("[auth] Reset email odeslán → %s", userEmail)
			}
		}

		// Vždy zobrazíme "e-mail byl odeslán"
		RenderTemplate(w, r, tmpl, "auth/forgot_password.html", TemplateData{"Sent": true, "Error": nil})
	}
}

// buildResetPasswordEmail sestaví HTML tělo emailu pro reset hesla.
func buildResetPasswordEmail(resetURL, appURL string) string {
	return fmt.Sprintf(`<html><body style="font-family:sans-serif;max-width:500px;margin:auto;padding:1rem;background:#f0f4f8">
<div style="background:#131f2e;color:#fff;padding:1rem 1.5rem;border-radius:8px 8px 0 0">
  <h2 style="margin:0;font-size:1.1rem">🔑 Reset hesla</h2>
</div>
<div style="background:#fff;padding:1.5rem;border-radius:0 0 8px 8px;border:1px solid #dde3ea;border-top:none">
  <p style="margin-top:0">Někdo požádal o reset hesla k tvému účtu v Tipovačce. Pokud to nebyl ty, tento email ignoruj — nic se nestane.</p>
  <div style="text-align:center;margin:1.5rem 0">
    <a href="%s" style="background:#6366f1;color:#fff;text-decoration:none;padding:.65rem 1.8rem;border-radius:6px;font-weight:700;font-size:.95rem">
      Nastavit nové heslo →
    </a>
  </div>
  <p style="color:#64748b;font-size:.82rem;margin:0">
    Odkaz je platný <strong>1 hodinu</strong>. Po vypršení bude potřeba požádat o nový.
  </p>
  <p style="color:#94a3b8;font-size:.78rem;text-align:center;margin-top:1.2rem;border-top:1px solid #e2e8f0;padding-top:.75rem">
    Tipovačka · <a href="%s" style="color:#64748b">%s</a>
  </p>
</div>
</body></html>`, resetURL, appURL, appURL)
}

// ─── GET /reset-password/{token} ─────────────────────────────────────────────

func ResetPasswordForm(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Chi router
		token := r.PathValue("token")
		ctx := context.Background()
		var id int
		err := db.Pool.QueryRow(ctx,
			`SELECT id FROM password_reset_tokens WHERE token = $1 AND used = false AND expires_at > NOW()`,
			token).Scan(&id)
		invalid := err != nil
		RenderTemplate(w, r, tmpl, "auth/reset_password.html", TemplateData{
			"Token":   token,
			"Invalid": invalid,
			"Error":   nil,
		})
	}
}

// ─── POST /reset-password/{token} ────────────────────────────────────────────

func ResetPasswordSubmit(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.PathValue("token")
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		password := r.FormValue("password")
		password2 := r.FormValue("password2")

		ctx := context.Background()
		var recID, userID int
		err := db.Pool.QueryRow(ctx,
			`SELECT id, user_id FROM password_reset_tokens WHERE token = $1 AND used = false AND expires_at > NOW()`,
			token).Scan(&recID, &userID)

		renderErr := func(msg string) {
			RenderTemplate(w, r, tmpl, "auth/reset_password.html", TemplateData{
				"Token": token, "Invalid": err != nil, "Error": msg,
			})
		}

		if err != nil {
			renderErr("")
			return
		}
		if len(password) < 8 {
			renderErr("Heslo musí mít alespoň 8 znaků.")
			return
		}
		if password != password2 {
			renderErr("Hesla se neshodují.")
			return
		}

		hash, err := HashPassword(password)
		if err != nil {
			renderErr("Chyba serveru.")
			return
		}

		_, _ = db.Pool.Exec(ctx, `UPDATE users SET password_hash = $1 WHERE id = $2`, hash, userID)
		_, _ = db.Pool.Exec(ctx, `UPDATE password_reset_tokens SET used = true WHERE id = $1`, recID)

		middleware.SetFlash(w, r, "ok", "Heslo bylo úspěšně změněno.")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}
