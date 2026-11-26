package webhook

import (
	"testing"

	"github.com/ndthuan/kipp/internal/apispec/apispectest"
)

// The package's tests hold its handlers to api/openapi.yaml: every answer is
// checked, and every operation of the package needs a good one.
func TestMain(m *testing.M) { apispectest.Main(m, "webhook") }
