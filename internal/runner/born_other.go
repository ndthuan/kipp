//go:build !darwin && !linux

package runner

import "errors"

func bornOf(int, string) (string, error) { return "", errors.ErrUnsupported }

func stillLeads(Group) (bool, error) { return false, errors.ErrUnsupported }
