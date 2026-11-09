package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// SelfJob names the job a build belongs to in permissions: and
// max_permissions:, which for a child is the child.
const SelfJob = "self"

// Grant is what a build's token may do on one job: kipp's own actions.
type Grant struct {
	View, Build, Cancel bool
}

// Actions are the grant's actions, in the order the docs give them.
func (g Grant) Actions() []string {
	var out []string
	for i, on := range []bool{g.View, g.Build, g.Cancel} {
		if on {
			out = append(out, []string{"view", "build", "cancel"}[i])
		}
	}
	return out
}

func (g Grant) union(o Grant) Grant {
	return Grant{g.View || o.View, g.Build || o.Build, g.Cancel || o.Cancel}
}

// Permissions is a job's permissions: or max_permissions: key: the jobs a
// build's token may reach, by name, with what it may do on each. The zero
// value is the default, view on the build's own job.
type Permissions struct {
	// None is permissions: none, a build with no token at all.
	None bool
	// Jobs are the grants by job name, SelfJob among them.
	Jobs map[string]Grant
}

// UnmarshalYAML reads none, or a map of a job name to one action or a list
// of them; read is view and write is view, build and cancel.
func (p *Permissions) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		if n.Value != "none" {
			return fmt.Errorf("line %d: permissions is none or a map of job names to actions, not %q", n.Line, n.Value)
		}
		p.None = true
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: permissions is none or a map of job names to actions", n.Line)
	}
	p.Jobs = map[string]Grant{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		if key.Value != SelfJob && !ValidName(key.Value) {
			return fmt.Errorf("line %d: permissions: %q is no job name", key.Line, key.Value)
		}
		if _, dup := p.Jobs[key.Value]; dup {
			return fmt.Errorf("line %d: permissions names %s twice", key.Line, key.Value)
		}
		g, err := readGrant(val)
		if err != nil {
			return fmt.Errorf("permissions: %s: %w", key.Value, err)
		}
		p.Jobs[key.Value] = g
	}
	return nil
}

func readGrant(n *yaml.Node) (Grant, error) {
	var names []string
	switch n.Kind {
	case yaml.ScalarNode:
		names = []string{n.Value}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return Grant{}, fmt.Errorf("line %d: an action is view, build or cancel", c.Line)
			}
			names = append(names, c.Value)
		}
	default:
		return Grant{}, fmt.Errorf("line %d: one action or a list of them", n.Line)
	}
	if len(names) == 0 {
		return Grant{}, fmt.Errorf("line %d: no action: use view, build, cancel, read or write", n.Line)
	}
	var g Grant
	for _, name := range names {
		switch name {
		case "view", "read":
			g.View = true
		case "build":
			g.Build = true
		case "cancel":
			g.Cancel = true
		case "write":
			g = g.union(Grant{true, true, true})
		default:
			return Grant{}, fmt.Errorf("line %d: %q is not an action: view, build, cancel, read or write", n.Line, name)
		}
	}
	return g, nil
}

// Resolve is the grants a build of the job named self holds: SelfJob
// replaced by self, and view on self always there. A nil p, and one that
// writes none, resolves to that alone: the default.
func (p *Permissions) Resolve(self string) map[string]Grant {
	out := map[string]Grant{self: {View: true}}
	if p == nil {
		return out
	}
	for name, g := range p.Jobs {
		if name == SelfJob {
			name = self
		}
		out[name] = out[name].union(g)
	}
	return out
}

// NoToken reports whether the build gets no token.
func (p *Permissions) NoToken() bool { return p != nil && p.None }

// CheckSelfBuild refuses build on the job's own build: a build cannot start
// its own job. own is the job's name; a request for SelfJob or for own is
// the same.
func (p *Permissions) CheckSelfBuild(own string) error {
	if p == nil {
		return nil
	}
	for _, name := range []string{SelfJob, own} {
		if g := p.Jobs[name]; g.Build {
			return fmt.Errorf("permissions: build or write on %s: a build cannot start its own job", name)
		}
	}
	return nil
}

// Names are the job names p mentions, SelfJob left out, sorted.
func (p *Permissions) Names() []string {
	if p == nil {
		return nil
	}
	var out []string
	for name := range p.Jobs {
		if name != SelfJob {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Excess lists what a build of the job named self asks for beyond ceiling,
// as "job: action, action", sorted. Grants are compared by job name and
// action as written, self matching the build's own name: one the job would
// inherit through its parent's or folder's grant is not inside the ceiling.
func (p *Permissions) Excess(ceiling *Permissions, self string) []string {
	if p == nil || p.None {
		return nil
	}
	allowed := ceiling.Resolve(self)
	var out []string
	for name, g := range p.Resolve(self) {
		var over []string
		have := allowed[name]
		for _, a := range g.Actions() {
			if !slices.Contains(have.Actions(), a) {
				over = append(over, a)
			}
		}
		if len(over) > 0 {
			out = append(out, name+": "+strings.Join(over, ", "))
		}
	}
	slices.Sort(out)
	return out
}

// CeilingOwner is the job whose max_permissions: bounds a build of j: the
// owner job of a repo, the multibranch job of a child, the job itself for a
// source job.
func (j *Job) CeilingOwner() string {
	switch {
	case j.Folder != "":
		return j.Folder
	case j.Parent != "":
		return j.Parent
	}
	return j.Name
}

// RepoPermissions is the permissions: of a stages file in a repo, read
// against j's max_permissions: it is the grant the build asks for; the
// error says each permission over the ceiling and whose key would allow it.
func (j *Job) RepoPermissions(file *Permissions) error {
	over := file.Excess(j.MaxPermissions, j.Name)
	if len(over) == 0 {
		return nil
	}
	return fmt.Errorf("permissions over the ceiling: %s; job %s would have to allow them in max_permissions",
		strings.Join(over, "; "), j.CeilingOwner())
}

// Grants are what a build of j holds before its stages file is read: the
// job's own permissions: of a file on the server. A child holds the default,
// a parent's permissions: not being its children's.
func (j *Job) Grants() map[string]Grant { return j.Permissions.Resolve(j.Name) }

// checkPermissions refuses what a job file may not say of permissions:
// build on its own job, and max_permissions: where no repo stages file is
// read.
func (j *Job) checkPermissions() error {
	if j.MaxPermissions != nil {
		if j.Multibranch == nil && j.Owner == nil && j.Source == nil {
			return errors.New("max_permissions: it bounds the permissions of a stages file in a repo, and this job reads none (it has no source, multibranch or owner)")
		}
		if j.MaxPermissions.None {
			return errors.New("max_permissions: none is no ceiling; leave the key out for the default")
		}
		if err := j.MaxPermissions.CheckSelfBuild(j.Name); err != nil {
			return errors.New("max_" + err.Error())
		}
	}
	return j.Permissions.CheckSelfBuild(j.Name)
}

// checkPermissionNames refuses a permissions: or max_permissions: naming a
// job nobody has.
func checkPermissionNames(snap *Snapshot) []error {
	var errs []error
	for _, n := range snap.Names {
		j := snap.Jobs[n]
		for _, key := range []struct {
			name string
			p    *Permissions
		}{{"permissions", j.Permissions}, {"max_permissions", j.MaxPermissions}} {
			for _, name := range key.p.Names() {
				if !snap.knowsJob(name) {
					errs = append(errs, fmt.Errorf("%s.yml: %s names %q, which is not a job", j.Name, key.name, name))
				}
			}
		}
	}
	return errs
}

// knowsJob reports whether name is a job of the files, or may be a child or
// a repo of a multibranch or owner job, which the files do not hold: a name
// that starts with theirs and a dot.
func (s *Snapshot) knowsJob(name string) bool {
	if _, ok := s.Jobs[name]; ok {
		return true
	}
	for _, j := range s.Jobs {
		if (j.IsParent() || j.IsOwner()) && strings.HasPrefix(name, j.Name+".") {
			return true
		}
	}
	return false
}

// checkTokenSecrets is a warning for each job that lists a secret called
// KIPP_TOKEN: kipp sets that variable last, so the secret is never seen.
func checkTokenSecrets(snap *Snapshot) []string {
	var out []string
	for _, n := range snap.Names {
		if slices.Contains(snap.Jobs[n].Secrets, "KIPP_TOKEN") {
			out = append(out, n+".yml: the secret KIPP_TOKEN is hidden by the build's own KIPP_TOKEN, which kipp sets after every secret; rename the secret, or set permissions: none to get no build token")
		}
	}
	return out
}
