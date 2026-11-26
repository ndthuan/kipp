package api

import (
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/apispec/apispectest"
)

// The package's tests hold its handlers to api/openapi.yaml: every answer is
// checked, and every operation of the package needs a good one. They run in
// a zone that is not UTC, so that a time the API answers in the server's
// zone instead of UTC fails them; it is set here, before any test starts a
// build whose goroutines read it.
func TestMain(m *testing.M) {
	time.Local = time.FixedZone("east", 7*3600)
	apispectest.Main(m, "api")
}
