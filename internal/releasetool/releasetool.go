// Package releasetool is the behaviour of cmd/kipp-release: the key
// generator, the signing of a release's SHA256SUMS and the check of one.
package releasetool

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ndthuan/kipp/internal/update"
)

const usage = `usage: kipp-release <command> [flags]

  keygen -secret <path> [-pub internal/update/release.pub]
      make a key pair; the secret key goes to <path> (mode 0600, never
      overwritten), the public key to the -pub file
  sign -sums SHA256SUMS [-key <path> | -key-env NAME] [-out SHA256SUMS.sig]
      sign the list of checksums; the key is read from the file, or from
      the environment variable (default RELEASE_SIGNING_KEY)
  verify -sums SHA256SUMS [-sig SHA256SUMS.sig] [-archive <file>] [-pub <file>]
      check the signature against the public key compiled in (or -pub), and
      the archive, by its base name, against the list
`

// Run runs one command and returns the exit code. pub is the public key
// file verify uses when it is not given -pub.
func Run(args []string, stdout, stderr io.Writer, getenv func(string) string, pub []byte) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "keygen":
		err = keygen(args[1:], stdout)
	case "sign":
		err = sign(args[1:], stdout, getenv)
	case "verify":
		err = verify(args[1:], stdout, pub)
	default:
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	}
	_, _ = fmt.Fprintln(stderr, "kipp-release:", err)
	return 1
}

func flags(name string, args []string) (*flag.FlagSet, func() error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs, func() error { return fs.Parse(args) }
}

func keygen(args []string, stdout io.Writer) error {
	fs, parse := flags("keygen", args)
	secret := fs.String("secret", "", "where the secret key is written")
	pubPath := fs.String("pub", filepath.Join("internal", "update", "release.pub"), "where the public key is written")
	if err := parse(); err != nil {
		return err
	}
	if *secret == "" {
		return errors.New("keygen: -secret <path> is required")
	}
	pub, sec, err := update.GenerateKey()
	if err != nil {
		return err
	}
	// O_EXCL: a key already there is never replaced, and the mode is set at
	// creation, so the key is never readable by others even for a moment.
	f, err := os.OpenFile(*secret, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(sec.Marshal()); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.WriteFile(*pubPath, pub.Marshal(), 0o600); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "public key written to %s\nsecret key written to %s (keep it out of the repository)\n", *pubPath, *secret)
	return nil
}

func sign(args []string, stdout io.Writer, getenv func(string) string) error {
	fs, parse := flags("sign", args)
	sums := fs.String("sums", "SHA256SUMS", "the list of checksums to sign")
	keyPath := fs.String("key", "", "file with the secret key")
	keyEnv := fs.String("key-env", "RELEASE_SIGNING_KEY", "environment variable with the secret key")
	out := fs.String("out", "", "signature file (default <sums>.sig)")
	if err := parse(); err != nil {
		return err
	}
	var keyText []byte
	if *keyPath != "" {
		b, err := os.ReadFile(*keyPath)
		if err != nil {
			return err
		}
		keyText = b
	} else if v := getenv(*keyEnv); v != "" {
		keyText = []byte(v)
	} else {
		return fmt.Errorf("sign: no secret key: give -key <path> or set %s", *keyEnv)
	}
	key, err := update.ParseSecretKey(keyText)
	if err != nil {
		return err
	}
	msg, err := os.ReadFile(*sums)
	if err != nil {
		return err
	}
	if *out == "" {
		*out = *sums + ".sig"
	}
	if err := os.WriteFile(*out, key.Sign(msg, "file:"+filepath.Base(*sums)), 0o600); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "signed %s: %s\n", *sums, *out)
	return nil
}

func verify(args []string, stdout io.Writer, defaultPub []byte) error {
	fs, parse := flags("verify", args)
	sums := fs.String("sums", "SHA256SUMS", "the list of checksums")
	sig := fs.String("sig", "", "its signature (default <sums>.sig)")
	archive := fs.String("archive", "", "an archive to check against the list")
	pubPath := fs.String("pub", "", "public key file (default: the key in the repository)")
	if err := parse(); err != nil {
		return err
	}
	if *sig == "" {
		*sig = *sums + ".sig"
	}
	pub := defaultPub
	if *pubPath != "" {
		b, err := os.ReadFile(*pubPath)
		if err != nil {
			return err
		}
		pub = b
	}
	sumsData, err := os.ReadFile(*sums)
	if err != nil {
		return err
	}
	sigData, err := os.ReadFile(*sig)
	if err != nil {
		return err
	}
	if *archive == "" {
		key, err := update.ParsePublicKey(pub)
		if err != nil {
			return fmt.Errorf("release key: %w", err)
		}
		if err := key.VerifySignature(sumsData, sigData); err != nil {
			return fmt.Errorf("%w: %w", update.ErrSignature, err)
		}
		_, _ = fmt.Fprintf(stdout, "%s: signature ok\n", *sums)
		return nil
	}
	data, err := os.ReadFile(*archive)
	if err != nil {
		return err
	}
	if err := update.Verify(pub, data, sumsData, sigData, filepath.Base(*archive)); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "%s: signature and checksum ok\n", *archive)
	return nil
}
