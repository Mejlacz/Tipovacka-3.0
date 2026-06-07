// handlers/profile.go — Tipovačka 2.0
// Uživatelský profil: zobrazení, editace, změna hesla, statistiky.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"tipovacka/config"
	"tipovacka/db"
	"tipovacka/middleware"
	"tipovacka/models"
)

// GET /profile
func ProfilePage(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := RequireApproved(w, r)
		if user == nil {
			return
		}
		ctx := context.Background()

		// Competitions
		compRows, _ := db.Pool.Query(ctx,
			`SELECT id, name, season, is_active, sport, sort_order FROM competitions WHERE is_active = TRUE AND COALESCE(is_hidden,false)=false ORDER BY sort_order ASC NULLS LAST, id DESC`)
		var competitions []*models.Competition
		for compRows.Next() {
			c := &models.Competition{}
			_ = compRows.Scan(&c.ID, &c.Name, &c.Season, &c.IsActive, &c.Sport, &c.SortOrder)
			competitions = append(competitions, c)
		}
		compRows.Close()

		// Stats per competition
		type CompStat struct {
			Comp         *models.Competition
			TipsGiven    int
			TotalFinished int
			Exact        int
			Winner       int
			Miss         int
			TipPts       int
			ExtraPts     int
			GrandTotal   int
		}
		var stats []CompStat

		for _, comp := range competitions {
			// Get round IDs for this competition
			var roundIDs []int
			rRows, _ := db.Pool.Query(ctx, `SELECT id FROM rounds WHERE competition_id=$1`, comp.ID)
			for rRows.Next() {
				var rid int
				_ = rRows.Scan(&rid)
				roundIDs = append(roundIDs, rid)
			}
			rRows.Close()
			if len(roundIDs) == 0 {
				continue
			}

			// Matches
			var matchIDs []int
			finishedIDs := map[int]bool{}
			mRows, _ := db.Pool.Query(ctx,
				`SELECT id, is_finished FROM matches WHERE round_id = ANY($1)`, roundIDs)
			for mRows.Next() {
				var mid int
				var fin bool
				_ = mRows.Scan(&mid, &fin)
				matchIDs = append(matchIDs, mid)
				if fin {
					finishedIDs[mid] = true
				}
			}
			mRows.Close()
			if len(matchIDs) == 0 {
				continue
			}

			// User tips
			tRows, _ := db.Pool.Query(ctx,
				`SELECT match_id, points FROM tips WHERE user_id=$1 AND match_id = ANY($2)`,
				user.ID, matchIDs)
			var tipPts, exact, winner, miss, tipsGiven int
			for tRows.Next() {
				var matchID int
				var pts *int
				_ = tRows.Scan(&matchID, &pts)
				tipsGiven++
				if finishedIDs[matchID] && pts != nil {
					tipPts += *pts
					switch *pts {
					case 3:
						exact++
					case 1:
						winner++
					case 0:
						miss++
					}
				}
			}
			tRows.Close()
			if tipsGiven == 0 {
				continue
			}

			// Extra points for this competition
			var extraPts int
			_ = db.Pool.QueryRow(ctx,
				`SELECT COALESCE(SUM(ea.points),0) FROM extra_answers ea
				   JOIN extra_questions eq ON eq.id = ea.question_id
				  WHERE eq.competition_id=$1 AND ea.user_id=$2 AND ea.points IS NOT NULL`,
				comp.ID, user.ID).Scan(&extraPts)

			stats = append(stats, CompStat{
				Comp:          comp,
				TipsGiven:    tipsGiven,
				TotalFinished: len(finishedIDs),
				Exact:        exact,
				Winner:       winner,
				Miss:         miss,
				TipPts:       tipPts,
				ExtraPts:     extraPts,
				GrandTotal:   tipPts + extraPts,
			})
		}

		// Active competitions + user notification opt-in
		var activeComps []*models.Competition
		for _, c := range competitions {
			if c.IsActive {
				activeComps = append(activeComps, c)
			}
		}
		notifyCompIDs := map[int]bool{}
		nsRows, _ := db.Pool.Query(ctx,
			`SELECT competition_id FROM notification_settings WHERE user_id=$1`, user.ID)
		for nsRows.Next() {
			var cid int
			_ = nsRows.Scan(&cid)
			notifyCompIDs[cid] = true
		}
		nsRows.Close()

		// Push subscriptions
		type PushSub struct {
			ID       int
			Endpoint string
		}
		var pushSubs []PushSub
		psRows, _ := db.Pool.Query(ctx,
			`SELECT id, endpoint FROM push_subscriptions WHERE user_id=$1 ORDER BY created_at`, user.ID)
		for psRows.Next() {
			var ps PushSub
			_ = psRows.Scan(&ps.ID, &ps.Endpoint)
			pushSubs = append(pushSubs, ps)
		}
		psRows.Close()

		RenderTemplate(w, r, tmpl, "profile.html", TemplateData{
			"User":               user,
			"Stats":              stats,
			"ActiveCompetitions": activeComps,
			"NotifyCompIDs":      notifyCompIDs,
			"PushSubs":           pushSubs,
			"PushDevices":        len(pushSubs),
			"Msg":                r.URL.Query().Get("msg"),
			"Error":              r.URL.Query().Get("error"),
			"Flash":              middleware.GetFlash(w, r),
		})
	}
}

// POST /profile/update
func ProfileUpdate(w http.ResponseWriter, r *http.Request) {
	user := RequireApproved(w, r)
	if user == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	firstName := strings.TrimSpace(r.FormValue("first_name"))
	lastName := strings.TrimSpace(r.FormValue("last_name"))
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	lang := r.FormValue("lang")
	if lang != "cs" && lang != "en" {
		lang = "cs"
	}

	ctx := context.Background()
	// Check email uniqueness
	if email != "" && userCols.Email {
		var conflict int
		_ = db.Pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM users WHERE email=$1 AND id!=$2`, email, user.ID).Scan(&conflict)
		if conflict > 0 {
			middleware.SetFlash(w, r, "err", "Tento email je již používán.")
			http.Redirect(w, r, "/profile", http.StatusSeeOther)
			return
		}
	}

	if userCols.Email {
		_, _ = db.Pool.Exec(ctx,
			`UPDATE users SET email=$1 WHERE id=$2`,
			PtrStr(email), user.ID)
	}
	if userCols.Lang {
		_, _ = db.Pool.Exec(ctx, `UPDATE users SET lang=$1 WHERE id=$2`, lang, user.ID)
	}
	if userCols.FirstName {
		fn := PtrStr(firstName)
		_, _ = db.Pool.Exec(ctx, `UPDATE users SET first_name=$1 WHERE id=$2`, fn, user.ID)
	}
	if userCols.LastName {
		ln := PtrStr(lastName)
		_, _ = db.Pool.Exec(ctx, `UPDATE users SET last_name=$1 WHERE id=$2`, ln, user.ID)
	}
	middleware.SetFlash(w, r, "ok", "Údaje uloženy.")
	http.Redirect(w, r, "/profile", http.StatusSeeOther)
}

// POST /profile/password
func ProfilePassword(w http.ResponseWriter, r *http.Request) {
	user := RequireApproved(w, r)
	if user == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	oldPw := r.FormValue("old_password")
	newPw := r.FormValue("new_password")
	newPw2 := r.FormValue("new_password2")

	if !VerifyPassword(oldPw, user.PasswordHash) {
		http.Redirect(w, r, "/profile?error=profile_pw_wrong", http.StatusSeeOther)
		return
	}
	if len(newPw) < 8 {
		http.Redirect(w, r, "/profile?error=profile_pw_short", http.StatusSeeOther)
		return
	}
	if newPw != newPw2 {
		http.Redirect(w, r, "/profile?error=profile_pw_mismatch", http.StatusSeeOther)
		return
	}
	hash, err := HashPassword(newPw)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	_, _ = db.Pool.Exec(context.Background(),
		`UPDATE users SET password_hash=$1 WHERE id=$2`, hash, user.ID)
	http.Redirect(w, r, "/profile?msg=profile_pw_changed", http.StatusSeeOther)
}

// POST /profile/notifications
func ProfileNotifications(w http.ResponseWriter, r *http.Request) {
	user := RequireApproved(w, r)
	if user == nil {
		return
	}
	if !user.IsOwner && !user.NotifyAccess {
		http.Redirect(w, r, "/profile", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	selectedIDs := map[int]bool{}
	for _, v := range r.Form["notify_comp"] {
		id, err := strconv.Atoi(v)
		if err == nil && id > 0 {
			selectedIDs[id] = true
		}
	}

	ctx := context.Background()
	// Get active competition IDs
	activeIDs := map[int]bool{}
	acRows, _ := db.Pool.Query(ctx, `SELECT id FROM competitions WHERE is_active=TRUE AND COALESCE(is_hidden,false)=false`)
	for acRows.Next() {
		var id int
		_ = acRows.Scan(&id)
		activeIDs[id] = true
	}
	acRows.Close()

	_, _ = db.Pool.Exec(ctx,
		`DELETE FROM notification_settings WHERE user_id=$1 AND competition_id = ANY($2)`,
		user.ID, activeIDsSlice(activeIDs))
	for cid := range selectedIDs {
		if activeIDs[cid] {
			_, _ = db.Pool.Exec(ctx,
				`INSERT INTO notification_settings (user_id, competition_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
				user.ID, cid)
		}
	}
	http.Redirect(w, r, "/profile?msg=profile_notify_saved", http.StatusSeeOther)
}

func activeIDsSlice(m map[int]bool) []int {
	s := make([]int, 0, len(m))
	for id := range m {
		s = append(s, id)
	}
	return s
}

// POST /profile/push-subscribe  (JSON body)
func ProfilePushSubscribe(w http.ResponseWriter, r *http.Request) {
	user := GetCurrentUser(r)
	if user == nil || (!user.IsOwner && !user.NotifyAccess) {
		jsonError(w, "unauthorized", http.StatusForbidden)
		return
	}
	var body struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Endpoint == "" {
		jsonError(w, "invalid subscription", http.StatusBadRequest)
		return
	}
	ctx := context.Background()
	_, _ = db.Pool.Exec(ctx,
		`INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth)
		 VALUES ($1,$2,$3,$4)
		 ON CONFLICT (user_id, endpoint) DO UPDATE SET p256dh=$3, auth=$4`,
		user.ID, body.Endpoint, body.Keys.P256dh, body.Keys.Auth)
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// POST /profile/push-unsubscribe  (JSON body)
func ProfilePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	user := GetCurrentUser(r)
	if user == nil {
		jsonError(w, "unauthorized", http.StatusForbidden)
		return
	}
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.Endpoint != "" {
		_, _ = db.Pool.Exec(context.Background(),
			`DELETE FROM push_subscriptions WHERE user_id=$1 AND endpoint=$2`, user.ID, body.Endpoint)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// POST /profile/push-test
func ProfilePushTest(w http.ResponseWriter, r *http.Request) {
	// TODO: implement — Web Push not implemented in Go version
	user := GetCurrentUser(r)
	if user == nil || (!user.IsOwner && !user.NotifyAccess) {
		jsonError(w, "unauthorized", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":false,"error":"push_not_implemented"}`))
}

// POST /profile/push-delete/{sub_id}
func ProfilePushDelete(w http.ResponseWriter, r *http.Request) {
	user := GetCurrentUser(r)
	if user == nil {
		jsonError(w, "unauthorized", http.StatusForbidden)
		return
	}
	// sub_id from path
	subID, _ := strconv.Atoi(r.PathValue("sub_id"))
	ctx := context.Background()
	if user.IsAdmin {
		_, _ = db.Pool.Exec(ctx, `DELETE FROM push_subscriptions WHERE id=$1`, subID)
	} else {
		_, _ = db.Pool.Exec(ctx,
			`DELETE FROM push_subscriptions WHERE id=$1 AND user_id=$2`, subID, user.ID)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// POST /profile/avatar — nahrání profilové fotky
func ProfileAvatarUpload(w http.ResponseWriter, r *http.Request) {
	user := RequireApproved(w, r)
	if user == nil {
		return
	}

	if err := r.ParseMultipartForm(int64(config.MaxUploadBytes)); err != nil {
		middleware.SetFlash(w, r, "err", "Soubor je příliš velký (max 5 MB).")
		http.Redirect(w, r, "/profile", http.StatusSeeOther)
		return
	}

	file, fileHeader, err := r.FormFile("avatar")
	if err != nil {
		middleware.SetFlash(w, r, "err", "Vyberte obrázek (JPEG, PNG nebo WEBP).")
		http.Redirect(w, r, "/profile", http.StatusSeeOther)
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(fileHeader.Filename))
	if !config.AllowedImageExtensions[ext] {
		middleware.SetFlash(w, r, "err", "Nepodporovaný formát. Použijte .jpg, .jpeg, .png nebo .webp.")
		http.Redirect(w, r, "/profile", http.StatusSeeOther)
		return
	}

	// Ensure avatars directory exists
	avatarDir := "static/avatars"
	if err := os.MkdirAll(avatarDir, 0755); err != nil {
		middleware.SetFlash(w, r, "err", "Chyba serveru při ukládání souboru.")
		http.Redirect(w, r, "/profile", http.StatusSeeOther)
		return
	}

	// Remove any existing avatar for this user (different extension)
	cleanOldAvatars(user.ID)

	filename := fmt.Sprintf("%d%s", user.ID, ext)
	destPath := filepath.Join(avatarDir, filename)
	out, err := os.Create(destPath)
	if err != nil {
		middleware.SetFlash(w, r, "err", "Nepodařilo se uložit soubor.")
		http.Redirect(w, r, "/profile", http.StatusSeeOther)
		return
	}
	defer out.Close()
	if _, err = io.Copy(out, file); err != nil {
		middleware.SetFlash(w, r, "err", "Chyba při zápisu souboru.")
		http.Redirect(w, r, "/profile", http.StatusSeeOther)
		return
	}

	avatarURL := "/static/avatars/" + filename
	_, _ = db.Pool.Exec(context.Background(),
		`UPDATE users SET background_url=$1 WHERE id=$2`, avatarURL, user.ID)

	middleware.SetFlash(w, r, "ok", "Profilová fotka byla nahrána.")
	http.Redirect(w, r, "/profile", http.StatusSeeOther)
}

// POST /profile/avatar/delete — smazání profilové fotky
func ProfileAvatarDelete(w http.ResponseWriter, r *http.Request) {
	user := RequireApproved(w, r)
	if user == nil {
		return
	}

	cleanOldAvatars(user.ID)

	_, _ = db.Pool.Exec(context.Background(),
		`UPDATE users SET background_url=NULL WHERE id=$1`, user.ID)

	middleware.SetFlash(w, r, "ok", "Profilová fotka byla odstraněna.")
	http.Redirect(w, r, "/profile", http.StatusSeeOther)
}

// cleanOldAvatars smaže všechny existující avatary uživatele (jakákoli přípona).
func cleanOldAvatars(userID int) {
	for ext := range config.AllowedImageExtensions {
		path := filepath.Join("static", "avatars", fmt.Sprintf("%d%s", userID, ext))
		_ = os.Remove(path) // ignore error — file may not exist
	}
}

