package client

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/hauke-cloud/router-api/internal/config/vyos/command"
	"github.com/hauke-cloud/router-api/internal/config/vyos/vyostest"
)

func newClient(t *testing.T, router *vyostest.Router) *Client {
	t.Helper()
	c, err := New(&Options{URL: router.URL, Key: router.Key, CACertPEM: router.CertPEM, ServerName: vyostest.ServerName})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestInfo(t *testing.T) {
	router := vyostest.New(t, "")
	info, err := newClient(t, router).Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Version != "2026.10.07-0712-rolling" || info.Hostname != "vyos" {
		t.Errorf("info = %+v", info)
	}
}

func TestCommands(t *testing.T) {
	router := vyostest.New(t, "set system host-name edge\nset interfaces ethernet eth0 description 'WAN uplink'\n")
	paths, err := newClient(t, router).Commands(context.Background())
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	want := []string{"set system host-name edge", "set interfaces ethernet eth0 description 'WAN uplink'"}
	if got := command.Lines(paths); !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestConfigureConfirmSave(t *testing.T) {
	router := vyostest.New(t, "set system host-name old\n")
	c := newClient(t, router)
	ctx := context.Background()

	changes := []command.Change{
		{Op: command.OpDelete, Path: command.Path{"system", "host-name", "old"}},
		{Op: command.OpSet, Path: command.Path{"system", "host-name", "new"}},
	}
	if err := c.Configure(ctx, changes, 2); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if !router.ConfirmPending() {
		t.Fatal("the change was applied without a revert timer")
	}
	if got := router.Running(); !slices.Equal(got, []string{"set system host-name new"}) {
		t.Errorf("running = %q", got)
	}

	if err := c.Confirm(ctx); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if router.ConfirmPending() {
		t.Error("still pending after Confirm")
	}
	if got := router.Saved(); !slices.Equal(got, []string{"set system host-name old"}) {
		t.Errorf("saved before Save = %q: a commit must not persist by itself", got)
	}

	if err := c.Save(ctx); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := router.Saved(); !slices.Equal(got, []string{"set system host-name new"}) {
		t.Errorf("saved = %q", got)
	}
}

func TestConfirmWithNothingPending(t *testing.T) {
	// A real router answers 200 "No confirm pending". That is the one case
	// where the change the caller believes it is confirming is already gone.
	router := vyostest.New(t, "")
	err := newClient(t, router).Confirm(context.Background())
	if !errors.Is(err, ErrNothingToConfirm) {
		t.Errorf("err = %v, want ErrNothingToConfirm", err)
	}
}

func TestConfigureFailure(t *testing.T) {
	router := vyostest.New(t, "")
	router.RejectCommitsOf("firewall")
	c := newClient(t, router)

	err := c.Configure(context.Background(), []command.Change{
		{Op: command.OpSet, Path: command.Path{"firewall", "ipv4", "input", "filter", "default-action", "drop"}},
	}, 1)

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an *APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest || !strings.Contains(apiErr.Message, "Commit failed") {
		t.Errorf("apiErr = %+v", apiErr)
	}
	if strings.Contains(apiErr.Message, "sudo: unable to resolve host") {
		t.Errorf("message still carries sudo noise: %q", apiErr.Message)
	}
}

func TestConfigureNothing(t *testing.T) {
	router := vyostest.New(t, "")
	if err := newClient(t, router).Configure(context.Background(), nil, 1); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if n := router.Configures(); n != 0 {
		t.Errorf("an empty change set made %d requests", n)
	}
}

func TestWrongKey(t *testing.T) {
	router := vyostest.New(t, "")
	c, err := New(&Options{URL: router.URL, Key: "wrong", CACertPEM: router.CertPEM, ServerName: vyostest.ServerName})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Commands(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("err = %v, want a 401 *APIError", err)
	}
}

func TestCertificateIsPinned(t *testing.T) {
	router := vyostest.New(t, "")
	other := vyostest.New(t, "")

	// The right key, but a certificate that is not this router's: someone
	// answering on the router's address must not get the API key.
	c, err := New(&Options{URL: router.URL, Key: router.Key, CACertPEM: other.CertPEM, ServerName: vyostest.ServerName})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Info(context.Background()); err == nil {
		t.Fatal("a request to a server with another certificate succeeded")
	}
	if len(router.Requests()) != 0 {
		t.Errorf("requests reached the server: %q", router.Requests())
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	router := vyostest.New(t, "")
	for name, opts := range map[string]*Options{
		"no CA":    {URL: router.URL, Key: "k", ServerName: "x"},
		"bad CA":   {URL: router.URL, Key: "k", ServerName: "x", CACertPEM: []byte("nope")},
		"no key":   {URL: router.URL, CACertPEM: router.CertPEM, ServerName: "x"},
		"http URL": {URL: "http://192.0.2.1", Key: "k", CACertPEM: router.CertPEM, ServerName: "x"},
	} {
		if _, err := New(opts); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
}

func TestShowStripsNoise(t *testing.T) {
	router := vyostest.New(t, "")
	router.SetVRRP("Name  Interface  VRID  State   Priority  Last Transition\nwan   eth1       10    MASTER  100       5m\n")
	out, err := newClient(t, router).Show(context.Background(), "vrrp")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if !strings.HasPrefix(out, "Name") {
		t.Errorf("output = %q", out)
	}
}
