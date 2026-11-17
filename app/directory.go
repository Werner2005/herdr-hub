package main

import (
	"net/http"
	"regexp"
	"strings"
)

var directoryQueryPattern = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// Sharing pickers expose names only. Account management stays admin-only.
func (a *application) searchDirectory(w http.ResponseWriter, r *http.Request, kind string) {
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	var sql string
	switch kind {
	case "users":
		sql = `SELECT username FROM hub_users WHERE strpos(lower(username),$1)>0 ORDER BY (strpos(lower(username),$1)=1) DESC,lower(username),username LIMIT 20`
	case "groups":
		sql = `SELECT name FROM hub_groups WHERE strpos(lower(name),$1)>0 ORDER BY (strpos(lower(name),$1)=1) DESC,lower(name),name LIMIT 20`
	default:
		writeError(w, 404, "directory not found")
		return
	}
	if query == "" {
		writeJSON(w, 200, []string{})
		return
	}
	if !directoryQueryPattern.MatchString(query) {
		writeError(w, 400, "search must contain 1–40 letters, digits or hyphens")
		return
	}
	rows, err := a.db.Query(r.Context(), sql, query)
	if err != nil {
		writeError(w, 500, "could not search directory")
		return
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var name string
		if rows.Scan(&name) != nil {
			writeError(w, 500, "could not read directory")
			return
		}
		result = append(result, name)
	}
	if rows.Err() != nil {
		writeError(w, 500, "could not read directory")
		return
	}
	writeJSON(w, 200, result)
}
