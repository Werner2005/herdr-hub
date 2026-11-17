package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const apiTokenPrefix = "herdr_"

type apiToken struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
	Token     string    `json:"token,omitempty"`
}

func validAPIToken(token string) bool {
	if !strings.HasPrefix(token, apiTokenPrefix) || len(token) != len(apiTokenPrefix)+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(token, apiTokenPrefix))
	return err == nil
}

func (a *application) apiTokens(w http.ResponseWriter, r *http.Request, parts []string) {
	if currentUser(r).Role != "admin" {
		writeError(w, 403, "admin role required")
		return
	}
	if len(parts) == 1 && r.Method == "GET" {
		rows, err := a.db.Query(r.Context(), `SELECT id,name,username,created_at FROM hub_api_tokens ORDER BY created_at,id`)
		if err != nil {
			writeError(w, 500, "could not list API tokens")
			return
		}
		defer rows.Close()
		result := []apiToken{}
		for rows.Next() {
			var item apiToken
			if rows.Scan(&item.ID, &item.Name, &item.Username, &item.CreatedAt) != nil {
				writeError(w, 500, "could not read API tokens")
				return
			}
			result = append(result, item)
		}
		if rows.Err() != nil {
			writeError(w, 500, "could not read API tokens")
			return
		}
		writeJSON(w, 200, result)
		return
	}
	if len(parts) == 1 && r.Method == "POST" {
		var input struct {
			Name     string `json:"name"`
			Username string `json:"username"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input) != nil {
			writeError(w, 400, "invalid API token request")
			return
		}
		input.Name = strings.TrimSpace(input.Name)
		if input.Username == "" {
			input.Username = currentUser(r).Username
		}
		if len(input.Name) == 0 || len(input.Name) > 100 || !validAccountName(input.Username) {
			writeError(w, 400, "provide a token name (1–100 bytes) and an admin username")
			return
		}
		var random [48]byte
		if _, err := rand.Read(random[:]); err != nil {
			writeError(w, 500, "could not generate API token")
			return
		}
		item := apiToken{ID: hex.EncodeToString(random[:16]), Name: input.Name, Username: input.Username, Token: apiTokenPrefix + hex.EncodeToString(random[16:])}
		err := a.db.QueryRow(r.Context(), `INSERT INTO hub_api_tokens(id,hash,name,username) SELECT $1,$2,$3,username FROM hub_users WHERE username=$4 AND role='admin' RETURNING created_at`, item.ID, sessionHash(item.Token), item.Name, item.Username).Scan(&item.CreatedAt)
		if err == pgx.ErrNoRows {
			writeError(w, 400, "token owner must be an existing admin")
			return
		}
		if err != nil {
			writeError(w, 500, "could not create API token")
			return
		}
		writeJSON(w, 201, item)
		return
	}
	if len(parts) == 2 && r.Method == "DELETE" {
		result, err := a.db.Exec(r.Context(), `DELETE FROM hub_api_tokens WHERE id=$1`, parts[1])
		if err != nil {
			writeError(w, 500, "could not revoke API token")
			return
		}
		if result.RowsAffected() == 0 {
			writeError(w, 404, "API token not found")
			return
		}
		w.WriteHeader(204)
		return
	}
	writeError(w, 404, "API token endpoint not found")
}
