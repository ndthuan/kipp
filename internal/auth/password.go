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

// The argon2id parameters a new hash is made with: 19 MiB, 2 passes, 1
// lane. That is the row "m=19456 (19 MiB), t=2, p=1" of the OWASP Password
// Storage Cheat Sheet, the one it names as the minimum configuration (read
// 2026-10-11). The login form is open to anybody, so what one check holds
// in memory is what a flood of them costs the server: the rows the sheet
// gives are equal in strength and this is the one of two passes or more
// that holds least.
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

// Upper bounds a stored hash may ask for: what a hash was once made with
// (64 MiB, 3 passes, 2 lanes), which data directories still hold. They are
// apart from the parameters above so that lowering those never makes a
// stored hash unreadable.
const (
	maxMemory  = 64 * 1024
	maxTime    = 3
	maxThreads = 2
)

// dummyHash has the real parameters; Login verifies against it for an unknown name.
const dummyHash = "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// current reports whether a stored hash was made with the parameters a new
// one is made with.
func current(encoded string) bool {
	m, t, p, _, _, ok := parseHash(encoded)
	return ok && m == argonMemory && t == argonTime && p == argonThreads
}

// parseHash rejects a hash whose parameters argon2 would panic on or that ask for more than we ever set.
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

// Verifying a password takes what its hash asks for (19 MiB, or 64 MiB for
// one made before) for as long as it runs, and anybody may send a login:
// verifySlots is how many run at once, and so what logins can cost the
// server in memory, and verifyWait how long one waits for a slot before it
// is told to come back. A hash that asks for more memory than a new one
// takes heavySlot first, so that only one of those is ever among them: a
// user who has not logged in since the parameters came down must not be a
// name that makes every check cost the old amount.
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

// verifyPassword is VerifyPassword for an attempt that may, when reserved,
// take reservedSlot too.
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

// take fills a place in slots, or says why it did not: the wait ran out,
// or ctx ended.
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

// takeVerify fills a place to verify in: one of verifySlots, or, when
// reserved, reservedSlot, whichever is free first. It returns the one it
// filled.
func takeVerify(ctx context.Context, wait *time.Timer, reserved bool) (chan struct{}, error) {
	var kept chan struct{} // nil: never ready
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
