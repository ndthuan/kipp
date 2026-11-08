package mcp

import (
	"bytes"
	"context"
	"os"
	"strings"

	"github.com/ndthuan/kipp/internal/dispatch"
)

// Limits of one read of a log.
const (
	defaultTail  = 200
	maxTail      = 2000
	defaultBytes = 64 << 10
	maxBytes     = 256 << 10
)

type readLogIn struct {
	jobRef
	Stage      string `json:"stage" jsonschema:"the stage's name, as get_build lists it; clone, prepare and hooks are the server's own logs"`
	TailLines  int    `json:"tail_lines,omitempty" jsonschema:"how many of the last lines to return, default 200, at most 2000; ignored when offset is given"`
	Offset     *int64 `json:"offset,omitempty" jsonschema:"read a byte range from this offset instead of the tail"`
	LimitBytes int    `json:"limit_bytes,omitempty" jsonschema:"with offset, how many bytes, default 65536, at most 262144"`
}

type readLogOut struct {
	Text string `json:"text"`
	// Offset is where Text begins in the log and Next where it ends; pass
	// Next as offset to read on.
	Offset int64 `json:"offset"`
	Next   int64 `json:"next_offset"`
	Size   int64 `json:"total_size_bytes"`
	// Truncated says the log has bytes before or after Text, in the size
	// read.
	Truncated bool `json:"truncated"`
	// More says the log may grow: its build has not ended.
	More bool `json:"build_still_writing"`
}

func (s *Server) readLog(ctx context.Context, c *Caller, in readLogIn) (readLogOut, error) {
	var out readLogOut
	if err := s.checkLogQuery(in); err != nil {
		return out, err
	}
	b, err := s.build(ctx, c, in.jobRef)
	if err != nil {
		return out, err
	}
	if in.Stage == "" {
		return out, badInput("stage is required: one of the stages get_build lists")
	}
	svc := s.cfg.Service
	if err := svc.HasStage(ctx, c.User, b.ID, in.Stage); err != nil {
		return out, err
	}
	path, _, err := svc.LogPath(ctx, c.User, b.ID, in.Stage)
	if err != nil {
		return out, err
	}
	v, err := svc.Build(ctx, c.User, b.ID)
	if err != nil {
		return out, err
	}
	out = readLog(path, in)
	out.More = !v.Complete
	return out, nil
}

func (s *Server) checkLogQuery(in readLogIn) error {
	switch {
	case in.TailLines < 0 || in.TailLines > maxTail:
		return badInput("tail_lines is 1 to %d", maxTail)
	case in.LimitBytes < 0 || in.LimitBytes > maxBytes:
		return badInput("limit_bytes is 1 to %d", maxBytes)
	case in.Offset != nil && *in.Offset < 0:
		return badInput("offset is 0 or more")
	}
	return nil
}

// readLog reads the part of the log at path that in asks for. A log that is
// not written yet is empty.
func readLog(path string, in readLogIn) readLogOut {
	f, err := dispatch.OpenLog(path, os.O_RDONLY)
	if err != nil {
		return readLogOut{}
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return readLogOut{}
	}
	size := info.Size()
	var start, end int64
	if in.Offset != nil {
		start = min(*in.Offset, size)
		end = min(start+int64(or(in.LimitBytes, defaultBytes)), size)
	} else {
		start, end = max(size-maxBytes, 0), size
	}
	buf := make([]byte, end-start)
	n, _ := f.ReadAt(buf, start)
	text := buf[:n]
	if in.Offset == nil {
		skip := tailStart(text, or(in.TailLines, defaultTail), start > 0)
		start += int64(skip)
		text = text[skip:]
	}
	return readLogOut{Text: strings.ToValidUTF8(string(text), "�"), Offset: start, Next: start + int64(len(text)), Size: size,
		Truncated: start > 0 || start+int64(len(text)) < size}
}

func or(n, fallback int) int {
	if n == 0 {
		return fallback
	}
	return n
}

// tailStart is where the last lines lines of text begin. When text was cut
// out of a longer log, its first line is probably a part of one and is
// dropped unless it is all there is.
func tailStart(text []byte, lines int, cut bool) int {
	at := len(text)
	if at > 0 && text[at-1] == '\n' {
		at--
	}
	for found := 0; lines > 0; lines-- {
		i := bytes.LastIndexByte(text[:at], '\n')
		if i < 0 {
			if cut && found > 0 {
				return at + 1
			}
			return 0
		}
		at, found = i, found+1
	}
	return at + 1
}
