package update

import _ "embed"

//go:embed release.pub
var releasePub []byte

// ReleaseKey is the public key file every kipp carries; only the production
// call site uses it, a test passes its own key to Verify.
func ReleaseKey() []byte { return releaseKey() }
