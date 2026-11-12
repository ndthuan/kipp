package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
)

// Snapshot is every job of a set, read at one moment.
type Snapshot struct {
	Jobs  map[string]*Job
	Names []string // sorted
	Vars  *Vars    // from VarsFile; nil when there is none
	Hash  string   // of every file's content; changes when any file does
	// Texts are the YAML each job was read from, by job name, comments
	// and all; Hashes are each job's DecodedHash. Neither holds a child.
	Texts  map[string][]byte
	Hashes map[string]string
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

// Files are the texts of a job set by file name, as a jobs directory
// holds them: <job>.yml for each job, and VarsFile.
type Files map[string][]byte

// FileName is the file a job of that name is read from.
func FileName(job string) string { return job + ".yml" }

// Load reads every *.yml in dir. One bad file fails the whole load, with
// every file's error listed, so the caller keeps its previous snapshot.
func Load(dir string) (*Snapshot, error) {
	files, err := ReadDir(dir)
	errs, refused := errors.AsType[*Errors](err)
	if err != nil && !refused {
		return nil, err
	}
	return check(files, errs)
}

// Check runs on files every check Load runs on a directory.
func Check(files Files) (*Snapshot, error) { return check(files, nil) }

// ReadDir reads the job files of dir. A file it cannot read, and a *.yaml
// file (read as no job at all it would go unnoticed), is an *Errors beside
// the files it read.
func ReadDir(dir string) (Files, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	files := Files{}
	var errs Errors
	for _, e := range entries {
		if stem, ok := strings.CutSuffix(e.Name(), ".yaml"); ok && !e.IsDir() {
			errs.Items = append(errs.Items, fmt.Errorf("%s: a job file ends in .yml; rename it to %s.yml", e.Name(), stem))
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		data, err := root.ReadFile(e.Name())
		if err != nil {
			errs.Items = append(errs.Items, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		files[e.Name()] = data
	}
	if len(errs.Items) > 0 {
		return files, &errs
	}
	return files, nil
}

func check(files Files, prior *Errors) (*Snapshot, error) {
	snap := &Snapshot{Jobs: map[string]*Job{}, Texts: map[string][]byte{}, Hashes: map[string]string{}}
	var errs Errors
	if prior != nil {
		errs.Items = append(errs.Items, prior.Items...)
	}
	names := slices.Sorted(maps.Keys(files))
	contents := make([][]byte, 0, 2*len(names))
	for _, name := range names {
		read := readJob
		if name == VarsFile {
			read = readVars
		}
		if err := read(snap, name, files[name]); err != nil {
			errs.Items = append(errs.Items, fmt.Errorf("%s: %w", name, err))
		}
		contents = append(contents, []byte(name), files[name])
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

// readJob parses one file into snap.
func readJob(snap *Snapshot, name string, data []byte) error {
	j, err := ParseJob(name, data)
	if err != nil {
		return err
	}
	snap.Jobs[j.Name] = j
	snap.Names = append(snap.Names, j.Name)
	snap.Texts[j.Name] = data
	snap.Hashes[j.Name] = DecodedHash(j)
	return nil
}

// readVars parses VarsFile into snap.
func readVars(snap *Snapshot, _ string, data []byte) error {
	var err error
	snap.Vars, err = ParseVars(data)
	return err
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

// checkAfter refuses a triggers.after naming what can never fire it.
func checkAfter(snap *Snapshot) []error {
	var errs []error
	for _, n := range snap.Names {
		j := snap.Jobs[n]
		for _, up := range j.Triggers.After {
			if why := snap.afterRefusal(up); why != "" {
				errs = append(errs, fmt.Errorf("%s.yml: triggers.after names %q, %s", j.Name, up, why))
			}
		}
	}
	return errs
}

// afterRefusal says why name can never fire a job that runs after it, or ""
// when it can: exactly the names triggerDownstream fires, a job of the
// files or a repo's node. A repo not indexed yet is accepted, as
// permissions accept it: the files alone cannot know the owner's repos.
func (s *Snapshot) afterRefusal(name string) string {
	if j, ok := s.Jobs[name]; ok {
		if j.IsOwner() {
			return fmt.Sprintf("which is an owner job and never builds itself: after follows one of its repos' nodes, %q", name+".<repo>")
		}
		return ""
	}
	for _, j := range s.Jobs {
		rest, ok := strings.CutPrefix(name, j.Name+".")
		if !ok || !j.IsOwner() && !j.IsParent() {
			continue
		}
		key, _, nested := strings.Cut(rest, ".")
		switch {
		case j.IsParent():
			return fmt.Sprintf("which is a branch or a pull request of a multibranch job: after follows the multibranch job %q; it fires on its default branch's green build", j.Name)
		case nested:
			return fmt.Sprintf("which is a branch or a pull request of a repo: after follows the repo node %q; it fires on its default branch's green build", j.Name+"."+key)
		case repoKeyRe.MatchString(key):
			return ""
		}
	}
	return "which is not a job"
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
		// A name that is no job of the files (a repo's node) lists no
		// after of its own, so no way round passes through it.
		job := snap.Jobs[n]
		if job == nil {
			return nil
		}
		path = append(path, n)
		for _, up := range job.Triggers.After {
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
