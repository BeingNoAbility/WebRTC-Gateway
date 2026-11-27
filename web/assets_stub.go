//go:build !embedweb

package web

import (
	"io/fs"
	"testing/fstest"
)

// DistFS is a minimal placeholder when built without `-tags embedweb`.
// Pure Go tests (`go test ./...`) compile with zero Node.js dependency,
// while production / Docker / release / CDP E2E builds pass `-tags embedweb`.
var DistFS fs.FS = fstest.MapFS{
	"dist/index.html": &fstest.MapFile{
		Data: []byte("<!doctype html><html><body>Web assets not embedded. Build with `make build` (`-tags embedweb`) or pass `-static`.</body></html>\n"),
	},
}
