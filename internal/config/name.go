package config

import (
	"errors"

	"go.yaml.in/yaml/v3"
)

// NameOf is the name a job file's text gives its job, for a job made from
// a text alone: the rest of the file is not checked here.
func NameOf(data []byte) (string, error) {
	var f struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(data, &f); err != nil {
		return "", err
	}
	if f.Name == "" {
		return "", errors.New("the job has no name: put name: <job> at the top of the file")
	}
	return f.Name, nil
}
