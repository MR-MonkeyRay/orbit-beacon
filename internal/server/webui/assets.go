package webui

import (
	"embed"
	"io/fs"
)

//go:embed dist
var distribution embed.FS

func Assets() fs.FS {
	assets, err := fs.Sub(distribution, "dist")
	if err != nil {
		panic(err)
	}
	return assets
}
