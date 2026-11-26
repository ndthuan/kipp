package config

// RunnerDir is the directory, inside its build's on a runner, that the
// stage called name of stages runs in, its KIPP_WORKSPACE there. It is
// the stage's own, named after it, unless the stage needs exactly one
// stage and is the only one that needs it: then it carries on in that
// stage's directory. The stages that share a directory are a line of
// needs, so no two of them run at once, and a line keeps for the next
// stage the files the one before it left there.
func RunnerDir(stages []*Stage, name string) string {
	byName := make(map[string]*Stage, len(stages))
	dependents := make(map[string]int, len(stages))
	for _, s := range stages {
		byName[s.Name] = s
		for _, n := range s.Needs {
			dependents[n]++
		}
	}
	for s := byName[name]; s != nil; {
		if len(s.Needs) != 1 || dependents[s.Needs[0]] != 1 || byName[s.Needs[0]] == nil {
			return s.Name
		}
		s = byName[s.Needs[0]]
	}
	return name
}
