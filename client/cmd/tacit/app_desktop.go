//go:build desktop

package main

import (
	"context"
	_ "embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// desktopBuild: this binary opens the native window (Wails v2, the system
// webview: WebKitGTK 4.1 on Linux, WKWebView on macOS).
const desktopBuild = true

//go:embed reviewui/icon.png
var appIconPNG []byte

// appIconICNS is the macOS bundle icon `tacit install app` writes
// (scripts/make-icns.sh builds it from reviewui/icon.svg).
//
//go:embed macos/tacit.icns
var appIconICNS []byte

// runDesktop opens the window. The page is the same one --browser serves; it
// draws without the GPU (Wails issue 2977: a blank window on some NVIDIA and
// virtual displays; the page is text and needs none). It
// reaches Go through the bindings of App, so no port and no token exist.
func runDesktop(app *App) error {
	var wctx context.Context
	return wails.Run(&options.App{
		Title:            "tacit",
		Width:            1600,
		Height:           1000,
		MinWidth:         900,
		MinHeight:        600,
		BackgroundColour: options.NewRGBA(0xf5, 0xf6, 0xf2, 0xff),
		AssetServer:      &assetserver.Options{Handler: app.uiHandler("")},
		OnStartup:        func(ctx context.Context) { wctx = ctx },
		Bind:             []interface{}{app},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "tacit-app-4f1c2b7e",
			OnSecondInstanceLaunch: func(options.SecondInstanceData) {
				if wctx != nil {
					wruntime.WindowUnminimise(wctx)
					wruntime.WindowShow(wctx)
				}
			},
		},
		Linux: &linux.Options{Icon: appIconPNG, ProgramName: "tacit", WebviewGpuPolicy: linux.WebviewGpuPolicyNever},
		Mac:   &mac.Options{About: &mac.AboutInfo{Title: "tacit", Message: "What needs you, and every fact your agents read."}},
	})
}
