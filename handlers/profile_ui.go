// handlers/profile_ui.go — Tipovačka 2.0
// Správa vzhledu uživatelského rozhraní (barvy tipů, font, akcent).
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strings"

	"tipovacka/db"
	"tipovacka/middleware"
)

// ── UISettings ────────────────────────────────────────────────────────────────

// UISettings drží uživatelská nastavení vzhledu uložená jako JSON v users.ui_settings.
type UISettings struct {
	ExactBg  string `json:"exact_bg,omitempty"`  // barva buňky přesného tipu (3 b)
	WinnerBg string `json:"winner_bg,omitempty"` // barva buňky správného vítěze (1 b)
	MissBg   string `json:"miss_bg,omitempty"`   // barva buňky špatného tipu (0 b)
	Accent   string `json:"accent,omitempty"`    // barva akcentu (tlačítka, linky)
	RowHL    string `json:"row_hl,omitempty"`    // barva zvýraznění vlastního řádku
	Font     string `json:"font,omitempty"`      // klíč fontu
}

var hexColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{3,8}$`)

func sanitizeColor(s string) string {
	s = strings.TrimSpace(s)
	if hexColorRe.MatchString(s) {
		return strings.ToLower(s)
	}
	return ""
}

// FontOptions je seznam dostupných fontů pro výběr.
var FontOptions = []struct {
	Key   string
	Label string
	Stack string
}{
	{"system",  "System (výchozí)",   "system-ui, -apple-system, sans-serif"},
	{"segoe",   "Segoe UI",           "'Segoe UI', Arial, sans-serif"},
	{"georgia", "Georgia (serif)",    "Georgia, 'Times New Roman', serif"},
	{"mono",    "Monospace",          "Consolas, 'Courier New', monospace"},
	{"verdana", "Verdana",            "Verdana, Geneva, sans-serif"},
}

func fontStack(key string) string {
	for _, f := range FontOptions {
		if f.Key == key {
			return f.Stack
		}
	}
	return ""
}

// BuildUICSS generuje CSS proměnné ze uživatelských nastavení.
// Výstup je template.CSS (bezpečné pro vložení do <style>).
func BuildUICSS(raw *string) template.CSS {
	if raw == nil || *raw == "" {
		return ""
	}
	var s UISettings
	if err := json.Unmarshal([]byte(*raw), &s); err != nil {
		return ""
	}
	var vars []string
	if c := sanitizeColor(s.ExactBg); c != "" {
		vars = append(vars, fmt.Sprintf("--ui-exact-bg:%s", c))
	}
	if c := sanitizeColor(s.WinnerBg); c != "" {
		vars = append(vars, fmt.Sprintf("--ui-winner-bg:%s", c))
	}
	if c := sanitizeColor(s.MissBg); c != "" {
		vars = append(vars, fmt.Sprintf("--ui-miss-bg:%s", c))
	}
	if c := sanitizeColor(s.Accent); c != "" {
		vars = append(vars, fmt.Sprintf("--accent:%s;--accent2:%s;--ui-accent:%s", c, c, c))
	}
	if c := sanitizeColor(s.RowHL); c != "" {
		vars = append(vars, fmt.Sprintf("--ui-row-hl:%s", c))
	}
	var sb strings.Builder
	if len(vars) > 0 {
		sb.WriteString(":root{")
		sb.WriteString(strings.Join(vars, ";"))
		sb.WriteString("}")
	}
	if stack := fontStack(s.Font); stack != "" {
		sb.WriteString(fmt.Sprintf("body,input,select,button,textarea{font-family:%s}", stack))
	}
	return template.CSS(sb.String())
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// GET /profile/appearance
func ProfileAppearance(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := RequireApproved(w, r)
		if user == nil {
			return
		}
		var s UISettings
		if user.UISettings != nil && *user.UISettings != "" {
			_ = json.Unmarshal([]byte(*user.UISettings), &s)
		}
		RenderTemplate(w, r, tmpl, "appearance.html", TemplateData{
			"User":        user,
			"Settings":    s,
			"FontOptions": FontOptions,
			"Flash":       middleware.GetFlash(w, r),
		})
	}
}

// POST /profile/appearance
func ProfileAppearanceSave(w http.ResponseWriter, r *http.Request) {
	user := RequireApproved(w, r)
	if user == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	s := UISettings{
		ExactBg:  sanitizeColor(r.FormValue("exact_bg")),
		WinnerBg: sanitizeColor(r.FormValue("winner_bg")),
		MissBg:   sanitizeColor(r.FormValue("miss_bg")),
		Accent:   sanitizeColor(r.FormValue("accent")),
		RowHL:    sanitizeColor(r.FormValue("row_hl")),
	}
	font := r.FormValue("font")
	if fontStack(font) != "" {
		s.Font = font
	}
	raw, _ := json.Marshal(s)
	_, _ = db.Pool.Exec(context.Background(),
		`UPDATE users SET ui_settings=$1 WHERE id=$2`, string(raw), user.ID)
	middleware.SetFlash(w, r, "ok", "Vzhled uložen.")
	http.Redirect(w, r, "/profile/appearance", http.StatusSeeOther)
}

// POST /profile/appearance/reset
func ProfileAppearanceReset(w http.ResponseWriter, r *http.Request) {
	user := RequireApproved(w, r)
	if user == nil {
		return
	}
	_, _ = db.Pool.Exec(context.Background(),
		`UPDATE users SET ui_settings=NULL WHERE id=$1`, user.ID)
	middleware.SetFlash(w, r, "ok", "Vzhled obnoven na výchozí.")
	http.Redirect(w, r, "/profile/appearance", http.StatusSeeOther)
}
