package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccountNames(t *testing.T) {
	for _, name := range []string{"Alice", "PROJECT-Team", "Admin1", "A", "alice", strings.Repeat("A", 40)} {
		if !validAccountName(name) {
			t.Errorf("account/group name rejected: %q", name)
		}
	}
	for _, name := range []string{"", "1Alice", "Alice-", "-Alice", "Alice Bob", "Alice/Bob", "Alice_Bob", strings.Repeat("A", 41)} {
		if validAccountName(name) {
			t.Errorf("invalid account/group name accepted: %q", name)
		}
	}
	if validNames([]string{"Alice", "Alice"}, validAccountName) {
		t.Error("duplicate names accepted")
	}
	if !validNames([]string{"Alice", "Team"}, validAccountName) {
		t.Error("mixed-case sharing names rejected")
	}
	if validNames([]string{"Data"}, validName) {
		t.Error("uppercase PVC reference accepted")
	}
	for _, name := range []string{"Alice", "PROJECT-Team"} {
		if validName(name) {
			t.Errorf("uppercase Kubernetes name accepted: %q", name)
		}
	}
}

func exerciseMixedCaseIdentities(t *testing.T, a *application, admin func(string, string, string) *httptest.ResponseRecorder, session func(*http.Cookie, string, string, string) *httptest.ResponseRecorder, volumeOwner *http.Cookie) {
	t.Helper()
	defer a.db.Exec(context.Background(), `DELETE FROM hub_groups WHERE name='Mixed-Team'; DELETE FROM hub_users WHERE username IN ('Test-Mixed','Mixed-Admin')`)
	expect := func(w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("mixed-case HTTP %d want %d: %s", w.Code, status, w.Body.String())
		}
	}
	login := func(name, password string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		a.api(w, httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"`+name+`","password":"`+password+`"}`)))
		return w
	}
	expect(admin("POST", "users", `{"username":"Test-Mixed","password":"test-password-123","role":"user"}`), 201)
	if err := a.bootstrapAdmin(context.Background(), "Mixed-Admin", "test-password-123"); err != nil {
		t.Fatal(err)
	}
	expect(login("Mixed-Admin", "test-password-123"), 200)
	expect(login("test-mixed", "test-password-123"), 401)
	loggedIn := login("Test-Mixed", "test-password-123")
	expect(loggedIn, 200)
	cookie := loggedIn.Result().Cookies()[0]
	expect(session(cookie, "POST", "groups", `{"name":"Mixed-Team"}`), 201)
	expect(session(cookie, "PUT", "groups/Mixed-Team/members", `{"members":["Test-Mixed","test-other"]}`), 200)
	for _, query := range []string{"mixed", "MIXED"} {
		users := session(cookie, "GET", "directory/users?q="+query, "")
		expect(users, 200)
		if !strings.Contains(users.Body.String(), `"Test-Mixed"`) || strings.Contains(users.Body.String(), `"test-mixed"`) {
			t.Fatal("search did not preserve saved username", users.Body.String())
		}
		groups := session(cookie, "GET", "directory/groups?q="+query, "")
		expect(groups, 200)
		if strings.TrimSpace(groups.Body.String()) != `["Mixed-Team"]` {
			t.Fatal("search did not find mixed-case group", groups.Body.String())
		}
	}
	expect(session(volumeOwner, "PUT", "volumes/shared-test/shares", `{"users":[],"groups":["Mixed-Team"]}`), 200)
	allowed, err := a.volumeAccess(context.Background(), "shared-test", "Test-Mixed")
	if err != nil || !allowed {
		t.Fatal("mixed-case group owner has no volume access", err)
	}
	expect(session(volumeOwner, "PUT", "volumes/shared-test/shares", `{"users":["Test-Mixed"],"groups":[]}`), 200)
	allowed, err = a.volumeAccess(context.Background(), "shared-test", "Test-Mixed")
	if err != nil || !allowed {
		t.Fatal("mixed-case direct share has no volume access", err)
	}
	expect(admin("PATCH", "workspaces/demo/owner", `{"owner":"Test-Mixed"}`), 200)
	expect(session(cookie, "GET", "workspaces/demo/token", ""), 200)
	expect(admin("PATCH", "workspaces/demo/owner", `{"owner":"test-user"}`), 200)
	expect(admin("POST", "workspaces/demo/deploy", ""), 201)
	expect(session(cookie, "DELETE", "groups/Mixed-Team", ""), 204)
	expect(session(volumeOwner, "PUT", "volumes/shared-test/shares", `{"users":[],"groups":[]}`), 200)
	expect(admin("PATCH", "users/Test-Mixed", `{"password":"reset-password-123"}`), 200)
	expect(login("Test-Mixed", "reset-password-123"), 200)
	expect(admin("DELETE", "users/Test-Mixed", ""), 204)
	expect(admin("DELETE", "users/Mixed-Admin", ""), 204)
}
