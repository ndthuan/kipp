package dispatch

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/runnerproto"
	"github.com/ndthuan/kipp/internal/store"
)

type envVar struct{ Name, Value string }

func parseEnvFile(data []byte) (vars []envVar, problems []string) {
	lines := strings.Split(string(data), "\n")
	for i := 0; i < len(lines); i++ {
		line, at := lines[i], i+1
		if line == "" {
			continue
		}
		name, value, delim, heredoc, ok := splitEnvLine(line)
		if !ok {
			problems = append(problems, fmt.Sprintf("line %d is neither NAME=value nor NAME<<DELIM", at))
			continue
		}
		if heredoc {
			end := slices.Index(lines[i+1:], delim)
			if end < 0 {
				problems = append(problems, fmt.Sprintf("line %d opens a value that no line ends", at))
				break
			}
			value, i = strings.Join(lines[i+1:i+1+end], "\n"), i+end+1
		}
		if strings.ContainsRune(name+value, 0) {
			problems = append(problems, fmt.Sprintf("line %d holds a NUL, which a variable cannot", at))
			continue
		}
		vars = append(vars, envVar{name, value})
	}
	return lastWins(vars), problems
}

func lastWins(vars []envVar) []envVar {
	at := map[string]int{}
	var out []envVar
	for _, v := range vars {
		if i, ok := at[v.Name]; ok {
			out[i].Value = v.Value
			continue
		}
		at[v.Name] = len(out)
		out = append(out, v)
	}
	return out
}

func splitEnvLine(line string) (name, value, delim string, heredoc, ok bool) {
	eq, hd := strings.Index(line, "="), strings.Index(line, "<<")
	switch {
	case hd >= 0 && (eq < 0 || hd < eq):
		name, delim = line[:hd], line[hd+2:]
		return name, "", delim, true, name != "" && delim != ""
	case eq > 0:
		return line[:eq], line[eq+1:], "", false, true
	}
	return "", "", "", false, false
}

type stageOutputs struct {
	mu    sync.Mutex
	seq   int
	ended map[string]endedEnv
}

type endedEnv struct {
	seq  int
	vars []envVar
}

func newStageOutputs() *stageOutputs { return &stageOutputs{ended: map[string]endedEnv{}} }

func (o *stageOutputs) add(stage string, vars []envVar) {
	if len(vars) == 0 {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seq++
	o.ended[stage] = endedEnv{o.seq, vars}
}

func (o *stageOutputs) upstream(stages []*config.Stage, s *config.Stage) []string {
	needed := map[string]bool{}
	byName := make(map[string]*config.Stage, len(stages))
	for _, st := range stages {
		byName[st.Name] = st
	}
	for todo := slices.Clone(s.Needs); len(todo) > 0; {
		n := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if !needed[n] {
			needed[n] = true
			if st := byName[n]; st != nil {
				todo = append(todo, st.Needs...)
			}
		}
	}
	return o.merged(func(stage string) bool { return needed[stage] })
}

func (o *stageOutputs) all() []string { return o.merged(func(string) bool { return true }) }

func (o *stageOutputs) merged(keep func(stage string) bool) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var from []string
	for stage := range o.ended {
		if keep(stage) {
			from = append(from, stage)
		}
	}
	slices.SortFunc(from, func(a, b string) int { return o.ended[a].seq - o.ended[b].seq })
	merged := map[string]string{}
	for _, stage := range from {
		for _, v := range o.ended[stage].vars {
			merged[v.Name] = v.Value
		}
	}
	var out []string
	for _, name := range slices.Sorted(maps.Keys(merged)) {
		out = append(out, name+"="+merged[name])
	}
	return out
}

func resetEnvFile(workspace, stage string) (string, error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	rel := filepath.FromSlash(runnerproto.EnvFile(stage))
	if err := root.MkdirAll(filepath.Dir(rel), 0o750); err != nil {
		return "", err
	}
	if err := root.WriteFile(rel, nil, 0o600); err != nil {
		return "", err
	}
	return "KIPP_ENV=" + filepath.Join(workspace, rel), nil
}

func (d *Dispatcher) collectEnv(b *store.Build, e *buildEnv, s *config.Stage, status store.StageStatus) {
	if status != store.StageSuccess && status != store.StageUnstable {
		return
	}
	var said []string
	say := func(format string, args ...any) {
		said = append(said, "kipp: KIPP_ENV "+s.Name+": "+fmt.Sprintf(format, args...))
	}
	data, err := d.readEnvFile(e, s)
	switch {
	case err != nil:
		say("the file could not be read: %v", err)
	case len(data) > runnerproto.MaxEnvFile:
		say("the file is over %d KiB and is refused whole", runnerproto.MaxEnvFile>>10)
	default:
		vars, problems := parseEnvFile(data)
		for _, p := range problems {
			say("%s", p)
		}
		kept := vars[:0]
		for _, v := range vars {
			if err := e.refuseEnvName(b, v.Name); err != nil {
				say("skipped: %v", err)
				continue
			}
			kept = append(kept, v)
		}
		e.outs.add(s.Name, kept)
	}
	for _, line := range said {
		d.note(b, s.Name, e, line)
	}
}

func (d *Dispatcher) readEnvFile(e *buildEnv, s *config.Stage) ([]byte, error) {
	dir, rel := e.workspace, filepath.FromSlash(runnerproto.EnvFile(s.Name))
	if d.remote(s) {
		dir, rel = junitDir(e, s), filepath.FromSlash(runnerproto.EnvTarName)
	}
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, runnerproto.MaxEnvFile+1))
}

func (e *buildEnv) refuseEnvName(b *store.Build, name string) error {
	if err := config.CheckEnvName(name); err != nil {
		return err
	}
	switch {
	case slices.Contains(e.secretNames, name):
		return fmt.Errorf("env %s: a secret this job lists has the name", name)
	case has(b.Params, name):
		return fmt.Errorf("env %s: a param of this job has the name", name)
	}
	return nil
}
