// handlers/admin_user_import.go — Tipovačka 2.0
// Import uživatelů z CSV souboru.
// Sloupce CSV (s nebo bez hlavičky): username, email, first_name, last_name, role
package handlers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/csv"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"strings"

	"tipovacka/db"
	"tipovacka/middleware"
)

// randomPassword vygeneruje náhodné heslo pro nové uživatele.
func randomPassword() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return base64.URLEncoding.EncodeToString(b)[:24]
}

// GET /admin/users/import
func AdminUserImportForm(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		flash := middleware.GetFlash(w, r)
		RenderTemplate(w, r, tmpl, "admin/user_import.html", TemplateData{
			"User":  admin,
			"Flash": flash,
		})
	}
}

// POST /admin/users/import
func AdminUserImportSubmit(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}

	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	file, _, err := r.FormFile("csvfile")
	if err != nil {
		middleware.SetFlash(w, r, "error", "CSV soubor nebyl nahrán.")
		http.Redirect(w, r, "/admin/users/import", http.StatusSeeOther)
		return
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.TrimLeadingSpace = true
	reader.FieldsPerRecord = -1 // proměnný počet sloupců

	var created, updated, skipped int
	ctx := context.Background()
	firstRow := true

	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("[user_import] CSV read error: %v", err)
			continue
		}
		if len(record) < 1 {
			continue
		}

		// Normalizuj sloupce
		for i := range record {
			record[i] = strings.TrimSpace(record[i])
		}

		// Přeskoč header řádek (pokud první sloupec je "username" nebo "Username")
		if firstRow {
			firstRow = false
			lower := strings.ToLower(record[0])
			if lower == "username" || lower == "user" || lower == "jméno" {
				continue
			}
		}

		username := ""
		email := ""
		firstName := ""
		lastName := ""
		role := "user"

		if len(record) > 0 {
			username = record[0]
		}
		if len(record) > 1 {
			email = record[1]
		}
		if len(record) > 2 {
			firstName = record[2]
		}
		if len(record) > 3 {
			lastName = record[3]
		}
		if len(record) > 4 {
			r := strings.ToLower(record[4])
			if r == "admin" || r == "owner" || r == "user" {
				role = r
			}
		}

		if username == "" {
			skipped++
			continue
		}

		// Zkontroluj zda uživatel existuje
		var existingID int
		err = db.Pool.QueryRow(ctx, `SELECT id FROM users WHERE username=$1`, username).Scan(&existingID)

		if err != nil {
			// Uživatel neexistuje — vytvoř nového
			randPass := randomPassword()
			hash, hashErr := HashPassword(randPass)
			if hashErr != nil {
				log.Printf("[user_import] bcrypt error: %v", hashErr)
				skipped++
				continue
			}

			isAdmin := role == "admin" || role == "owner"
			isOwner := role == "owner"

			fields := []UserInsertField{
				{Col: "username", Val: username, Include: true},
				{Col: "password_hash", Val: hash, Include: true},
				{Col: "is_admin", Val: isAdmin, Include: userCols.IsAdmin},
				{Col: "is_owner", Val: isOwner, Include: userCols.IsOwner},
			}
			if userCols.Email && email != "" {
				fields = append(fields, UserInsertField{Col: "email", Val: email, Include: true})
			}
			// first_name / last_name — přidej jen pokud sloupce existují
			// (dynamicky nezjišťujeme, použijeme try/catch via separate exec)
			sql, vals := buildUserInsertSQL(fields)
			var newID int
			if err := db.Pool.QueryRow(ctx, sql, vals...).Scan(&newID); err != nil {
				log.Printf("[user_import] INSERT error pro '%s': %v", username, err)
				skipped++
				continue
			}

			// Pokus o update first_name / last_name v samostatném dotazu (ignoruj chybu)
			if firstName != "" || lastName != "" {
				_, _ = db.Pool.Exec(ctx,
					`UPDATE users SET first_name=$1, last_name=$2 WHERE id=$3`,
					nilIfEmpty(firstName), nilIfEmpty(lastName), newID)
			}

			created++
		} else {
			// Uživatel existuje — update email, first_name, last_name pokud jsou vyplněny
			updates := []string{}
			vals := []interface{}{}
			n := 1

			if email != "" && userCols.Email {
				updates = append(updates, fmt.Sprintf("email=$%d", n))
				vals = append(vals, email)
				n++
			}
			if firstName != "" {
				updates = append(updates, fmt.Sprintf("first_name=$%d", n))
				vals = append(vals, firstName)
				n++
			}
			if lastName != "" {
				updates = append(updates, fmt.Sprintf("last_name=$%d", n))
				vals = append(vals, lastName)
				n++
			}

			if len(updates) > 0 {
				vals = append(vals, existingID)
				updateSQL := "UPDATE users SET " + strings.Join(updates, ", ") +
					fmt.Sprintf(" WHERE id=$%d", n)
				if _, err := db.Pool.Exec(ctx, updateSQL, vals...); err != nil {
					log.Printf("[user_import] UPDATE error pro '%s': %v", username, err)
					skipped++
					continue
				}
				updated++
			} else {
				skipped++
			}
		}
	}

	msg := fmt.Sprintf("Import dokončen: vytvořeno %d, aktualizováno %d, přeskočeno %d uživatelů.", created, updated, skipped)
	middleware.SetFlash(w, r, "ok", msg)
	http.Redirect(w, r, "/admin/users/import", http.StatusSeeOther)
}

// nilIfEmpty vrátí nil pro prázdný string, jinak pointer na string.
func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
