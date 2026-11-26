package runnerd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ndthuan/kipp/internal/runnerproto"
)

type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return fmt.Sprintf("the server answered %d: %s", e.code, e.msg) }

func retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if se, ok := errors.AsType[*statusError](err); ok {
		return se.code >= 500
	}
	_, ok := errors.AsType[*url.Error](err)
	return ok
}

func (a *daemon) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	resp, err := a.do(ctx, method, path, "application/json", body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return json.NewDecoder(resp.Body).Decode(out)
}

func (a *daemon) do(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.c.URL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.c.Token)
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := a.c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	var r struct {
		runnerproto.Refusal
		Detail string `json:"detail"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&r)
	if r.Error == "" {
		r.Error = r.Detail
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return nil, ErrTokenRefused
	case http.StatusConflict:
		return nil, &VersionError{Runner: a.c.Version, Server: r.ServerVersion}
	}
	return nil, &statusError{code: resp.StatusCode, msg: r.Error}
}

func stagePath(id, suffix string) string {
	return runnerproto.StagePath + url.PathEscape(id) + suffix
}

func donePath(id string, code, tries int, msg string) string {
	q := url.Values{"exit": {strconv.Itoa(code)}, "tries": {strconv.Itoa(tries)}}
	if msg != "" {
		q.Set("error", msg)
	}
	return stagePath(id, runnerproto.DoneSuffix) + "?" + q.Encode()
}
