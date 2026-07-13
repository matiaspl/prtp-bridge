package webui

import (
	"embed"
	"io/fs"
)

//go:embed app.js index.html rx-worklet.js
var embedded embed.FS

func FS() fs.FS {
	return embedded
}
