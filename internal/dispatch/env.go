package dispatch

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ndthuan/kipp/internal/config"
)

// DefaultPassEnv are the server's variables a stage is given when the
// server names none (serve --pass-env): what a shell and the programs it
// runs need to find themselves, and nothing the server was started with
// for its own sake, such as a cloud key or a token.
var DefaultPassEnv = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_*", "TZ", "TMPDIR"}

// PassEnv keeps of environ, as os.Environ gives it, the variables pass
// names: a name, a prefix with a star after it, or a star alone for all.
func PassEnv(environ, pass []string) []string {
	if slices.Contains(pass, "*") {
		return slices.Clone(environ)
	}
	out := []string{}
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if ok && slices.ContainsFunc(pass, func(p string) bool { return passes(p, name) }) {
			out = append(out, kv)
		}
	}
	return out
}

func passes(pattern, name string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(name, prefix)
	}
	return pattern == name
}

// ParsePassEnv reads a comma-separated --pass-env: names, prefixes with a
// star after them, or a star alone. Nothing at all passes no variable.
func ParsePassEnv(list string) ([]string, error) {
	out := []string{}
	for item := range strings.SplitSeq(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		prefix, star := strings.CutSuffix(item, "*")
		all := star && prefix == ""
		if !all && !config.ValidEnvName(prefix) {
			return nil, fmt.Errorf("--pass-env %q: a variable's name, a prefix of one with * after it, or * for all", item)
		}
		out = append(out, item)
	}
	return out, nil
}
