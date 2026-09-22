package server

import (
	"net/http/httptest"
	"testing"
)

// The point of moving this to the environment is that restoring somebody else's
// backup must not move where this instance says it lives. The stored value
// standing in for "the backup" is what makes that the thing under test.
func TestPublicBaseURLEnvBeatsStoredSetting(t *testing.T) {
	s := testServer(t)
	s.setSetting("public_base_url", "https://from-the-backup.example")

	if got := s.publicBaseSetting(); got != "https://from-the-backup.example" {
		t.Fatalf("without the variable the stored value must still win, got %q", got)
	}

	t.Setenv("DWORKSPACE_PUBLIC_BASE_URL", "https://this-deployment.example")
	if got := s.publicBaseSetting(); got != "https://this-deployment.example" {
		t.Fatalf("the environment must win, got %q", got)
	}

	// The links are the reason any of this matters, so check one rather than
	// trusting that the accessor is wired to them.
	r := httptest.NewRequest("GET", "http://internal.invalid/", nil)
	if got := s.publicShareBase(r); got != "https://this-deployment.example" {
		t.Fatalf("share links must follow the environment, got %q", got)
	}
	if got := s.baseURL(r); got != "https://this-deployment.example" {
		t.Fatalf("OAuth redirect base must follow the environment, got %q", got)
	}
}

// An empty variable is not a value. Treating it as one would blank the base URL
// for anyone who defines the name without filling it in — a compose file with a
// placeholder left empty is the ordinary case, not a strange one.
func TestPublicBaseURLEmptyEnvFallsBackToStored(t *testing.T) {
	s := testServer(t)
	s.setSetting("public_base_url", "https://stored.example")
	t.Setenv("DWORKSPACE_PUBLIC_BASE_URL", "")

	if got := s.publicBaseSetting(); got != "https://stored.example" {
		t.Fatalf("an empty variable must not erase the stored value, got %q", got)
	}
}
