package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type identity struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}
type identityKey struct{}

var accountNamePattern = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9-]*[A-Za-z0-9])?$`)

// Accounts and groups are database identities, not Kubernetes resource names.
func validAccountName(name string) bool {
	return len(name) <= 40 && accountNamePattern.MatchString(name)
}

func currentUser(r *http.Request) identity {
	u, _ := r.Context().Value(identityKey{}).(identity)
	return u
}
func sessionHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func (a *application) authenticate(r *http.Request) (identity, bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		if !strings.HasPrefix(h, "Bearer ") {
			return identity{}, false
		}
		token := strings.TrimPrefix(h, "Bearer ")
		if a.token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(a.token)) == 1 {
			return identity{"", "admin"}, true
		}
		if !validAPIToken(token) {
			return identity{}, false
		}
		var u identity
		err := a.db.QueryRow(r.Context(), `SELECT u.username,u.role FROM hub_api_tokens t JOIN hub_users u ON u.username=t.username WHERE t.hash=$1 AND u.role='admin'`, sessionHash(token)).Scan(&u.Username, &u.Role)
		return u, err == nil
	}
	c, err := r.Cookie(a.sessionCookieName())
	if err != nil {
		return identity{}, false
	}
	if len(c.Value) != 64 {
		return identity{}, false
	}
	if _, err = hex.DecodeString(c.Value); err != nil {
		return identity{}, false
	}
	var u identity
	err = a.db.QueryRow(r.Context(), `SELECT u.username,u.role FROM hub_sessions s JOIN hub_users u ON u.username=s.username WHERE s.hash=$1 AND s.expires_at>now()`, sessionHash(c.Value)).Scan(&u.Username, &u.Role)
	return u, err == nil
}
func (a *application) bootstrapAdmin(ctx context.Context, username, password string) error {
	if username == "" || password == "" {
		if username != "" || password != "" {
			return fmt.Errorf("both HUB_ADMIN_USERNAME and HUB_ADMIN_PASSWORD are required for bootstrap")
		}
		return nil
	}
	if !validAccountName(username) || len(password) < 12 || len(password) > 72 {
		return fmt.Errorf("bootstrap admin needs a username starting with a letter, containing letters, digits or internal hyphens (max 40 characters), and a 12–72 byte password")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = a.db.Exec(ctx, `INSERT INTO hub_users(username,password_hash,role) VALUES($1,$2,'admin') ON CONFLICT DO NOTHING`, username, string(hash))
	return err
}
func (a *application) login(w http.ResponseWriter, r *http.Request) {
	recorder := &auditResponse{ResponseWriter: w}
	w = recorder
	var attempted identity
	defer func() { a.audit(r, attempted, recorder.status) }()
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input) != nil {
		writeError(w, 400, "invalid login")
		return
	}
	if validAccountName(input.Username) {
		attempted.Username = input.Username
	}
	if !a.loginLimits.beginAccount(input.Username, time.Now()) {
		loginThrottled(w)
		return
	}
	defer a.loginLimits.done()
	var u identity
	var hash string
	err := a.db.QueryRow(r.Context(), `SELECT username,role,password_hash FROM hub_users WHERE username=$1`, input.Username).Scan(&u.Username, &u.Role, &hash)
	// Always perform a password check, including for unknown usernames.
	if err != nil {
		hash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
	}
	check := bcrypt.CompareHashAndPassword([]byte(hash), []byte(input.Password))
	if err != nil || check != nil {
		writeError(w, 401, "invalid username or password")
		return
	}
	// Password verification is expensive and stays outside the mutation lock.
	// Recheck under the lock so account deletion/reset cannot race session creation.
	if err = a.mu.lock(r.Context()); err != nil {
		writeError(w, 503, "request expired waiting for another operation")
		return
	}
	defer a.mu.unlock()
	var currentHash string
	err = a.db.QueryRow(r.Context(), `SELECT username,role,password_hash FROM hub_users WHERE username=$1`, input.Username).Scan(&u.Username, &u.Role, &currentHash)
	if err != nil || currentHash != hash {
		writeError(w, 401, "invalid username or password")
		return
	}
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		writeError(w, 500, "could not create session")
		return
	}
	token := hex.EncodeToString(b)
	_, err = a.db.Exec(r.Context(), `DELETE FROM hub_sessions WHERE expires_at<=now()`)
	if err == nil {
		_, err = a.db.Exec(r.Context(), `INSERT INTO hub_sessions(hash,username,expires_at) VALUES($1,$2,$3)`, sessionHash(token), u.Username, time.Now().Add(12*time.Hour))
	}
	if err != nil {
		writeError(w, 500, "could not create session")
		return
	}
	http.SetCookie(w, a.sessionCookie(token, 43200))
	attempted = u
	writeJSON(w, 200, u)
}
func (a *application) users(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		rows, err := a.db.Query(r.Context(), `SELECT username,role FROM hub_users ORDER BY username`)
		if err != nil {
			writeError(w, 500, "could not list users")
			return
		}
		defer rows.Close()
		result := []identity{}
		for rows.Next() {
			var u identity
			if rows.Scan(&u.Username, &u.Role) != nil {
				writeError(w, 500, "could not read users")
				return
			}
			result = append(result, u)
		}
		if rows.Err() != nil {
			writeError(w, 500, "could not read users")
			return
		}
		writeJSON(w, 200, result)
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input) != nil || !validAccountName(input.Username) || len(input.Password) < 12 || len(input.Password) > 72 || (input.Role != "admin" && input.Role != "user") {
		writeError(w, 400, "use a username starting with a letter, containing letters, digits or internal hyphens (max 40 characters), a 12–72 byte password, and admin or user role")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, 500, "password hashing failed")
		return
	}
	if _, err = a.db.Exec(r.Context(), `INSERT INTO hub_users(username,password_hash,role) VALUES($1,$2,$3)`, input.Username, string(hash), input.Role); err != nil {
		writeError(w, 409, "username already exists or database unavailable")
		return
	}
	writeJSON(w, 201, identity{input.Username, input.Role})
}
