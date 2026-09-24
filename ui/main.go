// Breez Recovery is the desktop app that restores a Breez mobile wallet
// backup from Google Drive, iCloud or a backup file and moves the funds to
// a bitcoin address. It is a Wails window over ../core.
package main

import (
	"embed"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

//go:embed all:frontend/dist
var assets embed.FS

// windowicon.png is a 256px copy of appicon.png: X11 window managers
// drop the icon property when it is handed the 1024px original.
//
//go:embed build/windowicon.png
var icon []byte

func main() {
	// The node helper, started by the window (helper.go). Checked before
	// anything of Wails: its runtime functions end a program that has no
	// window.
	if os.Getenv(helperEnv) != "" {
		runHelper()
		return
	}
	app := newApp()
	err := wails.Run(&options.App{
		Title:            "Breez Recovery",
		Width:            980,
		Height:           700,
		MinWidth:         760,
		MinHeight:        560,
		BackgroundColour: &options.RGBA{R: 244, G: 247, B: 251, A: 1},
		AssetServer:      &assetserver.Options{Assets: assets},
		OnStartup:        app.startup,
		OnBeforeClose:    app.beforeClose,
		Bind:             []interface{}{app},
		// No browser context menu: reload or back would drop the page state
		// while the node keeps running.
		EnableDefaultContextMenu: false,
		Mac: &mac.Options{
			About: &mac.AboutInfo{
				Title:   "Breez Recovery " + version,
				Message: "Restores a Breez app backup and moves the funds to a bitcoin address.",
				Icon:    icon,
			},
		},
		Linux: &linux.Options{
			Icon:        icon,
			ProgramName: "Breez Recovery",
		},
	})
	if err != nil {
		panic(err)
	}
}
