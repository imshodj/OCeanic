//go:build !linux && !windows && !darwin

package agent

import (
	"context"
	"errors"
	"github.com/coder/websocket"
)

func openScreen() (desktopScreen, error) {
	return nil, errors.New("remote desktop is not supported on this OS")
}

func launchDesktopHelper(ctx context.Context, sid string, dial func() (*websocket.Conn, error), onSAS func()) error {
	return errors.New("remote desktop is not supported on this OS")
}

func sendSAS() {}
