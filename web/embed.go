// Package web embeds the Vite build so the daemon ships as one binary.
package web

import "embed"

//go:embed dist
var Dist embed.FS
