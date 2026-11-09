package config

import (
	"fmt"
	"os"
	"slices"
	"strings"
)

// Snapshot is every job in a directory, read at one moment.
type Snapshot struct {
	Jobs  map[string]*Job
	Names []string // sorted
	Vars  *Vars    // from VarsFile; nil when there is none
	Hash  string   // of every file's content; changes when any file does
	// Warnings are what Load accepted and the author should still read.
	Warnings []string
}

// Errors is every problem Load found, one per file.
type Errors struct {
	Items []error
}

func (e *Errors) Error() string {
	lines := make([]string, len(e.Items))
	for i, it := range e.Items {
		lines[i] = it.Error()
	}
	return strings.Join(lines, "\n")
}

// Load reads every *.yml in dir. One bad file fails the whole load, with
// every file's error listed, so the caller keeps its previous snapshot. A
// *.yaml file is one of them: read as no job at all it would go unnoticed.
func Load(dir string) (*Snapshot, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	snap := &Snapshot{Jobs: map[string]*Job{}}
	var errs Errors
	var contents [][]byte
	for _, e := range entries {
		if stem, ok := strings.CutSuffix(e.Name(), ".yaml"); ok && !e.IsDir() {
			errs.Items = append(errs.Items, fmt.Errorf("%s: a job file ends in .yml; rename it to %s.yml", e.Name(), stem))
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		read := readJob
		if e.Name() == VarsFile {
			read = readVars
		}
		data, err := read(snap, root, e.Name())
		if err != nil {
			errs.Items = append(errs.Items, fmt.Errorf("%s: %w", e.Name(), err))
		}
		contents = append(contents, []byte(e.Name()), data)
	}
	slices.Sort(snap.Names)
	errs.Items = append(errs.Items, checkVars(snap)...)
	errs.Items = append(errs.Items, checkAfter(snap)...)
	errs.Items = append(errs.Items, checkChildNames(snap)...)
	errs.Items = append(errs.Items, checkApps(snap)...)
	errs.Items = append(errs.Items, checkPermissionNames(snap)...)
	if len(errs.Items) == 0 {
		errs.Items = append(errs.Items, checkAfterCycles(snap)...)
	}
	if len(errs.Items) > 0 {
		slices.SortFunc(errs.Items, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
		return nil, &errs
	}
	snap.Hash = hashOf(contents...)
	snap.Warnings = checkTokenSecrets(snap)
	return snap, nil
}

// readJob parses one file into snap and returns its raw content.
func readJob(snap *Snapshot, root *os.Root, name string) ([]byte, error) {
	data, err := root.ReadFile(name)
	if err != nil {
		return nil, err
	}
	j, err := ParseJob(name, data)
	if err != nil {
		return data, err
	}
	snap.Jobs[j.Name] = j
	snap.Names = append(snap.Names, j.Name)
	return data, nil
}

// readVars parses VarsFile into snap and returns its raw content.
func readVars(snap *Snapshot, root *os.Root, name string) ([]byte, error) {
	data, err := root.ReadFile(name)
	if err != nil {
		return nil, err
	}
	snap.Vars, err = ParseVars(data)
	return data, err
}

// checkVars refuses what only the jobs and VarsFile together show: two
// patterns setting one name for a job, a pattern's variable named like a
// secret the job lists (a global or group one is left to the build, as the
// file is shared), and a cache key naming a variable no file gives the job.
func checkVars(snap *Snapshot) []error {
	var errs []error
	for _, n := range snap.Names {
		j := snap.Jobs[n]
		vars, err := snap.Vars.For(j)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var patterned, all []string
		for _, v := range vars {
			all = append(all, v.Name)
			if v.FromJobs() {
				patterned = append(patterned, v.Name)
			}
		}
		if err := j.CheckVarNames(VarsFile+" for job "+j.Name, patterned); err != nil {
			errs = append(errs, err)
		}
		if err := j.CheckCaches(j.Stages, all); err != nil {
			errs = append(errs, fmt.Errorf("%s.yml: %w", j.Name, err))
		}
	}
	return errs
}

// checkAfter refuses a triggers.after naming a job outside the set.
func checkAfter(snap *Snapshot) []error {
	var errs []error
	for _, j := range snap.Jobs {
		for _, up := range j.Triggers.After {
			if _, ok := snap.Jobs[up]; !ok {
				errs = append(errs, fmt.Errorf("%s.yml: triggers.after names %q, which is not a job", j.Name, up))
			}
		}
	}
	return errs
}

// checkApps refuses two Apps under one name, which would share a webhook
// route and a secret, and two owner jobs of one account under one App,
// which could not tell whose a delivery is.
func checkApps(snap *Snapshot) []error {
	var errs []error
	byName := map[string]*Job{}
	byLogin := map[string]*Job{}
	for _, n := range snap.Names {
		j := snap.Jobs[n]
		if !j.IsOwner() || j.Owner.App == nil {
			continue
		}
		a := j.Owner.App
		if prev := byName[a.Name]; prev != nil && (prev.Owner.App.ID != a.ID || prev.Owner.App.KeyFile != a.KeyFile) {
			errs = append(errs, fmt.Errorf("%s.yml: owner.app is called %q, as %s's is, and it is another App: give one of them owner.app.name", j.Name, a.Name, prev.Name))
		} else if prev == nil {
			byName[a.Name] = j
		}
		key := a.Name + "/" + strings.ToLower(j.Owner.Login)
		if prev := byLogin[key]; prev != nil {
			errs = append(errs, fmt.Errorf("%s.yml: %s is indexed by %s too with the same App: one owner job each", j.Name, j.Owner.Login, prev.Name))
		} else {
			byLogin[key] = j
		}
	}
	return errs
}

// checkAfterCycles refuses a loop among triggers.after lists, which would
// build forever, and names the jobs on it.
func checkAfterCycles(snap *Snapshot) []error {
	done := map[string]bool{}
	var path []string
	var visit func(n string) []string
	visit = func(n string) []string {
		if i := slices.Index(path, n); i >= 0 {
			return append(slices.Clone(path[i:]), n)
		}
		if done[n] {
			return nil
		}
		path = append(path, n)
		for _, up := range snap.Jobs[n].Triggers.After {
			if c := visit(up); c != nil {
				return c
			}
		}
		path = path[:len(path)-1]
		done[n] = true
		return nil
	}
	for _, n := range snap.Names {
		if c := visit(n); c != nil {
			return []error{fmt.Errorf("triggers.after: a cycle among jobs: %s", strings.Join(c, " -> "))}
		}
	}
	return nil
}
