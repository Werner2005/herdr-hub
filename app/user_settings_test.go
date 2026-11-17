package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUserManagementValidation(t *testing.T) {
	a := &application{token: "test-token"}
	for _, body := range []string{`{}`, `{"role":"invalid"}`, `{"password":"short"}`, `{"password":"` + strings.Repeat("x", 73) + `"}`, `{"unknown":true}`, `invalid`} {
		r := httptest.NewRequest("PATCH", "/api/users/demo", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer test-token")
		w := httptest.NewRecorder()
		a.api(w, r)
		if w.Code != 400 {
			t.Fatalf("body %s: HTTP %d, want 400", body, w.Code)
		}
	}
}
