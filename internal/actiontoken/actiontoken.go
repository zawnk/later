// Package actiontoken mints and verifies the short-lived credentials
// embedded in a fired notification's ntfy action buttons. A token grants
// "perform this one action on this one reminder, until it expires" —
// nothing else. It carries no operational parameters (e.g. how long to
// snooze by).
package actiontoken

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	secretFileName = "action_secret"
	secretFileMode = 0600
	secretSize     = 32
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
// beyond falling back to the CLI/API.
func Mint(secret []byte, reminderID, action string) (string, error) {
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(expiry)),
		},
		ReminderID: reminderID,
		Action:     action,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
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
