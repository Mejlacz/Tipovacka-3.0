// middleware/session.go — Tipovačka 2.0
// Session middleware + CSRF ochrana pomocí gorilla/sessions.
package middleware

import (
	"net/http"

	"github.com/gorilla/sessions"
	"tipovacka/config"
)

var Store *sessions.CookieStore

func InitStore() {
	Store = sessions.NewCookieStore([]byte(config.SecretKey))
	Store.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   config.SessionMaxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// Secure: true — zapnout v produkci (HTTPS)
		Secure: false,
	}
}

// GetSession vrátí session pro daný request. Chyby ignoruje (prázdná session).
func GetSession(r *http.Request) *sessions.Session {
	sess, _ := Store.Get(r, "session")
	return sess
}

// GetUserID vrátí user_id ze session nebo 0.
func GetUserID(r *http.Request) int {
	sess := GetSession(r)
	if v, ok := sess.Values["user_id"]; ok {
		if id, ok := v.(int); ok {
			return id
		}
	}
	return 0
}

// SetUserID uloží user_id do session.
func SetUserID(w http.ResponseWriter, r *http.Request, userID int) {
	sess := GetSession(r)
	sess.Values["user_id"] = userID
	_ = sess.Save(r, w)
}

// ClearSession smaže celou session.
func ClearSession(w http.ResponseWriter, r *http.Request) {
	sess := GetSession(r)
	sess.Options.MaxAge = -1
	_ = sess.Save(r, w)
}

// GetFlash vrátí a smaže flash zprávu ze session.
func GetFlash(w http.ResponseWriter, r *http.Request) map[string]string {
	sess := GetSession(r)
	if v, ok := sess.Values["flash"]; ok {
		delete(sess.Values, "flash")
		_ = sess.Save(r, w)
		if m, ok := v.(map[string]string); ok {
			return m
		}
	}
	return nil
}

// SetFlash uloží flash zprávu do session.
func SetFlash(w http.ResponseWriter, r *http.Request, typ, msg string) {
	sess := GetSession(r)
	sess.Values["flash"] = map[string]string{"type": typ, "msg": msg}
	_ = sess.Save(r, w)
}

// GetCSRFToken vrátí nebo vygeneruje CSRF token v session.
func GetCSRFToken(w http.ResponseWriter, r *http.Request) string {
	sess := GetSession(r)
	if v, ok := sess.Values["csrf_token"]; ok {
		if t, ok := v.(string); ok && t != "" {
			return t
		}
	}
	tok := config.SecretKey[:32] // jednoduché řešení — v produkci použij crypto/rand
	sess.Values["csrf_token"] = tok
	_ = sess.Save(r, w)
	return tok
}

// GetLang vrátí jazyk ze session (cs nebo en).
func GetLang(r *http.Request) string {
	sess := GetSession(r)
	if v, ok := sess.Values["lang"]; ok {
		if l, ok := v.(string); ok && (l == "cs" || l == "en") {
			return l
		}
	}
	return "cs"
}

// SetLang uloží jazyk do session.
func SetLang(w http.ResponseWriter, r *http.Request, lang string) {
	sess := GetSession(r)
	sess.Values["lang"] = lang
	_ = sess.Save(r, w)
}

// CSRFMiddleware — kontroluje CSRF token pro POST/PUT/DELETE/PATCH.
func CSRFMiddleware(next http.Handler) http.Handler {
	safeMethods := map[string]bool{"GET": true, "HEAD": true, "OPTIONS": true, "TRACE": true}
	exemptPaths := map[string]bool{
		"/profile/push-subscribe":   true,
		"/profile/push-unsubscribe": true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Zajisti existenci tokenu
		tok := GetCSRFToken(w, r)

		if !safeMethods[r.Method] && !exemptPaths[r.URL.Path] {
			submitted := r.Header.Get("X-CSRF-Token")
			if submitted == "" {
				if err := r.ParseForm(); err == nil {
					submitted = r.FormValue("_csrf")
				}
			}
			if submitted != tok {
				http.Error(w, "CSRF chyba — obnovte stránku a zkuste znovu.", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
