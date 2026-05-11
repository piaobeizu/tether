package oauth_test

import (
	"errors"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/auth/oauth"
)

func TestCodeStore_PendingHappyPath(t *testing.T) {
	s := oauth.NewCodeStore()
	reqID, err := s.StorePending("cursor", "http://localhost:1234/cb", "challenge", "mcp", "state1")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.ConsumePending(reqID)
	if err != nil {
		t.Fatalf("ConsumePending: %v", err)
	}
	if p.ClientID != "cursor" || p.RedirectURI != "http://localhost:1234/cb" {
		t.Errorf("unexpected pending: %+v", p)
	}
	// Second consume must fail (single-use).
	if _, err := s.ConsumePending(reqID); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("second consume: want ErrNotFound, got %v", err)
	}
}

func TestCodeStore_PendingExpiry(t *testing.T) {
	s := oauth.NewCodeStoreWithTTL(-time.Second) // already expired
	reqID, err := s.StorePending("goose", "http://localhost:5678/cb", "c", "mcp", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumePending(reqID); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("want ErrNotFound for expired pending, got %v", err)
	}
}

func TestCodeStore_AuthCodeHappyPath(t *testing.T) {
	s := oauth.NewCodeStore()
	p := oauth.PendingRequest{
		ClientID:    "cursor",
		RedirectURI: "http://localhost:1234/cb",
		Challenge:   "challenge",
		Scope:       "mcp",
		State:       "s",
	}
	code, err := s.StoreCode(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ConsumeCode(code)
	if err != nil {
		t.Fatalf("ConsumeCode: %v", err)
	}
	if got.ClientID != "cursor" {
		t.Errorf("unexpected client_id: %s", got.ClientID)
	}
	// Replay must fail.
	if _, err := s.ConsumeCode(code); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("replay: want ErrNotFound, got %v", err)
	}
}

func TestCodeStore_AuthCodeExpiry(t *testing.T) {
	s := oauth.NewCodeStoreWithTTL(-time.Second)
	p := oauth.PendingRequest{ClientID: "x", RedirectURI: "http://localhost/cb", Challenge: "c", Scope: "mcp"}
	code, _ := s.StoreCode(p)
	if _, err := s.ConsumeCode(code); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("want ErrNotFound for expired code, got %v", err)
	}
}

func TestCodeStore_UnknownReqID(t *testing.T) {
	s := oauth.NewCodeStore()
	if _, err := s.ConsumePending("doesnotexist"); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}
