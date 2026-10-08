// Command userdata prints the cloud-init user data router-api would create a
// router with, and writes the credentials that go with it. It is a debugging
// aid: for looking at what a server is given, and for bootstrapping a server
// by hand.
//
//	go run ./hack/userdata -image ghcr.io/hauke-cloud/vyos:latest -name edge -out /tmp/edge
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hauke-cloud/router-api/internal/config/vyos/bootstrap"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "userdata:", err)
		os.Exit(1)
	}
}

func run() error {
	image := flag.String("image", "", "VyOS container image")
	name := flag.String("name", "edge", "host name of the router")
	port := flag.Int("port", 443, "port of the REST API")
	out := flag.String("out", "", "directory to write user-data, api-key, tls.crt and server-name to")
	flag.Parse()
	if *image == "" || *out == "" {
		flag.Usage()
		return errors.New("-image and -out are required")
	}

	credentials, err := bootstrap.NewCredentials(*name + ".manual.router-api.internal")
	if err != nil {
		return err
	}
	userData, err := bootstrap.UserData(&bootstrap.Params{
		Credentials: credentials, Image: *image, HostName: *name, Port: int32(*port), //nolint:gosec // a port number
		ConfirmAction: "reload", PublicInterface: "eth0", PrivateInterface: "eth1",
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o700); err != nil {
		return err
	}
	for file, content := range map[string][]byte{
		"user-data":   []byte(userData),
		"api-key":     []byte(credentials.APIKey),
		"tls.crt":     credentials.CertPEM,
		"server-name": []byte(credentials.ServerName),
	} {
		if err := os.WriteFile(filepath.Join(*out, file), content, 0o600); err != nil {
			return err
		}
	}
	fmt.Printf("wrote %d bytes of user data to %s\n", len(userData), *out)
	return nil
}
