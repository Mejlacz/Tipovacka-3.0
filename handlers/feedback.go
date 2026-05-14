// handlers/feedback.go — Tipovačka 3.0
// Zpětná vazba: uživatelé píší zprávy adminům (s volitelným screenshotem), admini je čtou.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"tipovacka/config"
	"tipovacka/db"
)

// FeedbackItem represents a single feedback message.
type FeedbackItem struct {
	ID        int64
	UserID    int64
	Username  string
	Message   string
	ImageURL  *string
	CreatedAt *time.Time
	IsRead    bool
}

// GET /feedback — user page: form + their own previous messages
func FeedbackPage(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := RequireLogin(w, r)
		if user == nil {
			return
		}
		ctx := context.Background()

		rows, err := db.Pool.Query(ctx,
			`SELECT id, message, image_url, created_at, is_read
			 FROM feedback
			 WHERE user_id = $1
			 ORDER BY created_at DESC
			 LIMIT 50`,
			user.ID)
		if err != nil {
			log.Printf("[feedback] query own: %v", err)
		}
		var items []FeedbackItem
		if rows != nil {
			for rows.Next() {
				var f FeedbackItem
				_ = rows.Scan(&f.ID, &f.Message, &f.ImageURL, &f.CreatedAt, &f.IsRead)
				f.UserID = int64(user.ID)
				f.Username = user.Username
				items = append(items, f)
			}
			rows.Close()
		}

		RenderTemplate(w, r, tmpl, "feedback.html", TemplateData{
			"User":  user,
			"Title": "Napište nám",
			"Items": items,
		})
	}
}

// POST /feedback/submit — multipart AJAX, submit a new message (+ optional image)
func FeedbackSubmit(w http.ResponseWriter, r *http.Request) {
	user := RequireLogin(w, r)
	if user == nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")

	// Parse multipart (max 8 MB total)
	if err := r.ParseMultipartForm(8 * 1024 * 1024); err != nil {
		// fallback: maybe no file, try regular form
		_ = r.ParseForm()
	}

	message := strings.TrimSpace(r.FormValue("message"))
	if len(message) < 3 {
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "too_short"})
		return
	}
	if len(message) > 5000 {
		message = message[:5000]
	}

	// Handle optional image
	var imageURL *string
	file, fileHeader, fileErr := r.FormFile("image")
	if fileErr == nil {
		defer file.Close()
		ext := strings.ToLower(filepath.Ext(fileHeader.Filename))
		if !config.AllowedImageExtensions[ext] {
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "bad_image_type"})
			return
		}
		if fileHeader.Size > int64(config.MaxUploadBytes) {
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "image_too_large"})
			return
		}

		imgDir := "static/feedback-images"
		if err := os.MkdirAll(imgDir, 0755); err != nil {
			log.Printf("[feedback] mkdir: %v", err)
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "server_error"})
			return
		}

		filename := fmt.Sprintf("%d_%d%s", user.ID, time.Now().UnixNano(), ext)
		destPath := filepath.Join(imgDir, filename)
		out, err := os.Create(destPath)
		if err != nil {
			log.Printf("[feedback] create file: %v", err)
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "server_error"})
			return
		}
		defer out.Close()
		if _, err = io.Copy(out, file); err != nil {
			log.Printf("[feedback] write file: %v", err)
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "server_error"})
			return
		}
		url := "/static/feedback-images/" + filename
		imageURL = &url
	}

	ctx := context.Background()
	var newID int64
	err := db.Pool.QueryRow(ctx,
		`INSERT INTO feedback (user_id, message, image_url, created_at, is_read)
		 VALUES ($1, $2, $3, NOW(), false) RETURNING id`,
		user.ID, message, imageURL).Scan(&newID)
	if err != nil {
		log.Printf("[feedback] insert: %v", err)
		// cleanup uploaded file on DB error
		if imageURL != nil {
			_ = os.Remove(strings.TrimPrefix(*imageURL, "/"))
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "db_error"})
		return
	}

	resp := map[string]interface{}{"ok": true, "id": newID}
	if imageURL != nil {
		resp["image_url"] = *imageURL
	}
	json.NewEncoder(w).Encode(resp)
}

// GET /admin/feedback — admin page: all messages, newest first, unread highlighted
func AdminFeedbackPage(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		ctx := context.Background()

		filter := r.URL.Query().Get("filter") // "unread" or "" (all)

		query := `SELECT f.id, f.user_id, u.username, f.message, f.image_url, f.created_at, f.is_read
		          FROM feedback f
		          JOIN users u ON u.id = f.user_id`
		if filter == "unread" {
			query += ` WHERE f.is_read = false`
		}
		query += ` ORDER BY f.created_at DESC LIMIT 200`

		rows, err := db.Pool.Query(ctx, query)
		if err != nil {
			log.Printf("[admin/feedback] query: %v", err)
		}
		var items []FeedbackItem
		if rows != nil {
			for rows.Next() {
				var f FeedbackItem
				_ = rows.Scan(&f.ID, &f.UserID, &f.Username, &f.Message, &f.ImageURL, &f.CreatedAt, &f.IsRead)
				items = append(items, f)
			}
			rows.Close()
		}

		var unreadCount int
		_ = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM feedback WHERE is_read = false`).Scan(&unreadCount)

		RenderTemplate(w, r, tmpl, "admin/feedback.html", TemplateData{
			"User":         admin,
			"Title":        "Zpětná vazba",
			"Items":        items,
			"UnreadCount":  unreadCount,
			"FilterUnread": filter == "unread",
		})
	}
}

// POST /admin/feedback/{id}/read — mark message as read (AJAX)
func AdminFeedbackMarkRead(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")

	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if id == 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "bad_id"})
		return
	}
	_, err := db.Pool.Exec(context.Background(),
		`UPDATE feedback SET is_read = true WHERE id = $1`, id)
	if err != nil {
		log.Printf("[admin/feedback] mark-read: %v", err)
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "db_error"})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// POST /admin/feedback/read-all — mark all as read (AJAX)
func AdminFeedbackMarkAllRead(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, err := db.Pool.Exec(context.Background(), `UPDATE feedback SET is_read = true WHERE is_read = false`)
	if err != nil {
		log.Printf("[admin/feedback] mark-all-read: %v", err)
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "db_error"})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// GET /admin/feedback/unread-count — returns {"count": N} for navbar badge
func AdminFeedbackUnreadCount(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	var count int
	_ = db.Pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM feedback WHERE is_read = false`).Scan(&count)
	json.NewEncoder(w).Encode(map[string]interface{}{"count": count})
}
