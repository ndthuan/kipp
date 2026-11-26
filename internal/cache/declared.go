package cache

// Declared is a cache a stage declares, as it is run: keys expanded, scope
// chosen. kipp restores each before the stage's command and saves each
// after it, with the same code the kipp cache command runs.
type Declared struct {
	Key         string
	RestoreKeys []string
	Paths       []string
	Scope       string
}

func (d Declared) restoreArgs() []string {
	args := []string{"--key", d.Key, "--scope", d.Scope}
	for _, k := range d.RestoreKeys {
		args = append(args, "--restore-key", k)
	}
	for _, p := range d.Paths {
		args = append(args, "--path", p)
	}
	return args
}

func (d Declared) saveArgs() []string {
	args := []string{"--key", d.Key, "--scope", d.Scope}
	for _, p := range d.Paths {
		args = append(args, "--path", p)
	}
	return args
}

// RestoreDeclared restores each cache, in order, and says for each whether
// it was an exact hit. A miss, or a server that does not answer, is a line
// in e.Stderr and no failure, as it is for the command.
func RestoreDeclared(specs []Declared, e Env) []bool {
	hits := make([]bool, len(specs))
	for i, d := range specs {
		hits[i] = restore(d.restoreArgs(), e) == ExitRestored
	}
	return hits
}

// SaveDeclared saves each cache that was not an exact hit, in order. One
// that cannot be saved is a line in e.Stderr and no failure: the stage's
// command has already ended well.
func SaveDeclared(specs []Declared, hits []bool, e Env) {
	for i, d := range specs {
		if hits[i] {
			say(e, "key %s was an exact hit; not saved again", d.Key)
			continue
		}
		save(d.saveArgs(), e)
	}
}
