package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestNormalizePublicKeys(t *testing.T) {
	makeKey := func() string {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		key, err := ssh.NewPublicKey(pub)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	}
	first, second := makeKey(), makeKey()
	parts := strings.Fields(first)
	wrapped := " \t" + parts[0] + "  \t" + parts[1][:20] + " \n\t" + parts[1][20:50] + "\r\n" + parts[1][50:] + "  laptop\t user  \n"
	for _, test := range []struct {
		name, input, want string
		invalid           bool
	}{
		{"single", first, first, false},
		{"key-like comment", first + " ssh-laptop", first + " ssh-laptop", false},
		{"wrapped payload", wrapped, first + " laptop user", false},
		{"multiple", wrapped + "\n" + second + " desktop", first + " laptop user\n" + second + " desktop", false},
		{"duplicates", first + " laptop\n" + first + " duplicate", first + " laptop", false},
		{"empty list", " \t\n", "", false},
		{"invalid", "ssh-ed25519 invalid", "", true},
		{"invalid second key", first + "\nssh-ed25519 invalid", "", true},
		{"private key", "-----BEGIN OPENSSH PRIVATE KEY-----", "", true},                    // gitleaks:allow -- header-only rejection fixture, no key material
		{"appended private key", first + "\n-----BEGIN OPENSSH PRIVATE KEY-----", "", true}, // gitleaks:allow -- header-only rejection fixture, no key material
		{"options", "command=foo " + first, "", true},
		{"type mismatch", "ssh-rsa " + parts[1], "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizePublicKeys(test.input)
			if (err != nil) != test.invalid || got != test.want {
				t.Fatalf("got %q, %v; want %q, invalid=%v", got, err, test.want, test.invalid)
			}
		})
	}
}
