package apply

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hauke-cloud/router-api/internal/config/vyos/client"
	"github.com/hauke-cloud/router-api/internal/config/vyos/command"
	"github.com/hauke-cloud/router-api/internal/config/vyos/vyostest"
)

const before = `set system host-name old
set service ntp server time1.vyos.net
set system login user vyos authentication encrypted-password x
`

const after = `set system host-name new
set firewall ipv4 input filter default-action drop
`

var keep = []command.Path{{"system", "login"}}

func setup(t *testing.T, config string) (*vyostest.Router, *client.Client) {
	t.Helper()
	router := vyostest.New(t, config)
	c, err := client.New(&client.Options{URL: router.URL, Key: router.Key, CACertPEM: router.CertPEM, ServerName: vyostest.ServerName})
	if err != nil {
		t.Fatal(err)
	}
	return router, c
}

func parse(t *testing.T, text string) []command.Path {
	t.Helper()
	paths, err := command.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func sorted(lines ...string) []string {
	slices.Sort(lines)
	return lines
}

func TestApply(t *testing.T) {
	router, c := setup(t, before)

	outcome, err := Apply(context.Background(), c, parse(t, after), keep, 2)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	want := sorted(
		"set system host-name new",
		"set firewall ipv4 input filter default-action drop",
		"set system login user vyos authentication encrypted-password x",
	)
	if got := router.Running(); !slices.Equal(got, want) {
		t.Errorf("running = %q, want %q", got, want)
	}
	if got := router.Saved(); !slices.Equal(got, want) {
		t.Errorf("saved = %q, want %q", got, want)
	}
	if router.ConfirmPending() {
		t.Error("the change was left unconfirmed")
	}
	if !outcome.Changed {
		t.Error("Changed = false")
	}

	running, _ := c.Commands(context.Background())
	if outcome.RunningHash != command.Hash(running) {
		t.Error("RunningHash is not the hash of what the router runs now")
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	router, c := setup(t, before)
	ctx := context.Background()

	if _, err := Apply(ctx, c, parse(t, after), keep, 2); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	configures := router.Configures()

	outcome, err := Apply(ctx, c, parse(t, after), keep, 2)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if outcome.Changed {
		t.Error("second Apply reports a change")
	}
	if router.Configures() != configures {
		t.Error("second Apply touched the router")
	}
}

func TestApplyRollsBackAFailedCommit(t *testing.T) {
	router, c := setup(t, before)
	router.RejectCommitsOf("firewall")
	saved := router.Saved()

	_, err := Apply(context.Background(), c, parse(t, after), keep, 2)

	var commitErr *CommitError
	if !errors.As(err, &commitErr) {
		t.Fatalf("err = %v, want a *CommitError", err)
	}
	if !commitErr.RolledBack {
		t.Errorf("RolledBack = false: %v", commitErr)
	}
	if !strings.Contains(commitErr.Error(), "Commit failed") {
		t.Errorf("error does not carry the router's message: %v", commitErr)
	}

	// A real router keeps the part of a failed commit that applied and does
	// not arm the revert timer. Left alone it would now run a host name
	// without the firewall that was supposed to come with it.
	if got := router.Running(); !slices.Equal(got, sorted(strings.Split(strings.TrimSpace(before), "\n")...)) {
		t.Errorf("running = %q, want the configuration from before", got)
	}
	if got := router.Saved(); !slices.Equal(got, saved) {
		t.Errorf("saved = %q, want it untouched", got)
	}
}

func TestApplyDoesNotSaveWithoutConfirm(t *testing.T) {
	router, c := setup(t, before)
	router.FailNext("/config-file", 1)
	saved := router.Saved()

	_, err := Apply(context.Background(), c, parse(t, after), keep, 2)

	if !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("err = %v, want ErrUnconfirmed", err)
	}
	// Not being able to confirm is exactly the situation commit-confirm is
	// for. The router must be left to revert, and nothing may be saved.
	if !router.ConfirmPending() {
		t.Error("the revert timer was stopped")
	}
	if got := router.Saved(); !slices.Equal(got, saved) {
		t.Errorf("saved = %q, want it untouched", got)
	}

	router.ExpireConfirm()
	if got := router.Running(); !slices.Equal(got, sorted(strings.Split(strings.TrimSpace(before), "\n")...)) {
		t.Errorf("running after the timer = %q, want the configuration from before", got)
	}
}

func TestPlan(t *testing.T) {
	_, c := setup(t, before)

	changes, hash, err := Plan(context.Background(), c, parse(t, after), keep)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(changes) == 0 {
		t.Error("Plan found nothing to change")
	}
	if hash != command.Hash(parse(t, before)) {
		t.Error("hash is not the hash of the running configuration")
	}
}

func TestSettleConfirmsAndSavesWhatAnEarlierAttemptLeftOpen(t *testing.T) {
	router, c := setup(t, before)
	router.FailNext("/config-file", 1)
	if _, err := Apply(context.Background(), c, parse(t, after), keep, 2); !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("err = %v, want ErrUnconfirmed", err)
	}
	saved := router.Saved()

	// The router runs the new configuration, with the revert timer armed and
	// nothing saved. The connection is back.
	if err := Settle(context.Background(), c); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	if router.ConfirmPending() {
		t.Error("the revert timer is still running: the router will undo a configuration that is reported as applied")
	}
	if slices.Equal(router.Saved(), saved) || !slices.Equal(router.Saved(), router.Running()) {
		t.Errorf("saved = %q, running = %q: a reboot would bring the old configuration back", router.Saved(), router.Running())
	}
}

func TestSettleWithNothingPending(t *testing.T) {
	router, c := setup(t, before)
	// Nothing to confirm is not an error here: the point is that nothing is
	// left open, and nothing is.
	if err := Settle(context.Background(), c); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if !slices.Equal(router.Saved(), router.Running()) {
		t.Error("not saved")
	}
}

func TestSettleReportsAFailedConfirm(t *testing.T) {
	router, c := setup(t, before)
	router.FailNext("/config-file", 1)
	if err := Settle(context.Background(), c); !errors.Is(err, ErrUnconfirmed) {
		t.Errorf("err = %v, want ErrUnconfirmed", err)
	}
}
