//go:build embedspa

package main

import (
	"embed"
	"io/fs"
)

//go:embed dist
var dist embed.FS

// spaFS returns the built sign-in application, rooted at "/" rather than
// "dist/".
//
// Only compiled with `-tags embedspa`, which every target producing a shippable
// binary passes and no CI gate does. See spa_none.go for why.
func spaFS() (fs.FS, error) { return fs.Sub(dist, "dist") }
