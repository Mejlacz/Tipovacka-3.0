// handlers/admin_groups.go — Tipovačka 2.0
// Správa skupin uživatelů (pouze Owner).
package handlers

import (
	"context"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"tipovacka/db"
	"tipovacka/middleware"
	"tipovacka/models"
)

// requireOwner vrátí přihlášeného uživatele jen pokud je Owner, jinak přesměruje.
func requireOwner(w http.ResponseWriter, r *http.Request) *models.User {
	u := GetCurrentUser(r)
	if u == nil || !u.IsOwner {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return nil
	}
	return u
}

// GET /admin/groups
func AdminGroupsList(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		owner := requireOwner(w, r)
		if owner == nil {
			return
		}
		ctx := context.Background()

		// Load groups
		gRows, _ := db.Pool.Query(ctx,
			`SELECT id, name, can_see_hidden FROM user_groups ORDER BY name`)
		var groups []*models.UserGroup
		for gRows.Next() {
			g := &models.UserGroup{}
			_ = gRows.Scan(&g.ID, &g.Name, &g.CanSeeHidden)
			groups = append(groups, g)
		}
		gRows.Close()

		// For each group, load member IDs and usernames
		type MemberEntry struct {
			UserID   int
			Username string
			IsHidden bool
		}
		groupMembers := map[int][]MemberEntry{}
		memberIDs := map[int]map[int]bool{}
		for _, g := range groups {
			mRows, _ := db.Pool.Query(ctx,
				`SELECT gm.user_id, u.username, COALESCE(u.is_hidden, false)
				   FROM group_memberships gm
				   JOIN users u ON u.id = gm.user_id
				  WHERE gm.group_id=$1
				  ORDER BY u.username`, g.ID)
			var members []MemberEntry
			ids := map[int]bool{}
			for mRows.Next() {
				var m MemberEntry
				_ = mRows.Scan(&m.UserID, &m.Username, &m.IsHidden)
				members = append(members, m)
				ids[m.UserID] = true
			}
			mRows.Close()
			groupMembers[g.ID] = members
			memberIDs[g.ID] = ids
		}

		// All non-blocked users (for the add-member dropdown)
		cols, _ := buildUserSelect()
		var allUsers []*models.User
		if userCols.IsBlocked {
			uRows, _ := db.Pool.Query(ctx,
				"SELECT "+cols+" FROM users WHERE is_blocked=FALSE ORDER BY username")
			for uRows.Next() {
				u := &models.User{}
				if scanUser(u, uRows) == nil {
					allUsers = append(allUsers, u)
				}
			}
			uRows.Close()
		} else {
			uRows, _ := db.Pool.Query(ctx, "SELECT "+cols+" FROM users ORDER BY username")
			for uRows.Next() {
				u := &models.User{}
				if scanUser(u, uRows) == nil {
					allUsers = append(allUsers, u)
				}
			}
			uRows.Close()
		}

		flash := middleware.GetFlash(w, r)

		RenderTemplate(w, r, tmpl, "admin/groups.html", TemplateData{
			"User":         owner,
			"Groups":       groups,
			"GroupMembers": groupMembers,
			"MemberIDs":    memberIDs,
			"AllUsers":     allUsers,
			"Flash":        flash,
		})
	}
}

// POST /admin/groups/new
func AdminGroupsNew(w http.ResponseWriter, r *http.Request) {
	owner := requireOwner(w, r)
	if owner == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		middleware.SetFlash(w, r, "warn", "Název skupiny nesmí být prázdný.")
		http.Redirect(w, r, "/admin/groups", http.StatusSeeOther)
		return
	}
	canSeeHidden := r.FormValue("can_see_hidden") == "true" || r.FormValue("can_see_hidden") == "on"
	ctx := context.Background()
	_, _ = db.Pool.Exec(ctx,
		`INSERT INTO user_groups (name, can_see_hidden) VALUES ($1, $2)`, name, canSeeHidden)
	middleware.SetFlash(w, r, "ok", "Skupina '"+name+"' vytvořena.")
	http.Redirect(w, r, "/admin/groups", http.StatusSeeOther)
}

// POST /admin/groups/{group_id}/delete
func AdminGroupsDelete(w http.ResponseWriter, r *http.Request) {
	owner := requireOwner(w, r)
	if owner == nil {
		return
	}
	groupID, _ := strconv.Atoi(r.PathValue("group_id"))
	ctx := context.Background()
	var name string
	_ = db.Pool.QueryRow(ctx, `SELECT name FROM user_groups WHERE id=$1`, groupID).Scan(&name)
	_, _ = db.Pool.Exec(ctx, `DELETE FROM user_groups WHERE id=$1`, groupID)
	if name != "" {
		middleware.SetFlash(w, r, "ok", "Skupina '"+name+"' smazána.")
	}
	http.Redirect(w, r, "/admin/groups", http.StatusSeeOther)
}

// POST /admin/groups/{group_id}/toggle-hidden
func AdminGroupsToggleHidden(w http.ResponseWriter, r *http.Request) {
	owner := requireOwner(w, r)
	if owner == nil {
		return
	}
	groupID, _ := strconv.Atoi(r.PathValue("group_id"))
	ctx := context.Background()
	_, _ = db.Pool.Exec(ctx,
		`UPDATE user_groups SET can_see_hidden = NOT can_see_hidden WHERE id=$1`, groupID)
	http.Redirect(w, r, "/admin/groups", http.StatusSeeOther)
}

// POST /admin/groups/{group_id}/add-member
func AdminGroupsAddMember(w http.ResponseWriter, r *http.Request) {
	owner := requireOwner(w, r)
	if owner == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	groupID, _ := strconv.Atoi(r.PathValue("group_id"))
	userID, _ := strconv.Atoi(r.FormValue("user_id"))
	if groupID == 0 || userID == 0 {
		http.Redirect(w, r, "/admin/groups", http.StatusSeeOther)
		return
	}
	ctx := context.Background()
	_, _ = db.Pool.Exec(ctx,
		`INSERT INTO group_memberships (group_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		groupID, userID)
	http.Redirect(w, r, "/admin/groups", http.StatusSeeOther)
}

// POST /admin/groups/{group_id}/remove-member/{user_id}
func AdminGroupsRemoveMember(w http.ResponseWriter, r *http.Request) {
	owner := requireOwner(w, r)
	if owner == nil {
		return
	}
	groupID, _ := strconv.Atoi(r.PathValue("group_id"))
	userID, _ := strconv.Atoi(r.PathValue("user_id"))
	ctx := context.Background()
	_, _ = db.Pool.Exec(ctx,
		`DELETE FROM group_memberships WHERE group_id=$1 AND user_id=$2`, groupID, userID)
	http.Redirect(w, r, "/admin/groups", http.StatusSeeOther)
}
