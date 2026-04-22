// handlers/admin_keepalive.go — Tipovačka 2.0
// Admin stránka pro ovládání GitHub Actions keepalive workflow.
package handlers

import (
	"encoding/json"
	"html/template"
	"io"
	"log"
	"net/http"
	"time"

	"tipovacka/config"
	"tipovacka/middleware"
)

// ghKeepalive zavolá GitHub REST API pro keepalive workflow.
// method: "GET", "PUT"; action: "" pro stav, "enable"/"disable" pro akce.
// Vrátí nil při chybě nebo chybějícím PAT.
func ghKeepalive(method, action string) map[string]interface{} {
	if config.GithubPAT == "" || config.GithubRepo == "" {
		return nil
	}
	url := "https://api.github.com/repos/" + config.GithubRepo + "/actions/workflows/keepalive.yml"
	if action != "" {
		url += "/" + action
	}

	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		log.Printf("[keepalive] http.NewRequest error: %v", err)
		return nil
	}
	req.Header.Set("Authorization", "token "+config.GithubPAT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[keepalive] request error: %v", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		log.Printf("[keepalive] GitHub API returned %d", resp.StatusCode)
		return nil
	}

	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		return map[string]interface{}{}
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil
	}
	return result
}

// getWorkflowState vrátí "active", "disabled_manually" nebo "" (PAT chybí/chyba).
func getWorkflowState() string {
	data := ghKeepalive("GET", "")
	if data == nil {
		return ""
	}
	if s, ok := data["state"].(string); ok {
		return s
	}
	return ""
}

// GET /admin/keepalive
func AdminKeepalive(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}

		state := getWorkflowState()
		flash := middleware.GetFlash(w, r)

		RenderTemplate(w, r, tmpl, "admin/keepalive.html", TemplateData{
			"User":       admin,
			"State":      state,
			"PatSet":     config.GithubPAT != "",
			"GithubRepo": config.GithubRepo,
			"Workflow":   "keepalive.yml",
			"Flash":      flash,
		})
	}
}

// POST /admin/keepalive/enable
func AdminKeepaliveEnable(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}

	result := ghKeepalive("PUT", "enable")
	if result != nil {
		middleware.SetFlash(w, r, "ok", "Keep Alive zapnut — workflow běží každých 10 minut.")
	} else {
		middleware.SetFlash(w, r, "error", "Nepodařilo se zapnout workflow. Zkontroluj GITHUB_PAT.")
	}

	http.Redirect(w, r, "/admin/keepalive", http.StatusSeeOther)
}

// POST /admin/keepalive/disable
func AdminKeepaliveDisable(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}

	result := ghKeepalive("PUT", "disable")
	if result != nil {
		middleware.SetFlash(w, r, "ok", "Keep Alive vypnut — Koyeb může přejít do spánku.")
	} else {
		middleware.SetFlash(w, r, "error", "Nepodařilo se vypnout workflow. Zkontroluj GITHUB_PAT.")
	}

	http.Redirect(w, r, "/admin/keepalive", http.StatusSeeOther)
}
