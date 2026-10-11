// Package buildinfo carries what the release build stamps into the binary.
package buildinfo

// Version is the release tag the binary was built from (e.g. "v1.0.18"). The
// release workflow sets it with
//
//	-ldflags "-X github.com/digitrade-e/digi-erp-connector/internal/buildinfo.Version=$VERSION"
//
// and anything built without that — go run, go test, a local build — says
// "dev". GET /api/ping returns it, so a caller can tell which build answers.
var Version = "dev"
