// Package actiontoken mints and verifies the short-lived credentials
// embedded in a fired notification's ntfy action buttons. A token grants
// "perform this one action on this one reminder, until it expires" —
// nothing else. It carries no operational parameters (e.g. how long to
// snooze by).
package actiontoken

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	secretFileName = "action_secret"
	secretFileMode = 0600
	secretSize     = 32
	jtiSize        = 16
	expiry         = 72 * time.Hour
)

type Claims struct {
	jwt.RegisteredClaims
	ReminderID string `json:"reminder_id"`
	Action     string `json:"action"`
}

// LoadOrCreateSecret returns the signing secret from
// <dataDir>/action_secret, generating and persisting a new
// random one on first run.
func LoadOrCreateSecret(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, secretFileName)

	secret, err := os.ReadFile(path)
	if err == nil {
		return secret, nil
	}

	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading action secret: %w", err)
	}

	secret = make([]byte, secretSize)
	if _, err := rand.Read(secret); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}

	if err := os.WriteFile(path, secret, secretFileMode); err != nil {
		return nil, fmt.Errorf("writing action secret: %w", err)
	}

	return secret, nil
}

// Mint signs a token scoped to reminderID/action, valid for 72 hours.
// Reminders only ever fire once (see internal/scheduler) with no retry, so
// this has to stay long enough to cover a notification going unseen over a
// weekend; shorter would silently kill the buttons with no way to retry
// beyond falling back to the CLI/API. Carries a random jti so a
// UsedTracker can enforce single use despite the long expiry - see
// UsedTracker's doc comment.
func Mint(secret []byte, reminderID, action string) (string, error) {
	jti, err := newJTI()
	if err != nil {
		return "", fmt.Errorf("generating token id: %w", err)
	}

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(expiry)),
		},
		ReminderID: reminderID,
		Action:     action,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
}

func newJTI() (string, error) {
	b := make([]byte, jtiSize)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Verify checks tokenString's signature and expiry, then confirms it's
// scoped to wantReminderID/wantAction before returning its claims.
func Verify(secret []byte, tokenString, wantReminderID, wantAction string) (*Claims, error) {
	var claims Claims
	_, err := jwt.ParseWithClaims(tokenString, &claims, func(*jwt.Token) (any, error) {
		return secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))

	if err != nil {
		return nil, fmt.Errorf("invalid action token: %w", err)
	}

	if claims.ReminderID != wantReminderID {
		return nil, fmt.Errorf("action token is scoped to reminder %s, not %s", claims.ReminderID, wantReminderID)
	}

	if claims.Action != wantAction {
		return nil, fmt.Errorf("action token is scoped to action %q, not %q", claims.Action, wantAction)
	}

	return &claims, nil
}

// UsedTokenTracker enforces single-use action tokens in memory: two of ntfy's
// own action buttons share one minted token (only the duration query
// param differs), and a notification can be delivered to more than one
// subscriber/device on the same topic, so clear=true alone can't prevent
// a second tap - on another device, or the other button on the same
// notification - from postponing the same reminder twice. Deliberately
// not persisted: a restart losing track of a few recently-used tokens
// within their remaining window is an acceptable, low-stakes gap for a
// homelab app, and not worth a datastore for.
type UsedTokenTracker struct {
	mu   sync.Mutex
	used map[string]time.Time
}

func NewUsedTracker() *UsedTokenTracker {
	return &UsedTokenTracker{used: make(map[string]time.Time)}
}

// MarkUsed records jti as used until expiresAt and reports whether this is
// the first time it's been seen; a jti already marked used (and not yet
// expired) returns false. No background cleanup loop: expired entries are
// opportunistically swept on every call instead, since the map only ever
// holds tokens still within their (at most 72h) expiry, which stays small
// at homelab scale.
func (t *UsedTokenTracker) MarkUsed(jti string, expiresAt time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	for id, exp := range t.used {
		if now.After(exp) {
			delete(t.used, id)
		}
	}

	if exp, seen := t.used[jti]; seen && now.Before(exp) {
		return false
	}
	t.used[jti] = expiresAt
	return true
}
