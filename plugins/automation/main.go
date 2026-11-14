// Command silo-plugin-automation lets Silo admins read Radarr, Sonarr and
// SABnzbd through Silo, with the apps' addresses and API keys kept in the
// plugin's server-side settings.
package main

import (
	"context"
	_ "embed"
	"os"

	"github.com/hashicorp/go-hclog"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

//go:embed manifest.json
var manifestJSON []byte

var version = ""

func main() {
	logger := hclog.New(&hclog.LoggerOptions{Name: "automation", Level: hclog.Info, Output: os.Stderr, JSONFormat: true})
	app := newApp(logger)
	sdkruntime.ServeManifestWithOptions(manifestJSON, version, sdkruntime.CapabilityServers{
		HttpRoutes: &routeServer{app: app},
	}, sdkruntime.WithConfigure(func(ctx context.Context, entries []*pluginv1.ConfigEntry) error {
		app.configure(entries)
		return nil
	}))
}
