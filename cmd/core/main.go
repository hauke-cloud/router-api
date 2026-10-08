// Command core runs the provider-independent controllers of router-api:
// RouterDeployment, RouterSet, Router, RouterMachine and RouterHealthCheck.
package main

import (
	"github.com/hauke-cloud/router-api/internal/manager"
	"github.com/hauke-cloud/router-api/internal/managers"
)

func main() {
	manager.Main(managers.Core())
}
