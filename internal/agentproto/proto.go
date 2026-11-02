// Package agentproto is what an agent and the server say to each other
// under /api/v1/agent/. The agent dials the server and the server never dials an
// agent, so an agent behind a NAT or in a container takes part as one on
// the server's own network does. Every call carries the agent's token as
// "Authorization: Bearer <token>".
package agentproto

import (
	"path"
	"time"
)

// Root is where the server serves the agents' calls. An agent is always the
// server's own version.
const Root = "/api/v1/agent/"

// The paths, under the server's API, and what travels on them.
const (
	ConnectPath   = Root + "connect" // POST ConnectRequest -> ConnectResponse
	ClaimPath     = Root + "claim"   // GET, long-poll -> Claim
	StagePath     = Root + "stages/" // + <assignment id> + one of the suffixes
	LogSuffix     = "/log"           // POST, the stage's output as a chunked body
	ControlSuffix = "/control"       // GET, long-poll -> Control
	DoneSuffix    = "/done"          // POST ?exit=<code>&tries=<n>&error=<text>, body a tar of the junit and metrics files
	// ArtifactsSuffix is PUT ?size=<bytes>, the body a tar of the files the
	// stage keeps, once its command has ended and before done; size is
	// what they come to.
	ArtifactsSuffix = "/artifacts"
	// InputsSuffix + <stage> is GET, a tar of the files that stage of the
	// build kept, which the agent puts under
	// $KIPP_WORKSPACE/.kipp/artifacts/<stage>/ before the command.
	InputsSuffix = "/inputs/"
	JunitType    = "application/x-tar"
	// TokenPrefix is what an agent's token starts with, so that one found
	// in a log is known for what it is.
	TokenPrefix = "kippa_"
)

// MaxEnvFile is the most a stage's KIPP_ENV file may hold; the agent sends
// one byte more than that of a longer file, which is how the server knows.
const MaxEnvFile = 64 << 10

// EnvTarName is the name, in the tar a done call carries, of the stage's
// KIPP_ENV file.
const EnvTarName = ".kipp/env"

// EnvFile is where, inside a build's workspace, the stage's KIPP_ENV file
// is: one per stage, as stages that run at once each write their own.
func EnvFile(stage string) string { return path.Join(".kipp", "env-"+stage) }

// ConnectRequest is what an agent says of itself. It is sent when the agent
// starts and again every Heartbeat as long as it runs; three missed and the
// server counts it offline.
type ConnectRequest struct {
	OS, Arch, Version string
	// Fresh is true on the first connect of the agent's process: what the
	// server thought it was running died with the one before.
	Fresh bool
	// Running are the assignments the agent is running now.
	Running []string
}

// ConnectResponse is who the server says the agent is. Labels and
// executors are the server's to set: an agent reports only what it is.
type ConnectResponse struct {
	Name          string
	Labels        []string
	Executors     int // 0: as many at once as it is given
	ServerVersion string
	Heartbeat     time.Duration
	// Kill are assignments the agent said it runs that the server does not
	// know as running there: their builds ended, or the server started
	// again since. The agent kills them and removes their workspaces.
	Kill []string
}

// Assignment is one stage to run.
type Assignment struct {
	ID     string
	Job    string
	Number int
	Stage  string
	// Command is run with the agent's shell, -c.
	Command string
	// Env is what the server gives the stage: the variables --agent-env
	// names, then the job's env, params and secrets, then KIPP_*. The agent
	// puts its own environment before it and KIPP_WORKSPACE and KIPP_AGENT
	// after it.
	Env []string
	// Workspace is the build's directory under the agent's workdir, the same
	// for every stage of one build there; empty is the workdir itself.
	Workspace string
	// Workdir is a path inside the workspace the stage runs in.
	Workdir string
	// Junit is the stage's junit glob, read inside the stage's directory
	// once the command ends; empty is none.
	Junit string
	// Metrics is the stage's metrics.file glob, read as Junit is; the
	// files of both go back in one tar.
	Metrics string `json:",omitempty"`
	// Baseline is what the stage's store remembers, for the agent to write
	// to a file of the workspace and name in KIPP_METRICS; nil for a stage
	// that declares no metrics.
	Baseline []byte `json:",omitempty"`
	// Artifacts are the files the stage keeps, sent once its command has
	// ended, however it ended; nil keeps none.
	Artifacts *Artifacts `json:",omitempty"`
	// Inputs are the stages whose artifacts the stage is given, each with
	// what they come to.
	Inputs []Input `json:",omitempty"`
	// Cache are the caches the agent restores before the command and saves
	// after one that ended well; UnstableExit are the codes that count as
	// ending well besides 0.
	Cache        []Cache `json:",omitempty"`
	UnstableExit []int   `json:",omitempty"`
	// Retries is how many more times the agent runs the command when it
	// ends with a code that is neither 0 nor in UnstableExit.
	Retries int `json:",omitempty"`
	// SecretFiles are the file secrets of the stage's job. They are bytes,
	// which Env's strings cannot carry; the agent writes each to a file of
	// its own outside the workspace, names it in a variable called after
	// the secret, and removes it when the stage ends.
	SecretFiles []SecretFile `json:",omitempty"`
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

// Ended is a build whose stages on the agent are over: its workspace there
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
// the version the server is, so that the agent can say which to run.
type Refusal struct {
	Error         string
	ServerVersion string `json:",omitempty"`
}
