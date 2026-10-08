// Package bootstrap produces what a VyOS router is created with: its
// credentials, the part of its configuration the operator depends on, and the
// user data that turns a stock Ubuntu server into the router.
package bootstrap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/hauke-cloud/router-api/internal/config/vyos/command"
)

const (
	// HostConfigDir is the directory on the host that is VyOS's /config.
	HostConfigDir = "/var/lib/router-api/config"
	// MaxUserDataBytes is the largest user data Hetzner Cloud accepts.
	MaxUserDataBytes = 32 * 1024
	// name is what the operator calls its certificate and API key on the
	// router.
	name = "router-api"
	// certificateLifetime is long on purpose. The certificate is pinned,
	// not trusted through a CA, and a router is replaced far more often
	// than this; an expiry would only ever cut the operator off from a
	// router that works.
	certificateLifetime = 20 * 365 * 24 * time.Hour
)

// imageReference is what an image name may consist of. It is the pattern the
// API enforces on VyOSConfig.spec.image; keep the two in step.
var imageReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)

//go:embed host-setup.sh
var hostSetup string

// Credentials are how the operator and a router know each other.
type Credentials struct {
	// APIKey authenticates the operator to the router.
	APIKey string
	// CertPEM and KeyPEM are the router's TLS certificate. The operator
	// trusts exactly this certificate.
	CertPEM, KeyPEM []byte
	// ServerName is the name in the certificate. A router's address is not
	// known when its certificate is made, so the name is one of our own
	// choosing and the client checks for it instead of the address.
	ServerName string
}

// NewCredentials generates an API key and a self-signed certificate.
func NewCredentials(serverName string) (*Credentials, error) {
	rawKey := make([]byte, 32)
	if _, err := rand.Read(rawKey); err != nil {
		return nil, fmt.Errorf("generate API key: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: serverName},
		DNSNames:              []string{serverName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(certificateLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode key: %w", err)
	}
	return &Credentials{
		APIKey:     hex.EncodeToString(rawKey),
		CertPEM:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:     pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		ServerName: serverName,
	}, nil
}

// File is a file placed below /config on the router.
type File struct {
	Path        string
	Permissions string
	Content     string
}

// Params describe one router.
type Params struct {
	Credentials *Credentials
	// Image is the VyOS container image.
	Image string
	// HostName of the router.
	HostName string
	// Port of the REST API.
	Port int32
	// AllowedSources restricts the REST API on the router; empty for none.
	AllowedSources []string
	// ConfirmAction is what the router does when a commit is not confirmed.
	ConfirmAction string
	// PublicInterface and PrivateInterface are the VyOS names of the host's
	// interfaces.
	PublicInterface, PrivateInterface string
	// Files placed below /config.
	Files []File
}

// body returns the base64 inside a PEM block, which is the form VyOS stores
// certificates and keys in.
func body(pemBytes []byte) string {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(block.Bytes)
}

// ManagementCommands is the configuration the operator needs to stay in
// contact with the router. It is part of every configuration applied.
func ManagementCommands(p *Params) []command.Path {
	action := p.ConfirmAction
	if action == "" {
		action = "reload"
	}
	port := p.Port
	if port == 0 {
		port = 443
	}
	paths := make([]command.Path, 0, 8+len(p.AllowedSources))
	paths = append(paths, []command.Path{
		{"pki", "certificate", name, "certificate", body(p.Credentials.CertPEM)},
		{"pki", "certificate", name, "private", "key", body(p.Credentials.KeyPEM)},
		{"service", "https", "certificates", "certificate", name},
		{"service", "https", "api", "keys", "id", name, "key", p.Credentials.APIKey},
		{"service", "https", "api", "rest"},
		{"service", "https", "port", strconv.Itoa(int(port))},
		// The default action is a reboot, which stops the container. And
		// reverting in place needs revisions to revert to: VyOS rejects
		// "commit-confirm action reload" without commit-revisions.
		{"system", "config-management", "commit-confirm", "action", action},
		{"system", "config-management", "commit-revisions", "100"},
	}...)
	for _, source := range p.AllowedSources {
		paths = append(paths, command.Path{"service", "https", "allow-client", "address", source})
	}
	return paths
}

// reserved are the parts of the configuration that belong to the operator. A
// user command below one of them is dropped.
var reserved = []command.Path{
	{"pki", "certificate", name},
	{"service", "https", "certificates"},
	{"service", "https", "api", "keys", "id", name},
	{"service", "https", "port"},
	{"service", "https", "allow-client"},
	{"system", "config-management"},
}

// Keep are the parts of a router's configuration that are never deleted
// although nobody lists them: VyOS writes each interface's MAC address into
// its own configuration.
var Keep = []command.Path{
	{"interfaces", "ethernet", command.Wildcard, "hw-id"},
}

// fallback is configuration that applies unless the user configured that
// part themselves.
type fallback struct {
	unless command.Path
	path   command.Path
}

func fallbacks(p *Params) []fallback {
	out := []fallback{{
		// The account VyOS ships with has a well-known password.
		unless: command.Path{"system", "login", "user"},
		path:   command.Path{"system", "login", "user", "vyos", "authentication", "encrypted-password", "!"},
	}}
	for _, iface := range []string{p.PublicInterface, p.PrivateInterface} {
		if iface == "" {
			continue
		}
		out = append(out, fallback{
			// Without an address on the interfaces the host came up with,
			// the router is unreachable.
			unless: command.Path{"interfaces", "ethernet", iface, "address"},
			path:   command.Path{"interfaces", "ethernet", iface, "address", "dhcp"},
		})
	}
	return out
}

// Desired is the complete configuration of a router: the user's commands,
// minus anything that reaches into what the operator owns, plus the
// operator's own, plus the fallbacks for what the user left open.
func Desired(user []command.Path, p *Params) []command.Path {
	desired := make([]command.Path, 0, len(user)+16)
outer:
	for _, path := range user {
		for _, prefix := range reserved {
			if path.HasPrefix(prefix) {
				continue outer
			}
		}
		desired = append(desired, path)
	}
	desired = append(desired, ManagementCommands(p)...)
	for _, fb := range fallbacks(p) {
		configured := false
		for _, path := range user {
			if path.HasPrefix(fb.unless) {
				configured = true
				break
			}
		}
		if !configured {
			desired = append(desired, fb.path)
		}
	}
	return desired
}

// SeedScript is the hook VyOS runs after loading its configuration. On the
// first boot it applies the configuration the operator needs to reach the
// router; from then on the operator does everything through the API.
func SeedScript(p *Params) string {
	var script strings.Builder
	script.WriteString("#!/bin/vbash\n")
	script.WriteString("# Written by router-api. Applied once, on the first boot.\n")
	script.WriteString("[ -e /config/router-api/seeded ] && exit 0\n")
	script.WriteString("source /opt/vyatta/etc/functions/script-template\n")
	script.WriteString("configure\n")
	script.WriteString(command.Path{"system", "host-name", p.HostName}.String() + "\n")
	// The plaintext password is what VyOS ships; it has to go, or the
	// commit would turn it back into a usable hash.
	script.WriteString("delete system login user vyos authentication plaintext-password\n")
	for _, path := range Desired(nil, p) {
		script.WriteString(path.String() + "\n")
	}
	script.WriteString("commit\n")
	script.WriteString("save\n")
	script.WriteString("exit\n")
	// "exit" leaves configuration mode, not the script.
	script.WriteString("mkdir -p /config/router-api\n")
	script.WriteString("touch /config/router-api/seeded\n")
	return script.String()
}

func quadlet(p *Params) string {
	return `[Unit]
Description=VyOS router (managed by router-api)
Wants=network-online.target
After=network-online.target

[Container]
ContainerName=vyos
Image=` + p.Image + `
Network=host
PodmanArgs=--privileged --stop-signal=SIGRTMIN+3
Volume=/lib/modules:/lib/modules:ro
Volume=` + HostConfigDir + `:/opt/vyatta/etc/config

[Service]
Restart=always
RestartSec=5
TimeoutStartSec=900

[Install]
WantedBy=multi-user.target
`
}

type writeFile struct {
	Path        string `json:"path"`
	Permissions string `json:"permissions"`
	Encoding    string `json:"encoding"`
	Content     string `json:"content"`
}

func file(filePath, permissions, content string) writeFile {
	// Base64 keeps YAML from having an opinion about the content.
	return writeFile{Path: filePath, Permissions: permissions, Encoding: "b64", Content: base64.StdEncoding.EncodeToString([]byte(content))}
}

// UserData renders the cloud-init user data that turns a stock Ubuntu server
// into the router described by p.
func UserData(p *Params) (string, error) {
	const setupPath = "/usr/local/sbin/router-api-host-setup"
	if !imageReference.MatchString(p.Image) {
		// The image name becomes a line of a systemd unit file.
		return "", fmt.Errorf("image %q is not an image reference", p.Image)
	}
	files := []writeFile{
		file(HostConfigDir+"/scripts/vyos-postconfig-bootup.script", "0755", SeedScript(p)),
		file("/etc/containers/systemd/vyos.container", "0644", quadlet(p)),
		file(setupPath, "0755", hostSetup),
	}
	for _, f := range p.Files {
		// The API validates the pattern; this is the last line of defence
		// against writing somewhere else on the host.
		clean := path.Clean(f.Path)
		if !strings.HasPrefix(clean, "/config/") || clean != f.Path {
			return "", fmt.Errorf("file path %q has to be a clean path below /config", f.Path)
		}
		permissions := f.Permissions
		if permissions == "" {
			permissions = "0600"
		}
		files = append(files, file(HostConfigDir+strings.TrimPrefix(clean, "/config"), permissions, f.Content))
	}

	raw, err := yaml.Marshal(map[string]any{
		"package_update": true,
		"packages":       []string{"podman"},
		"write_files":    files,
		"runcmd": [][]string{
			{setupPath, p.Image, p.PublicInterface, p.PrivateInterface},
		},
	})
	if err != nil {
		return "", fmt.Errorf("render user data: %w", err)
	}
	userData := "#cloud-config\n" + string(raw)
	if len(userData) > MaxUserDataBytes {
		return "", fmt.Errorf("user data is too large: %d bytes, the limit is %d; reduce the size of spec.files", len(userData), MaxUserDataBytes)
	}
	return userData, nil
}
