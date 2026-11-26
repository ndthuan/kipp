// Package runnerproto is what a runner and the server say to each other
// under /api/v1/runner/. The runner dials the server and the server never dials an
// runner, so a runner behind a NAT or in a container takes part as one on
// the server's own network does. Every call carries the runner's token as
// "Authorization: Bearer <token>".
package runnerproto

import (
	"path"
	"time"
)

// Root is where the server serves the runners' calls. A runner is always the
// server's own version.
const Root = "/api/v1/runner/"

// The paths, under the server's API, and what travels on them.
const (
	ConnectPath   = Root + "connect"
	ClaimPath     = Root + "claim"
	StagePath     = Root + "stages/"
	LogSuffix     = "/log"
	ControlSuffix = "/control"
	DoneSuffix    = "/done"
	// ArtifactsSuffix is PUT ?size=<bytes>, the body a tar of the files the
	// stage keeps, once its command has ended and before done; size is
	// what they come to.
	ArtifactsSuffix = "/artifacts"
	// InputsSuffix + <stage> is GET, a tar of the files that stage of the
	// build kept, which the runner puts under
	// $KIPP_WORKSPACE/.kipp/artifacts/<stage>/ before the command.
	InputsSuffix = "/inputs/"
	JunitType    = "application/x-tar"
	// TokenPrefix is what a runner's token starts with, so that one found
	// in a log is known for what it is.
	TokenPrefix = "kippa_"
)

// MaxEnvFile is the most a stage's KIPP_ENV file may hold; the runner sends
// one byte more than that of a longer file, which is how the server knows.
const MaxEnvFile = 64 << 10

// EnvTarName is the name, in the tar a done call carries, of the stage's
// KIPP_ENV file.
const EnvTarName = ".kipp/env"

// EnvFile is where, inside a build's workspace, the stage's KIPP_ENV file
// is: one per stage, as stages that run at once each write their own.
func EnvFile(stage string) string { return path.Join(".kipp", "env-"+stage) }

// ConnectRequest is what a runner says of itself. It is sent when the runner
// starts and again every Heartbeat as long as it runs; three missed and the
// server counts it offline.
type ConnectRequest struct {
	OS, Arch, Version string
	Fresh             bool
	Running           []string
}

// ConnectResponse is who the server says the runner is. Labels and
// executors are the server's to set: a runner reports only what it is.
type ConnectResponse struct {
	Name          string
	Labels        []string
	Executors     int
	ServerVersion string
	Heartbeat     time.Duration
	Kill          []string
}

// Assignment is one stage to run.
type Assignment struct {
	ID           string
	Job          string
	Number       int
	Stage        string
	Command      string
	Env          []string
	Workspace    string
	Workdir      string
	Junit        string
	Metrics      string       `json:",omitempty"`
	Baseline     []byte       `json:",omitempty"`
	Artifacts    *Artifacts   `json:",omitempty"`
	Inputs       []Input      `json:",omitempty"`
	Cache        []Cache      `json:",omitempty"`
	UnstableExit []int        `json:",omitempty"`
	Retries      int          `json:",omitempty"`
	SecretFiles  []SecretFile `json:",omitempty"`
}

// SecretFile is one file secret: the name of the secret, and its bytes.
type SecretFile struct {
	Name string
	Data []byte
}

// Cache is one declared cache of a stage, its keys already expanded.
type Cache struct {
	Key         string
	RestoreKeys []string `json:",omitempty"`
	Paths       []string
	Scope       string
}

// Input is one stage whose artifacts a stage is given, and the most they
// come to.
type Input struct {
	Stage string
	Size  int64
}

// Artifacts is a stage's artifacts key: its globs, read inside the stage's
// directory, and the most the matches may come to.
type Artifacts struct {
	Paths []string
	Max   int64
}

// Ended is a build whose stages on the runner are over: its workspace there
// goes.
type Ended struct {
	Workspace string
}

// Claim answers a claim: an assignment, or none when the long-poll ran out,
// and the builds that ended since the last answer.
type Claim struct {
	Assignment *Assignment
	Ended      []Ended
}

// The actions a control poll answers.
const (
	ActionNone   = "none"
	ActionCancel = "cancel"
)

// Control answers a control poll.
type Control struct {
	Action string
}

// Refusal is the body of an answer that is not 2xx: why, and for a 409
// the version the server is, so that the runner can say which to run.
type Refusal struct {
	Error         string
	ServerVersion string `json:",omitempty"`
}
