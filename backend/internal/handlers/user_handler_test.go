package handlers

import "testing"

func TestValidateImportedUserAllowsEmptyPassword(t *testing.T) {
	if err := validateImportedUser("alice", "alice@example.com", "", "user"); err != "" {
		t.Fatalf("expected empty import password to be allowed, got %q", err)
	}
}

func TestValidateImportedUserAllowsDottedUsername(t *testing.T) {
	if err := validateImportedUser("alice.smith", "alice.smith@example.com", "", "user"); err != "" {
		t.Fatalf("expected dotted username to be allowed, got %q", err)
	}
}

func TestValidateImportedUserAllowsUnrestrictedUsernameCharacters(t *testing.T) {
	if err := validateImportedUser("alice_smith+ops/team", "alice@example.com", "", "user"); err != "" {
		t.Fatalf("expected unrestricted username characters to be allowed, got %q", err)
	}
}

func TestValidateImportedUserRejectsEmptyUsername(t *testing.T) {
	if err := validateImportedUser("", "alice@example.com", "", "user"); err != "Username is required" {
		t.Fatalf("expected empty username error, got %q", err)
	}
}

func TestValidateImportedUserRejectsShortExplicitPassword(t *testing.T) {
	if err := validateImportedUser("alice", "alice@example.com", "user123", "user"); err != "Password must be at least 8 characters" {
		t.Fatalf("expected short explicit password error, got %q", err)
	}
}
