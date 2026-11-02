// Package openapi holds the OpenAPI description of the kipp API: the single
// file that `make openapi` bundles from the split sources in this directory.
package openapi

import _ "embed"

// YAML is api/openapi.gen.yaml, the whole description in one file.
//
//go:embed openapi.gen.yaml
var YAML []byte
