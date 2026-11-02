//go:build !darwin && !linux

package runner

import "errors"

// bornOf has nothing to read here.
func bornOf(int, string) (string, error) { return "", errors.ErrUnsupported }

// stillLeads has nothing to go by here.
func stillLeads(Group) (bool, error) { return false, errors.ErrUnsupported }
