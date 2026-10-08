// Package apply makes a router run a configuration without ever leaving it
// somewhere in between.
package apply

import (
	"context"
	"errors"
	"fmt"

	"github.com/hauke-cloud/router-api/internal/config/vyos/client"
	"github.com/hauke-cloud/router-api/internal/config/vyos/command"
)

// ErrUnconfirmed means a change was committed but could not be confirmed. The
// router reverts it by itself when its timer runs out; nothing was saved.
var ErrUnconfirmed = errors.New("the change could not be confirmed and will be reverted by the router")

// CommitError means the router rejected a change.
type CommitError struct {
	// Err is the router's answer.
	Err error
	// RolledBack is true if the configuration from before the attempt is
	// running again. If it is false the router runs a mix of both.
	RolledBack bool
	// RollbackErr is why the rollback failed, if it did.
	RollbackErr error
}

func (e *CommitError) Error() string {
	if e.RolledBack {
		return fmt.Sprintf("commit failed and was rolled back: %v", e.Err)
	}
	return fmt.Sprintf("commit failed: %v; rolling back failed too: %v", e.Err, e.RollbackErr)
}

func (e *CommitError) Unwrap() error { return e.Err }

// Router is the part of the REST client Apply needs.
type Router interface {
	Commands(ctx context.Context) ([]command.Path, error)
	Configure(ctx context.Context, changes []command.Change, confirmMinutes int) error
	Confirm(ctx context.Context) error
	Save(ctx context.Context) error
}

var _ Router = (*client.Client)(nil)

// Outcome is the result of an Apply.
type Outcome struct {
	// Changed is true if the router's configuration was changed.
	Changed bool
	// RunningHash identifies the configuration the router runs now. VyOS
	// rewrites some values when it stores them (a plaintext password becomes
	// a hash), so this is not the hash of what was asked for. Comparing it
	// with the hash of a later read is how drift is detected.
	RunningHash string
}

// Plan returns the changes Apply would make, and the hash of the running
// configuration.
func Plan(ctx context.Context, router Router, desired, keep []command.Path) ([]command.Change, string, error) {
	current, err := router.Commands(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("read the running configuration: %w", err)
	}
	return command.Diff(current, desired, keep), command.Hash(current), nil
}

// Apply makes the router run desired, keeping whatever lies below a path in
// keep.
//
// The change is committed with a revert timer of confirmMinutes and then
// confirmed over the same connection that made it. A change that cuts the
// operator off from the router is therefore never confirmed and undoes
// itself. Only a confirmed change is saved.
//
// A commit the router rejects is rolled back here: the router keeps the part
// of a failed commit that applied and arms no timer for it.
func Apply(ctx context.Context, router Router, desired, keep []command.Path, confirmMinutes int) (Outcome, error) {
	before, err := router.Commands(ctx)
	if err != nil {
		return Outcome{}, fmt.Errorf("read the running configuration: %w", err)
	}
	changes := command.Diff(before, desired, keep)
	if len(changes) == 0 {
		return Outcome{RunningHash: command.Hash(before)}, nil
	}

	if err := router.Configure(ctx, changes, confirmMinutes); err != nil {
		commitErr := &CommitError{Err: err}
		commitErr.RollbackErr = rollback(ctx, router, before)
		commitErr.RolledBack = commitErr.RollbackErr == nil
		return Outcome{}, commitErr
	}

	if err := router.Confirm(ctx); err != nil {
		return Outcome{}, fmt.Errorf("%w: %w", ErrUnconfirmed, err)
	}
	if err := router.Save(ctx); err != nil {
		return Outcome{}, fmt.Errorf("save the configuration: %w", err)
	}

	after, err := router.Commands(ctx)
	if err != nil {
		return Outcome{}, fmt.Errorf("read the configuration back: %w", err)
	}
	return Outcome{Changed: true, RunningHash: command.Hash(after)}, nil
}

// rollback puts the configuration from before a failed commit back. It runs
// without a revert timer: there is nothing sensible to revert to from here.
func rollback(ctx context.Context, router Router, before []command.Path) error {
	now, err := router.Commands(ctx)
	if err != nil {
		return fmt.Errorf("read the running configuration: %w", err)
	}
	return router.Configure(ctx, command.Diff(now, before, nil), 0)
}
