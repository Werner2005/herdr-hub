package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

func (a *application) manageUser(w http.ResponseWriter, r *http.Request, username string) {
	var input struct {
		Role     *string `json:"role"`
		Password *string `json:"password"`
	}
	deleting := r.Method == "DELETE"
	if !deleting {
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || (input.Role == nil && input.Password == nil) {
			writeError(w, 400, "provide a role or password")
			return
		}
		if input.Role != nil && *input.Role != "admin" && *input.Role != "user" {
			writeError(w, 400, "role must be admin or user")
			return
		}
		if input.Password != nil && (len(*input.Password) < 12 || len(*input.Password) > 72) {
			writeError(w, 400, "password must be 12–72 bytes")
			return
		}
	}
	if username == currentUser(r).Username && (deleting || (input.Role != nil && *input.Role != "admin")) {
		writeError(w, 409, "you cannot delete or demote your own account")
		return
	}
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "database unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var role string
	err = tx.QueryRow(r.Context(), `SELECT role FROM hub_users WHERE username=$1 FOR UPDATE`, username).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "user not found")
		return
	}
	if err != nil {
		writeError(w, 500, "could not load user")
		return
	}
	if role == "admin" && (deleting || (input.Role != nil && *input.Role == "user")) {
		var count int
		if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM hub_users WHERE role='admin'`).Scan(&count); err != nil {
			writeError(w, 500, "could not check administrators")
			return
		}
		if count <= 1 {
			writeError(w, 409, "at least one admin account must remain")
			return
		}
	}
	if deleting {
		var assigned bool
		if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM workspaces WHERE owner=$1)`, username).Scan(&assigned); err != nil {
			writeError(w, 500, "could not check workspace ownership")
			return
		}
		if assigned {
			writeError(w, 409, "reassign this user's workspaces before deleting the account")
			return
		}
		if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM shared_volumes WHERE owner=$1) OR EXISTS(SELECT 1 FROM hub_groups WHERE owner=$1) OR EXISTS(SELECT 1 FROM configurations WHERE owner=$1)`, username).Scan(&assigned); err != nil {
			writeError(w, 500, "could not check shared storage ownership")
			return
		}
		if assigned {
			writeError(w, 409, "delete this user's shared volumes, groups and configurations before deleting the account")
			return
		}
		_, err = tx.Exec(r.Context(), `DELETE FROM hub_users WHERE username=$1`, username)
	} else {
		if input.Role != nil {
			role = *input.Role
			_, err = tx.Exec(r.Context(), `UPDATE hub_users SET role=$2 WHERE username=$1`, username, role)
			if err == nil && role != "admin" {
				_, err = tx.Exec(r.Context(), `DELETE FROM hub_api_tokens WHERE username=$1`, username)
			}
		}
		if err == nil && input.Password != nil {
			var hash []byte
			hash, err = bcrypt.GenerateFromPassword([]byte(*input.Password), bcrypt.DefaultCost)
			if err == nil {
				_, err = tx.Exec(r.Context(), `UPDATE hub_users SET password_hash=$2 WHERE username=$1`, username, string(hash))
			}
			if err == nil {
				keepSession := ""
				if username == currentUser(r).Username {
					if cookie, e := r.Cookie(a.sessionCookieName()); e == nil {
						keepSession = sessionHash(cookie.Value)
					}
				}
				_, err = tx.Exec(r.Context(), `DELETE FROM hub_sessions WHERE username=$1 AND hash<>$2`, username, keepSession)
			}
		}
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, 500, "could not save user changes")
		return
	}
	if deleting {
		w.WriteHeader(204)
		return
	}
	writeJSON(w, 200, identity{username, role})
}
