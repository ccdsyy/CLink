package main

import (
	"embed"
	"io/fs"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend
var assets embed.FS

func main() {
	app := NewApp()

	err := wails.Run(&options.App{
		Title:            "CLink · Minecraft 一键联机",
		Width:            960,
		Height:           700,
		MinWidth:         720,
		MinHeight:        560,
		Frameless:        true, // 无边框：自绘苹果风标题栏 + 悬浮球模式必需
		HideWindowOnClose: false,
		AssetServer: &assetserver.Options{
			Assets: frontendAssets(),
		},
		OnStartup:  app.startup,
		OnShutdown: app.shutdown,
		Bind:       []interface{}{app},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			DisableWindowIcon:    false,
		},
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
