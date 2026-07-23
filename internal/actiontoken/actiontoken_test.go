package actiontoken

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestLoadOrCreateSecret(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreateSecret(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateSecret() error = %v", err)
	}

	if len(first) != secretSize {
		t.Errorf("secret length = %d, want %d", len(first), secretSize)
	}

	second, err := LoadOrCreateSecret(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateSecret() second call error = %v", err)
	}

	if string(first) != string(second) {
		t.Error("LoadOrCreateSecret() returned a different secret on the second call, want it reloaded from disk")
	}

	info, err := os.Stat(filepath.Join(dir, secretFileName))
	if err != nil {
		t.Fatalf("stat secret file: %v", err)
	}

	if info.Mode().Perm() != secretFileMode {
		t.Errorf("secret file mode = %v, want %v", info.Mode().Perm(), os.FileMode(secretFileMode))
	}
}

func TestMintAndVerify(t *testing.T) {
	secret := []byte("test-secret-32-bytes-long-abcde")

	token, err := Mint(secret, "abc123", "postpone")
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}

	claims, err := Verify(secret, token, "abc123", "postpone")
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}

	if claims.ReminderID != "abc123" || claims.Action != "postpone" {
		t.Errorf("claims = %+v, want fields matching what was minted", claims)
	}

	if claims.ID == "" {
		t.Error("claims.ID (jti) is empty, want a random id so UsedTracker can dedupe")
	}
}

func TestVerify_WrongReminderID(t *testing.T) {
	secret := []byte("test-secret")
	token, _ := Mint(secret, "abc123", "postpone")

	if _, err := Verify(secret, token, "different-id", "postpone"); err == nil {
		t.Error("Verify() error = nil, want an error for a mismatched reminder id")
	}
}

func TestVerify_WrongAction(t *testing.T) {
	secret := []byte("test-secret")
	token, _ := Mint(secret, "abc123", "postpone")

	if _, err := Verify(secret, token, "abc123", "cancel"); err == nil {
		t.Error("Verify() error = nil, want an error for a mismatched action")
	}
}

func TestVerify_WrongSecret(t *testing.T) {
	token, _ := Mint([]byte("real-secret"), "abc123", "postpone")

	if _, err := Verify([]byte("wrong-secret"), token, "abc123", "postpone"); err == nil {
		t.Error("Verify() error = nil, want an error for a token signed with a different secret")
	}
}

func TestVerify_Expired(t *testing.T) {
	secret := []byte("test-secret")
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
		},
		ReminderID: "abc123",
		Action:     "postpone",
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
	if err != nil {
		t.Fatalf("signing expired token: %v", err)
	}

	if _, err := Verify(secret, token, "abc123", "postpone"); err == nil {
		t.Error("Verify() error = nil, want an error for an expired token")
	}
}

func TestVerify_RejectsAlgNone(t *testing.T) {
	secret := []byte("test-secret")
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
		ReminderID:       "abc123",
		Action:           "postpone",
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)

	if err != nil {
		t.Fatalf("signing alg=none token: %v", err)
	}

	if _, err := Verify(secret, token, "abc123", "postpone"); err == nil {
		t.Error("Verify() error = nil, want alg=none tokens rejected regardless of the secret")
	} else if !strings.Contains(err.Error(), "invalid action token") {
		t.Errorf("Verify() error = %q, want it to say the token itself is invalid", err)
	}
}

func TestUsedTracker(t *testing.T) {
	t.Run("first use is accepted, replay is rejected", func(t *testing.T) {
		tr := NewUsedTracker()
		exp := time.Now().Add(time.Hour)

		if !tr.MarkUsed("jti-1", exp) {
			t.Fatal("MarkUsed() first call = false, want true")
		}

		if tr.MarkUsed("jti-1", exp) {
			t.Error("MarkUsed() replay of the same jti = true, want false (already used)")
		}
	})

	t.Run("different jtis don't interfere with each other", func(t *testing.T) {
		tr := NewUsedTracker()
		exp := time.Now().Add(time.Hour)

		if !tr.MarkUsed("jti-a", exp) {
			t.Fatal("MarkUsed(jti-a) = false, want true")
		}

		if !tr.MarkUsed("jti-b", exp) {
			t.Error("MarkUsed(jti-b) = false, want true (unrelated jti, first use)")
		}
	})

	t.Run("an expired entry is swept and its jti can be reused", func(t *testing.T) {
		tr := NewUsedTracker()
		alreadyExpired := time.Now().Add(-time.Minute)

		if !tr.MarkUsed("jti-1", alreadyExpired) {
			t.Fatal("MarkUsed() first call = false, want true")
		}

		if !tr.MarkUsed("jti-1", time.Now().Add(time.Hour)) {
			t.Error("MarkUsed() after the prior entry expired = false, want true (swept, not still blocked)")
		}
	})
}
