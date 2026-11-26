package mcp

import (
	"bytes"
	"context"
	"os"
	"strings"

	"github.com/ndthuan/kipp/internal/dispatch"
)

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
	LimitBytes int    `json:"limit_bytes,omitempty" jsonschema:"the most bytes to return, default 65536, at most 262144; a tail of long lines gives fewer lines than asked"`
}

type readLogOut struct {
	Text      string `json:"text"`
	Offset    int64  `json:"offset"`
	Next      int64  `json:"next_offset"`
	Size      int64  `json:"total_size_bytes"`
	Truncated bool   `json:"truncated"`
	More      bool   `json:"build_still_writing"`
}

func (s *Server) readLog(ctx context.Context, c *Caller, in readLogIn) (readLogOut, error) {
	var out readLogOut
	if err := s.checkLogQuery(in); err != nil {
		return out, err
	}
	path, complete, err := s.stageLog(ctx, c, in.jobRef, in.Stage)
	if err != nil {
		return out, err
	}
	out = readLog(path, in)
	out.More = !complete
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
	size, limit := info.Size(), or(in.LimitBytes, defaultBytes)
	from := max(size-int64(limit), 0)
	if in.Offset != nil {
		from = *in.Offset
	}
	text, start := readAt(f, size, from, limit)
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
