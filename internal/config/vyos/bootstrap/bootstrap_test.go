package bootstrap

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/hauke-cloud/router-api/internal/config/vyos/command"
)

func newCredentials(t *testing.T) *Credentials {
	t.Helper()
	credentials, err := NewCredentials("edge-abc.routers.router-api.internal")
	if err != nil {
		t.Fatalf("NewCredentials: %v", err)
	}
	return credentials
}

func TestNewCredentials(t *testing.T) {
	a, b := newCredentials(t), newCredentials(t)

	if len(a.APIKey) < 32 || a.APIKey == b.APIKey {
		t.Errorf("API keys %q and %q: too short or not random", a.APIKey, b.APIKey)
	}
	// The pair has to work as a TLS server certificate.
	if _, err := tls.X509KeyPair(a.CertPEM, a.KeyPEM); err != nil {
		t.Fatalf("the certificate and key do not form a pair: %v", err)
	}
	block, _ := pem.Decode(a.CertPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cert.DNSNames, []string{"edge-abc.routers.router-api.internal"}) {
		t.Errorf("DNS names = %v", cert.DNSNames)
	}
	// A router lives as long as nobody replaces it. A certificate that
	// expires would cut the operator off from a router that works.
	if time.Until(cert.NotAfter) < 9*365*24*time.Hour {
		t.Errorf("certificate expires %v", cert.NotAfter)
	}
	// It verifies against itself with the name it was issued for: that is
	// exactly the check the operator's client makes.
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: a.ServerName}); err != nil {
		t.Errorf("certificate does not verify: %v", err)
	}
}

func params(t *testing.T) *Params {
	t.Helper()
	return &Params{
		Credentials:      newCredentials(t),
		Image:            "ghcr.io/hauke-cloud/vyos@sha256:abc",
		HostName:         "edge-abc",
		Port:             8443,
		AllowedSources:   []string{"192.0.2.0/24"},
		ConfirmAction:    "reload",
		PublicInterface:  "eth0",
		PrivateInterface: "eth1",
	}
}

func TestManagementCommands(t *testing.T) {
	p := params(t)
	lines := command.Lines(ManagementCommands(p))

	for _, want := range []string{
		"set service https api rest",
		"set service https api keys id router-api key " + p.Credentials.APIKey,
		"set service https certificates certificate router-api",
		"set service https port 8443",
		"set service https allow-client address 192.0.2.0/24",
		"set system config-management commit-confirm action reload",
		// Found on a real router: without revisions to revert to, VyOS
		// rejects the reload action, and with it every commit.
		"set system config-management commit-revisions 100",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("missing %q in\n%s", want, strings.Join(lines, "\n"))
		}
	}

	// VyOS wants the bare base64 of the DER, without the PEM armour.
	var certLine string
	for _, line := range lines {
		if strings.HasPrefix(line, "set pki certificate router-api certificate ") {
			certLine = strings.TrimPrefix(line, "set pki certificate router-api certificate ")
		}
	}
	der, err := base64.StdEncoding.DecodeString(strings.Trim(certLine, "'"))
	if err != nil {
		t.Fatalf("certificate value %q is not base64: %v", certLine, err)
	}
	if _, err := x509.ParseCertificate(der); err != nil {
		t.Errorf("certificate value is not a certificate: %v", err)
	}
}

func TestDesired(t *testing.T) {
	p := params(t)
	parse := func(text string) []command.Path {
		paths, err := command.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		return paths
	}

	t.Run("defaults fill what the user left out", func(t *testing.T) {
		lines := command.Lines(Desired(parse("set system time-zone UTC\n"), p))
		for _, want := range []string{
			"set system time-zone UTC",
			"set service https api rest",
			// Without these the first apply would delete the address the
			// operator reaches the router on, and the only login.
			"set interfaces ethernet eth0 address dhcp",
			"set interfaces ethernet eth1 address dhcp",
			"set system login user vyos authentication encrypted-password '!'",
		} {
			if !slices.Contains(lines, want) {
				t.Errorf("missing %q in\n%s", want, strings.Join(lines, "\n"))
			}
		}
	})

	t.Run("the user's own choice replaces a default", func(t *testing.T) {
		lines := command.Lines(Desired(parse(`
set interfaces ethernet eth0 address 203.0.113.7/32
set system login user hauke authentication public-keys k key AAAA
`), p))
		for _, unwanted := range []string{
			"set interfaces ethernet eth0 address dhcp",
			"set system login user vyos authentication encrypted-password '!'",
		} {
			if slices.Contains(lines, unwanted) {
				t.Errorf("default %q was added although the user configured that part", unwanted)
			}
		}
		if !slices.Contains(lines, "set interfaces ethernet eth1 address dhcp") {
			t.Error("the default for eth1 is missing although the user said nothing about eth1")
		}
	})

	t.Run("the user cannot override what the operator needs", func(t *testing.T) {
		lines := command.Lines(Desired(parse("set service https api keys id router-api key stolen\n"), p))
		if slices.Contains(lines, "set service https api keys id router-api key stolen") {
			t.Error("a user command replaced the operator's API key")
		}
	})
}

func TestSeedScript(t *testing.T) {
	p := params(t)
	script := SeedScript(p)

	if !strings.HasPrefix(script, "#!/bin/vbash\n") {
		t.Errorf("script starts with %q", strings.SplitN(script, "\n", 2)[0])
	}
	order := []string{
		"[ -e /config/router-api/seeded ] && exit 0",
		"source /opt/vyatta/etc/functions/script-template",
		"configure",
		"set system host-name edge-abc",
		"set service https api rest",
		"commit",
		"save",
		"touch /config/router-api/seeded",
	}
	at := 0
	for _, want := range order {
		i := strings.Index(script[at:], want)
		if i < 0 {
			t.Fatalf("%q is missing or out of order in\n%s", want, script)
		}
		at += i
	}
	// The default account's password is public knowledge.
	if !strings.Contains(script, "set system login user vyos authentication encrypted-password '!'") ||
		!strings.Contains(script, "delete system login user vyos authentication plaintext-password") {
		t.Error("the seed does not lock the default account")
	}
}

type cloudConfig struct {
	Packages   []string `json:"packages"`
	WriteFiles []struct {
		Path        string `json:"path"`
		Permissions string `json:"permissions"`
		Content     string `json:"content"`
		Encoding    string `json:"encoding"`
	} `json:"write_files"`
	RunCmd [][]string `json:"runcmd"`
}

func TestUserData(t *testing.T) {
	p := params(t)
	p.Files = []File{{Path: "/config/hetzner/failover.json", Permissions: "0640", Content: `{"tokenFile":"/config/hetzner/token"}`}}

	userData, err := UserData(p)
	if err != nil {
		t.Fatalf("UserData: %v", err)
	}
	if !strings.HasPrefix(userData, "#cloud-config\n") {
		t.Fatalf("user data starts with %q; cloud-init ignores it without the header", strings.SplitN(userData, "\n", 2)[0])
	}
	var config cloudConfig
	if err := yaml.Unmarshal([]byte(userData), &config); err != nil {
		t.Fatalf("user data is not YAML: %v", err)
	}
	if !slices.Contains(config.Packages, "podman") {
		t.Errorf("packages = %v", config.Packages)
	}

	files := map[string]string{}
	perms := map[string]string{}
	for _, file := range config.WriteFiles {
		content := file.Content
		if file.Encoding == "b64" {
			raw, err := base64.StdEncoding.DecodeString(content)
			if err != nil {
				t.Fatalf("%s: %v", file.Path, err)
			}
			content = string(raw)
		}
		files[file.Path] = content
		perms[file.Path] = file.Permissions
	}

	hook := HostConfigDir + "/scripts/vyos-postconfig-bootup.script"
	if files[hook] != SeedScript(p) || perms[hook] != "0755" {
		t.Errorf("the seed hook is missing, different or not executable (permissions %q)", perms[hook])
	}
	// A path below /config in the router is a path below the volume on the
	// host.
	if got := files[HostConfigDir+"/hetzner/failover.json"]; got != `{"tokenFile":"/config/hetzner/token"}` {
		t.Errorf("file content = %q", got)
	}
	if perms[HostConfigDir+"/hetzner/failover.json"] != "0640" {
		t.Errorf("file permissions = %q", perms[HostConfigDir+"/hetzner/failover.json"])
	}
	quadlet := files["/etc/containers/systemd/vyos.container"]
	for _, want := range []string{
		"Image=ghcr.io/hauke-cloud/vyos@sha256:abc",
		"Network=host",
		"--privileged",
		"Volume=/lib/modules:/lib/modules:ro",
		"Volume=" + HostConfigDir + ":/opt/vyatta/etc/config",
		"Restart=always",
	} {
		if !strings.Contains(quadlet, want) {
			t.Errorf("quadlet lacks %q:\n%s", want, quadlet)
		}
	}
	if len(config.RunCmd) == 0 {
		t.Error("nothing starts the router")
	}
}

func TestUserDataRejectsFilesOutsideConfig(t *testing.T) {
	p := params(t)
	p.Files = []File{{Path: "/config/../etc/shadow", Content: "x"}}
	if _, err := UserData(p); err == nil {
		t.Error("a file path that escapes /config was accepted")
	}
}

func TestUserDataSizeLimit(t *testing.T) {
	p := params(t)
	p.Files = []File{{Path: "/config/big", Content: strings.Repeat("x", MaxUserDataBytes)}}
	_, err := UserData(p)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("err = %v, want the size limit named", err)
	}
}
