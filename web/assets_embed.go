//go:build embedweb

package web

import (
	"embed"
	"io/fs"
)

// Explicit file allowlist only — never use `all:dist` so source maps, dotfiles,
// or stray files in web/dist/ can never be silently embedded into binaries.
//
//go:embed dist/index.html dist/favicon.svg dist/__p2p_gw__/bridge.html dist/__p2p_gw__/manifest.json dist/__p2p_gw__/*.js dist/__p2p_gw__/*.css
var embeddedDistFS embed.FS

// DistFS exposes the built web frontend rooted at dist/.
var DistFS fs.FS = embeddedDistFS
