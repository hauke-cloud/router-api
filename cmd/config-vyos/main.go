// Command config-vyos runs the VyOS config provider of router-api:
// VyOSConfig and VyOSConfigTemplate.
package main

import (
	"github.com/hauke-cloud/router-api/internal/manager"
	"github.com/hauke-cloud/router-api/internal/managers"
)

func main() {
	manager.Main(managers.VyOS())
}
