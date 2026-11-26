package dispatch

import (
	"maps"
	"slices"
	"strings"
)

var hookOnlyNames = []string{
	"PATH", "HOME", "SHELL", "ENV", "IFS", "CDPATH", "PS4", "SHELLOPTS", "GLOBIGNORE", "TMPDIR",
	"JAVA_TOOL_OPTIONS", "_JAVA_OPTIONS",
}

var hookOnlyPrefixes = []string{
	"LD_", "DYLD_", "BASH", "GIT_", "SSH_", "SSL_", "CURL_", "XDG_", "PYTHON", "PERL", "RUBY", "NODE_",
}

func steersHook(name string) bool {
	if slices.Contains(hookOnlyNames, name) || strings.HasSuffix(strings.ToUpper(name), "_PROXY") {
		return true
	}
	return slices.ContainsFunc(hookOnlyPrefixes, func(p string) bool { return strings.HasPrefix(name, p) })
}

func (e *buildEnv) hookEnv(more ...string) []string {
	server := slices.Concat(e.serverEnv, more)
	set := map[string]bool{}
	for _, kv := range server {
		name, _, _ := strings.Cut(kv, "=")
		set[name] = true
	}
	var given []string
	for _, name := range slices.Sorted(maps.Keys(e.fileEnv)) {
		given = append(given, name+"="+e.fileEnv[name])
	}
	given = append(given, e.outs.all()...)
	given = slices.DeleteFunc(given, func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return set[name] || steersHook(name)
	})
	return slices.Concat(given, server)
}
