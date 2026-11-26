package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/ndthuan/kipp/internal/runnerproto"
)

const (
	argonMemory  = 19456
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
)

// HashPassword returns the encoded argon2id hash with a fresh salt.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

const (
	maxMemory  = 64 * 1024
	maxTime    = 3
	maxThreads = 2
)

const dummyHash = "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func current(encoded string) bool {
	m, t, p, _, _, ok := parseHash(encoded)
	return ok && m == argonMemory && t == argonTime && p == argonThreads
}

func parseHash(encoded string) (m, t uint32, p uint8, salt, key []byte, ok bool) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return 0, 0, 0, nil, nil, false
	}
	if parts[2] != "v=19" {
		return 0, 0, 0, nil, nil, false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil ||
		parts[3] != fmt.Sprintf("m=%d,t=%d,p=%d", m, t, p) {
		return 0, 0, 0, nil, nil, false
	}
	if m < 8 || m > maxMemory || t < 1 || t > maxTime || p < 1 || p > maxThreads {
		return 0, 0, 0, nil, nil, false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return 0, 0, 0, nil, nil, false
	}
	key, err = base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) != argonKeyLen {
		return 0, 0, 0, nil, nil, false
	}
	return m, t, p, salt, key, true
}

// ErrBusy answers a password that found no free slot to be verified in.
var ErrBusy = errors.New("too many logins at once; try again in a moment")

var (
	verifySlots  = make(chan struct{}, 2)
	reservedSlot = make(chan struct{}, 1)
	heavySlot    = make(chan struct{}, 1)
	verifyWait   = 2 * time.Second
	deriveKey    = argon2.IDKey
)

// VerifyPassword compares in constant time; a malformed hash is false. The
// error is ErrBusy, or ctx's if it ends while the password waits its turn.
func VerifyPassword(ctx context.Context, encoded, pw string) (bool, error) {
	return verifyPassword(ctx, encoded, pw, false)
}

func verifyPassword(ctx context.Context, encoded, pw string, reserved bool) (bool, error) {
	m, t, p, salt, want, ok := parseHash(encoded)
	if !ok {
		return false, nil
	}
	wait := time.NewTimer(verifyWait)
	defer wait.Stop()
	if m > argonMemory {
		if err := take(ctx, heavySlot, wait); err != nil {
			return false, err
		}
		defer func() { <-heavySlot }()
	}
	slot, err := takeVerify(ctx, wait, reserved)
	if err != nil {
		return false, err
	}
	defer func() { <-slot }()
	got := deriveKey([]byte(pw), salt, t, m, p, argonKeyLen)
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func take(ctx context.Context, slots chan struct{}, wait *time.Timer) error {
	select {
	case slots <- struct{}{}:
		return nil
	case <-wait.C:
		return ErrBusy
	case <-ctx.Done():
		return ctx.Err()
	}
}

func takeVerify(ctx context.Context, wait *time.Timer, reserved bool) (chan struct{}, error) {
	var kept chan struct{}
	if reserved {
		kept = reservedSlot
	}
	select {
	case verifySlots <- struct{}{}:
		return verifySlots, nil
	case kept <- struct{}{}:
		return reservedSlot, nil
	case <-wait.C:
		return nil, ErrBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// NewID is 32 random bytes, base64url, for sessions and CSRF.
func NewID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewToken makes an API token and the hash the store keeps.
func NewToken() (plain, hash string, err error) {
	plain, err = NewID()
	if err != nil {
		return "", "", err
	}
	return "kipp_" + plain, HashToken("kipp_" + plain), nil
}

// NewRunnerToken makes a runner's token and the hash the server keeps. It
// is told from an API token by its prefix, and is no API token: it reaches
// /runner/ and nothing else.
func NewRunnerToken() (plain, hash string, err error) {
	plain, err = NewID()
	if err != nil {
		return "", "", err
	}
	plain = runnerproto.TokenPrefix + plain
	return plain, HashToken(plain), nil
}

// HashToken is sha256 hex; tokens are random, so no salt is needed.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}
