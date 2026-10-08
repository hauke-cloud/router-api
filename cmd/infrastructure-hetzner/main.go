// Command infrastructure-hetzner runs the Hetzner Cloud infrastructure
// provider of router-api: HetznerRouterNetwork and HetznerMachine.
package main

import (
	"github.com/hauke-cloud/router-api/internal/infrastructure/hetzner/cloud"
	"github.com/hauke-cloud/router-api/internal/manager"
	"github.com/hauke-cloud/router-api/internal/managers"
	"github.com/hauke-cloud/router-api/internal/version"
)

func main() {
	manager.Main(managers.Hetzner(cloud.NewFactory(cloud.WithApplication("router-api", version.Version()))))
}
