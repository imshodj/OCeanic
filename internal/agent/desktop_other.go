//go:build !linux && !windows

package agent

import (
	"context"
	"errors"
)

func openScreen() (desktopScreen, error) {
	return nil, errors.New("remote desktop is not supported on this OS")
}

func launchDesktopHelper(ctx context.Context, sid string, onSAS func()) error {
	return errors.New("remote desktop is not supported on this OS")
}

func sendSAS() {}
