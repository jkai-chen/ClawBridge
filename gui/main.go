// Command gui is the ClawBridge desktop app: a Wails v3 shell around a
// TypeScript UI that talks to `clawbridge gui-backend --stdio` (GUI-DESIGN
// §4.1). It is its own module and is never imported by the main module.
package main

import (
	"embed"
	"log"
	"os"
	"runtime"

	"clawbridge/gui/backend"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed all:frontend/dist
var assets embed.FS

// Version is set at build time with -ldflags "-X main.Version=…".
var Version = "dev"

func main() {
	store, err := backend.DefaultSettingsStore()
	if err != nil {
		log.Fatal(err)
	}
	shell := &Shell{settings: store, ready: make(chan bool, 1)}
	selfcheck := os.Getenv("CLAWBRIDGE_GUI_SELFCHECK") == "1"

	app := application.New(application.Options{
		Name:     "ClawBridge",
		Services: []application.Service{application.NewService(shell)},
		Assets:   application.AssetOptions{Handler: application.BundledAssetFileServer(assets)},
		Mac:      application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: true},
		OnShutdown: func() {
			shell.bridge.Stop()
		},
	})
	shell.app = app
	shell.bridge = backend.New(
		backend.NewResolver(store).Resolve,
		func(name string, data any) { app.Event.Emit(name, data) },
		Version, runtime.GOOS,
	)

	menu := application.NewMenu()
	if runtime.GOOS == "darwin" {
		menu.AddRole(application.AppMenu)
	}
	menu.AddRole(application.EditMenu)
	if runtime.GOOS == "darwin" {
		menu.AddRole(application.WindowMenu)
	}
	help := menu.AddSubmenu("Help")
	help.Add("Logs").OnClick(func(*application.Context) { app.Event.Emit("ui/open-logs") })
	app.Menu.Set(menu)

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             "main",
		Title:            "ClawBridge",
		Width:            1280,
		Height:           820,
		MinWidth:         1024,
		MinHeight:        640,
		BackgroundColour: application.NewRGB(244, 246, 248),
		URL:              "/",
	})

	// The page asks for the state when it loads, so a start that finished
	// before the window existed is not missed.
	if selfcheck {
		// No client is started: the window must render on its own.
		go runSelfCheck(shell)
	} else {
		shell.bridge.Start()
	}
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
