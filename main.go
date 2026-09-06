package main

import (
	"embed"
	"io/fs"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend
var assets embed.FS

func main() {
	app := NewApp()

	err := wails.Run(&options.App{
		Title:     "CLink · Minecraft 一键联机",
		Width:     920,
		Height:    680,
		MinWidth:  720,
		MinHeight: 560,
		AssetServer: &assetserver.Options{
			Assets: frontendAssets(),
		},
		OnStartup: app.startup,
		Bind:      []interface{}{app},
	})
	if err != nil {
		log.Fatal(err)
	}
}

// frontendAssets 取 frontend 子树作为静态资源根。
func frontendAssets() fs.FS {
	sub, err := fs.Sub(assets, "frontend")
	if err != nil {
		log.Fatal(err)
	}
	return sub
}
