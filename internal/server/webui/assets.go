// Package webui 通过 go:embed 打包并暴露前端构建产物目录。
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distribution embed.FS

// Assets 返回以构建输出目录 dist 为根的只读文件系统。
func Assets() fs.FS {
	assets, err := fs.Sub(distribution, "dist")
	if err != nil {
		panic(err)
	}
	return assets
}
