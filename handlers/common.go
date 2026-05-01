// handlers/common.go — Tipovačka 2.0
// Společné pomocné funkce pro všechny handlery.
package handlers

import (
	"bytes"
	"context"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tipovacka/config"
	"tipovacka/db"
	"tipovacka/middleware"
	"tipovacka/models"
)

// ─── Schema detection ────────────────────────────────────────────────────────

// userCols obsahuje info o tom, které sloupce tabulky users v DB existují.
var userCols struct {
	Email         bool
	IsAdmin       bool
	IsOwner       bool
	IsBlocked     bool
	IsHidden      bool
	IsApproved    bool
	IsInactive    bool
	Lang          bool
	CreatedAt     bool
	FirstName     bool
	LastName      bool
	NotifyAccess  bool
	BackgroundURL bool
}

// InitUserSchema zjistí dostupné sloupce tabulky users jednou při startu.
func InitUserSchema() {
	ctx := context.Background()
	rows, err := db.Pool.Query(ctx,
		`SELECT column_name FROM information_schema.columns
		  WHERE table_name = 'users' AND table_schema = 'public'`)
	if err != nil {
		log.Printf("[schema] nelze zjistit sloupce users: %v — používám minimum", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var col string
		_ = rows.Scan(&col)
		switch col {
		case "email":          userCols.Email = true
		case "is_admin":       userCols.IsAdmin = true
		case "is_owner":       userCols.IsOwner = true
		case "is_blocked":     userCols.IsBlocked = true
		case "is_hidden":      userCols.IsHidden = true
		case "is_approved":    userCols.IsApproved = true
		case "is_inactive":    userCols.IsInactive = true
		case "lang":           userCols.Lang = true
		case "created_at":     userCols.CreatedAt = true
		case "first_name":     userCols.FirstName = true
		case "last_name":      userCols.LastName = true
		case "notify_access":  userCols.NotifyAccess = true
		case "background_url": userCols.BackgroundURL = true
		}
	}
	log.Printf("[schema] users: email=%v is_admin=%v is_owner=%v is_blocked=%v is_hidden=%v is_approved=%v is_inactive=%v lang=%v created_at=%v first_name=%v last_name=%v notify_access=%v background_url=%v",
		userCols.Email, userCols.IsAdmin, userCols.IsOwner, userCols.IsBlocked, userCols.IsHidden, userCols.IsApproved, userCols.IsInactive, userCols.Lang, userCols.CreatedAt, userCols.FirstName, userCols.LastName, userCols.NotifyAccess, userCols.BackgroundURL)
}

// buildUserSelect vrátí SELECT sloupců + jejich scan cíle pro daného uživatele.
// Vždy obsahuje: id, username, password_hash.
// Volitelně přidává existující sloupce.
func buildUserSelect() (cols string, scanInto func(u *models.User) []interface{}) {
	var names []string
	names = append(names, "id", "username", "password_hash")
	if userCols.Email {
		names = append(names, "email")
	}
	if userCols.IsAdmin {
		names = append(names, "is_admin")
	}
	if userCols.IsOwner {
		names = append(names, "is_owner")
	}
	if userCols.IsBlocked {
		names = append(names, "is_blocked")
	}
	if userCols.IsHidden {
		names = append(names, "is_hidden")
	}
	if userCols.IsApproved {
		names = append(names, "is_approved")
	}
	if userCols.IsInactive {
		names = append(names, "is_inactive")
	}
	if userCols.Lang {
		names = append(names, "lang")
	}
	if userCols.CreatedAt {
		names = append(names, "created_at")
	}
	if userCols.FirstName {
		names = append(names, "first_name")
	}
	if userCols.LastName {
		names = append(names, "last_name")
	}
	if userCols.NotifyAccess {
		names = append(names, "notify_access")
	}
	if userCols.BackgroundURL {
		names = append(names, "background_url")
	}

	cols = strings.Join(names, ", ")
	scanInto = func(u *models.User) []interface{} {
		var email *string
		ptrs := []interface{}{&u.ID, &u.Username, &u.PasswordHash}
		if userCols.Email {
			ptrs = append(ptrs, &email)
		}
		if userCols.IsAdmin {
			ptrs = append(ptrs, &u.IsAdmin)
		}
		if userCols.IsOwner {
			ptrs = append(ptrs, &u.IsOwner)
		}
		if userCols.IsBlocked {
			ptrs = append(ptrs, &u.IsBlocked)
		}
		if userCols.Lang {
			ptrs = append(ptrs, &u.Lang)
		}
		if userCols.CreatedAt {
			ptrs = append(ptrs, &u.CreatedAt)
		}
		if userCols.FirstName {
			ptrs = append(ptrs, &u.FirstName)
		}
		if userCols.LastName {
			ptrs = append(ptrs, &u.LastName)
		}
		_ = email // closure capture
		return ptrs
	}
	return cols, scanInto
}

// scanUser naskenuje řádek DB do modelu User a doplní výchozí hodnoty.
func scanUser(u *models.User, row interface {
	Scan(...interface{}) error
}) error {
	var email *string
	var isAdmin, isOwner, isBlocked, isHidden, isApproved, isInactive bool
	var lang string
	var createdAt time.Time

	ptrs := []interface{}{&u.ID, &u.Username, &u.PasswordHash}
	if userCols.Email {
		ptrs = append(ptrs, &email)
	}
	if userCols.IsAdmin {
		ptrs = append(ptrs, &isAdmin)
	}
	if userCols.IsOwner {
		ptrs = append(ptrs, &isOwner)
	}
	if userCols.IsBlocked {
		ptrs = append(ptrs, &isBlocked)
	}
	if userCols.IsHidden {
		ptrs = append(ptrs, &isHidden)
	}
	if userCols.IsApproved {
		ptrs = append(ptrs, &isApproved)
	}
	if userCols.IsInactive {
		ptrs = append(ptrs, &isInactive)
	}
	if userCols.Lang {
		ptrs = append(ptrs, &lang)
	}
	if userCols.CreatedAt {
		ptrs = append(ptrs, &createdAt)
	}
	var firstName, lastName *string
	if userCols.FirstName {
		ptrs = append(ptrs, &firstName)
	}
	if userCols.LastName {
		ptrs = append(ptrs, &lastName)
	}
	var notifyAccess bool
	if userCols.NotifyAccess {
		ptrs = append(ptrs, &notifyAccess)
	}
	var backgroundURL *string
	if userCols.BackgroundURL {
		ptrs = append(ptrs, &backgroundURL)
	}

	if err := row.Scan(ptrs...); err != nil {
		return err
	}

	u.Email = email
	u.IsAdmin = isAdmin || (config.AdminUsername != "" && u.Username == config.AdminUsername)
	u.IsOwner = isOwner || (config.AdminUsername != "" && u.Username == config.AdminUsername)
	u.IsBlocked = isBlocked
	u.IsHidden = isHidden
	// is_approved: pokud sloupec neexistuje, fallback = true (backward compat)
	u.IsApproved = isApproved || !userCols.IsApproved
	u.IsInactive = isInactive
	u.NotifyAccess = notifyAccess
	u.BackgroundURL = backgroundURL
	if lang != "" {
		u.Lang = lang
	} else {
		u.Lang = "cs"
	}
	if !userCols.CreatedAt {
		u.CreatedAt = time.Time{}
	} else {
		u.CreatedAt = createdAt
	}
	u.FirstName = firstName
	u.LastName = lastName
	return nil
}

// ─── Načítání aktuálního uživatele ───────────────────────────────────────────

// GetCurrentUser vrátí přihlášeného uživatele nebo nil.
func GetCurrentUser(r *http.Request) *models.User {
	uid := middleware.GetUserID(r)
	if uid == 0 {
		return nil
	}
	cols, _ := buildUserSelect()
	row := db.Pool.QueryRow(context.Background(),
		"SELECT "+cols+" FROM users WHERE id = $1", uid)
	u := &models.User{}
	if err := scanUser(u, row); err != nil {
		return nil
	}
	return u
}

// autoBlockIfExpired zablokuje čekajícího uživatele staršího 7 dní.
// Vrátí true pokud byl uživatel zablokován.
func autoBlockIfExpired(u *models.User) bool {
	if u.IsApproved || u.IsBlocked || u.IsAdmin || !userCols.CreatedAt || !userCols.IsBlocked {
		return false
	}
	if u.CreatedAt.IsZero() || time.Since(u.CreatedAt) <= 7*24*time.Hour {
		return false
	}
	_, _ = db.Pool.Exec(context.Background(),
		`UPDATE users SET is_blocked = true WHERE id = $1`, u.ID)
	u.IsBlocked = true
	log.Printf("[auto-block] uživatel %s (%d) zablokován — čekal déle než 7 dní", u.Username, u.ID)
	return true
}

// RequireAdmin vrátí admina nebo nil + přesměruje na /.
func RequireAdmin(w http.ResponseWriter, r *http.Request) *models.User {
	u := GetCurrentUser(r)
	if u == nil || !u.IsAdmin {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return nil
	}
	return u
}

// RequireLogin přesměruje na /login pokud není přihlášen nebo je blokován.
func RequireLogin(w http.ResponseWriter, r *http.Request) *models.User {
	u := GetCurrentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return nil
	}
	if autoBlockIfExpired(u) || u.IsBlocked {
		middleware.ClearSession(w, r)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return nil
	}
	return u
}

// RequireApproved přesměruje na /login nebo /pending pokud uživatel není schválený.
// Pending uživatelé starší 7 dní jsou automaticky blokováni.
func RequireApproved(w http.ResponseWriter, r *http.Request) *models.User {
	u := GetCurrentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return nil
	}
	if autoBlockIfExpired(u) || u.IsBlocked {
		middleware.ClearSession(w, r)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return nil
	}
	if !u.IsApproved && !u.IsAdmin {
		http.Redirect(w, r, "/pending", http.StatusSeeOther)
		return nil
	}
	return u
}

// ─── Deadline / čas ──────────────────────────────────────────────────────────

var pragueLocation *time.Location

func init() {
	var err error
	pragueLocation, err = time.LoadLocation("Europe/Prague")
	if err != nil {
		pragueLocation = time.UTC
	}
}

// NowPrague vrátí aktuální čas v pražském časovém pásmu jako naive (bez TZ).
func NowPrague() time.Time {
	return time.Now().In(pragueLocation)
}

// IsBeforeDeadline vrátí true pokud tipování ještě není uzavřeno.
func IsBeforeDeadline(round *models.Round, match *models.Match) bool {
	now := NowPrague()
	if match.MatchDate != nil {
		return now.Before(*match.MatchDate)
	}
	if round.Deadline != nil {
		return now.Before(*round.Deadline)
	}
	return false
}

// ─── Template rendering ───────────────────────────────────────────────────────

// TemplateData je základní mapa pro šablony.
type TemplateData map[string]interface{}

// RenderTemplate renderuje šablonu dvouprůchodově: page → base.html.
func RenderTemplate(w http.ResponseWriter, r *http.Request, tmpl *template.Template, name string, data TemplateData) {
	if data == nil {
		data = TemplateData{}
	}
	data["CSRFToken"] = middleware.GetCSRFToken(w, r)
	data["Lang"] = middleware.GetLang(r)
	data["User"] = GetCurrentUser(r)

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("template error (%s): %v", name, err)
		http.Error(w, "Chyba šablony: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data["Content"] = template.HTML(buf.String())

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "base.html", data); err != nil {
		log.Printf("base template error: %v", err)
		w.Write(buf.Bytes())
	}
}

// ─── Audit log ───────────────────────────────────────────────────────────────

// LogAction zapíše akci do audit logu.
func LogAction(adminID *int, adminUsername, action, entityType string, entityID *int, description string, oldValue, newValue *string) {
	_, err := db.Pool.Exec(context.Background(),
		`INSERT INTO audit_log (admin_id, admin_username, action, entity_type, entity_id, description, old_value, new_value)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		adminID, adminUsername, action, entityType, entityID, description, oldValue, newValue)
	if err != nil {
		log.Printf("[audit] chyba: %v", err)
	}
}

// ─── Recalculate standings ────────────────────────────────────────────────────

// RecalculateStandings přepočítá pořadí soutěže.
func RecalculateStandings(competitionID int) {
	ctx := context.Background()

	roundRows, err := db.Pool.Query(ctx, `SELECT id FROM rounds WHERE competition_id = $1`, competitionID)
	if err != nil {
		log.Printf("[standings] rounds query error: %v", err)
		return
	}
	var roundIDs []int
	for roundRows.Next() {
		var rid int
		_ = roundRows.Scan(&rid)
		roundIDs = append(roundIDs, rid)
	}
	roundRows.Close()
	if len(roundIDs) == 0 {
		return
	}

	tipRows, err := db.Pool.Query(ctx,
		`SELECT user_id, points FROM tips
		  JOIN matches ON matches.id = tips.match_id
		  WHERE matches.round_id = ANY($1)`, roundIDs)
	if err != nil {
		log.Printf("[standings] tips query error: %v", err)
		return
	}
	type userStats struct{ tipPts, exact, partial, miss int }
	statsByUser := map[int]*userStats{}
	for tipRows.Next() {
		var uid int
		var pts *int
		_ = tipRows.Scan(&uid, &pts)
		if statsByUser[uid] == nil {
			statsByUser[uid] = &userStats{}
		}
		if pts != nil {
			statsByUser[uid].tipPts += *pts
			switch *pts {
			case 3:
				statsByUser[uid].exact++
			case 1:
				statsByUser[uid].partial++
			case 0:
				statsByUser[uid].miss++
			}
		}
	}
	tipRows.Close()

	extraRows, err := db.Pool.Query(ctx,
		`SELECT ea.user_id, SUM(COALESCE(ea.points, 0))
		   FROM extra_answers ea
		   JOIN extra_questions eq ON eq.id = ea.question_id
		  WHERE eq.competition_id = $1 AND ea.points IS NOT NULL
		  GROUP BY ea.user_id`, competitionID)
	extraByUser := map[int]int{}
	if err == nil {
		for extraRows.Next() {
			var uid, pts int
			_ = extraRows.Scan(&uid, &pts)
			extraByUser[uid] = pts
		}
		extraRows.Close()
	}

	allUIDs := map[int]bool{}
	for uid := range statsByUser {
		allUIDs[uid] = true
	}
	for uid := range extraByUser {
		allUIDs[uid] = true
	}

	now := time.Now().UTC()
	for uid := range allUIDs {
		s := statsByUser[uid]
		if s == nil {
			s = &userStats{}
		}
		extra := extraByUser[uid]
		grand := s.tipPts + extra

		_, err := db.Pool.Exec(ctx,
			`INSERT INTO competition_standings
			   (competition_id, user_id, tip_points, extra_points, grand_total, exact_count, partial_count, miss_count, updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			 ON CONFLICT (competition_id, user_id) DO UPDATE SET
			   tip_points=$3, extra_points=$4, grand_total=$5,
			   exact_count=$6, partial_count=$7, miss_count=$8, updated_at=$9`,
			competitionID, uid, s.tipPts, extra, grand, s.exact, s.partial, s.miss, now)
		if err != nil {
			log.Printf("[standings] upsert error uid=%d: %v", uid, err)
		}
	}
}

// ─── Tip points kalkulace ─────────────────────────────────────────────────────

// RecalculateTips přepočítá body všech tipů pro daný zápas.
func RecalculateTips(ctx context.Context, matchID, homeScore, awayScore int) {
	rows, err := db.Pool.Query(ctx, `SELECT id, home_score, away_score FROM tips WHERE match_id = $1`, matchID)
	if err != nil {
		return
	}
	type tipRow struct{ id, home, away int }
	var tips []tipRow
	for rows.Next() {
		var t tipRow
		_ = rows.Scan(&t.id, &t.home, &t.away)
		tips = append(tips, t)
	}
	rows.Close()

	for _, t := range tips {
		tip := &models.Tip{HomeScore: t.home, AwayScore: t.away}
		pts := tip.CalculatePoints(homeScore, awayScore)
		_, _ = db.Pool.Exec(ctx, `UPDATE tips SET points = $1 WHERE id = $2`, pts, t.id)
	}
}

// ─── EnsureAdmin ─────────────────────────────────────────────────────────────

// EnsureAdmin creates the admin user on first startup if not already present.
// Uses schema-aware INSERT — only inserts columns that exist in the DB.
func EnsureAdmin(username, password string) {
	if username == "" || password == "" {
		return
	}
	ctx := context.Background()
	var count int
	_ = db.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM users WHERE username=$1`, username).Scan(&count)
	if count > 0 {
		return
	}
	hash, err := HashPassword(password)
	if err != nil {
		log.Printf("[ensureAdmin] bcrypt error: %v", err)
		return
	}
	sql, vals := buildUserInsertSQL([]UserInsertField{
		{Col: "username", Val: username, Include: true},
		{Col: "password_hash", Val: hash, Include: true},
		{Col: "is_admin", Val: true, Include: userCols.IsAdmin},
		{Col: "is_owner", Val: true, Include: userCols.IsOwner},
	})
	var newID int
	if err = db.Pool.QueryRow(ctx, sql, vals...).Scan(&newID); err != nil {
		log.Printf("[ensureAdmin] DB error: %v", err)
		return
	}
	log.Printf("[ensureAdmin] Admin '%s' created (id=%d).", username, newID)
}

// ─── Dynamic INSERT / UPDATE helpers ─────────────────────────────────────────

// UserInsertField describes one column to include in a dynamic INSERT.
type UserInsertField struct {
	Col     string
	Val     interface{}
	Include bool
}

// buildUserInsertSQL builds "INSERT INTO users (...) VALUES (...) RETURNING id"
// from a list of fields, skipping those with Include=false.
func buildUserInsertSQL(fields []UserInsertField) (string, []interface{}) {
	var cols []string
	var vals []interface{}
	for _, f := range fields {
		if f.Include {
			cols = append(cols, f.Col)
			vals = append(vals, f.Val)
		}
	}
	phs := make([]string, len(cols))
	for i := range phs {
		phs[i] = "$" + strconv.Itoa(i+1)
	}
	sql := "INSERT INTO users (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(phs, ", ") + ") RETURNING id"
	return sql, vals
}

// UserUpdateField describes one column to include in a dynamic UPDATE SET clause.
type UserUpdateField struct {
	Col     string
	Val     interface{}
	Include bool
}

// buildUserUpdateSQL builds "UPDATE users SET ... WHERE id=$N" dynamically,
// using only fields with Include=true. Always includes username (always present).
// Returns the SQL string, values slice (with userID appended last), and the $N for WHERE.
func buildUserUpdateSQL(userID int, fields []UserUpdateField) (string, []interface{}) {
	var setClauses []string
	var vals []interface{}
	n := 1
	for _, f := range fields {
		if f.Include {
			setClauses = append(setClauses, f.Col+"=$"+strconv.Itoa(n))
			vals = append(vals, f.Val)
			n++
		}
	}
	vals = append(vals, userID)
	sql := "UPDATE users SET " + strings.Join(setClauses, ", ") + " WHERE id=$" + strconv.Itoa(n)
	return sql, vals
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// Ptr vrátí pointer na hodnotu (helper).
func Ptr[T any](v T) *T {
	return &v
}

// PtrStr vrátí *string nebo nil pro prázdný string.
func PtrStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
