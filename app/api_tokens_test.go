package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAPITokenValidation(t *testing.T) {
	valid := apiTokenPrefix + strings.Repeat("a", 64)
	if !validAPIToken(valid) {
		t.Fatal("valid token rejected")
	}
	a := &application{token: "bootstrap-token"}
	for _, token := range []string{"", "short", strings.Repeat("a", 64), apiTokenPrefix + strings.Repeat("z", 64), valid + "a"} {
		if validAPIToken(token) {
			t.Fatal("malformed token accepted")
		}
		r := httptest.NewRequest("GET", "/api/me", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		// Even with a session cookie, explicit invalid credentials must fail.
		r.AddCookie(&http.Cookie{Name: a.sessionCookieName(), Value: strings.Repeat("a", 64)})
		if _, ok := a.authenticate(r); ok {
			t.Fatal("invalid bearer accepted")
		}
	}
	for _, method := range []string{"GET", "POST", "DELETE"} {
		r := httptest.NewRequest(method, "/api/api-tokens", nil)
		r = r.WithContext(context.WithValue(r.Context(), identityKey{}, identity{"user", "user"}))
		w := httptest.NewRecorder()
		a.apiTokens(w, r, []string{"api-tokens"})
		if w.Code != 403 {
			t.Fatalf("non-admin %s: %d", method, w.Code)
		}
	}
}

func TestAPITokenLifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; use a dedicated test database")
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = initializeDatabase(ctx, db); err != nil {
		t.Fatal(err)
	}
	username := "token-test-" + time.Now().Format("150405000000000")
	if _, err = db.Exec(ctx, `INSERT INTO hub_users(username,password_hash,role) VALUES($1,'unused','admin')`, username); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(ctx, `DELETE FROM hub_users WHERE username=$1`, username)
	otherAdmin := username + "-other"
	if _, err = db.Exec(ctx, `INSERT INTO hub_users(username,password_hash,role) VALUES($1,'unused','admin')`, otherAdmin); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(ctx, `DELETE FROM hub_users WHERE username=$1`, otherAdmin)
	a := &application{db: db, token: "bootstrap-token"}
	request := func(method, path, body, token string, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/api/"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.api(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: %d want %d: %s", method, path, w.Code, status, w.Body.String())
		}
		return w
	}
	request("POST", "api-tokens", `{"name":""}`, a.token, 400)
	request("POST", "api-tokens", `{"name":"automation","username":"missing-admin"}`, a.token, 400)
	w := request("POST", "api-tokens", `{"name":"automation","username":"`+username+`"}`, a.token, 201)
	var item apiToken
	if err = json.Unmarshal(w.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if !validAPIToken(item.Token) || item.Username != username {
		t.Fatal("invalid created token")
	}
	var hash string
	if err = db.QueryRow(ctx, `SELECT hash FROM hub_api_tokens WHERE id=$1`, item.ID).Scan(&hash); err != nil || hash != sessionHash(item.Token) {
		t.Fatal("token not stored hashed", err)
	}
	w = request("GET", "me", "", item.Token, 200)
	var u identity
	if err = json.Unmarshal(w.Body.Bytes(), &u); err != nil || u.Username != username || u.Role != "admin" {
		t.Fatal("wrong token identity", err)
	}
	w = request("GET", "api-tokens", "", item.Token, 200)
	if strings.Contains(w.Body.String(), item.Token) || strings.Contains(w.Body.String(), hash) {
		t.Fatal("list leaks credentials")
	}
	// Account-owned tokens can create another token without explicitly naming an owner.
	w = request("POST", "api-tokens", `{"name":"second"}`, item.Token, 201)
	var second apiToken
	if err = json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	request("DELETE", "api-tokens/"+item.ID, "", a.token, 204)
	request("GET", "me", "", item.Token, 401)
	request("DELETE", "api-tokens/"+item.ID, "", a.token, 404)
	request("PATCH", "users/"+username, `{"role":"user"}`, a.token, 200)
	request("GET", "me", "", second.Token, 401)
	request("PATCH", "users/"+username, `{"role":"admin"}`, a.token, 200)
	request("GET", "me", "", second.Token, 401)
	w = request("POST", "api-tokens", `{"name":"third","username":"`+username+`"}`, a.token, 201)
	var third apiToken
	if err = json.Unmarshal(w.Body.Bytes(), &third); err != nil {
		t.Fatal(err)
	}
	request("DELETE", "users/"+username, "", a.token, 204)
	request("GET", "me", "", third.Token, 401)
}
