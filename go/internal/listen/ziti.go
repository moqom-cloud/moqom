package listen

import (
	"net"

	"github.com/openziti/sdk-golang/ziti"
)

// openZiti binds service on the overlay as the identity in identityFile.
//
// The identity file is the enrolled JSON an OpenZiti enrolment produces: a
// client certificate, its key, the controller's address and the authority to
// trust it by. It is read once here and is as sensitive as any private key.
func openZiti(identityFile, service string) (net.Listener, error) {
	config, err := ziti.NewConfigFromFile(identityFile)
	if err != nil {
		return nil, err
	}
	overlay, err := ziti.NewContext(config)
	if err != nil {
		return nil, err
	}

	// Default options, and in particular no terminator identity: the overlay
	// routes a dial to a terminator that has one only when the dial names it,
	// so binding under this identity's name would make the service unreachable
	// to every relay and operator that dials it by service alone.
	bound, err := overlay.ListenWithOptions(service, ziti.DefaultListenOptions())
	if err != nil {
		overlay.Close()
		return nil, err
	}
	return &zitiListener{Listener: bound, overlay: overlay}, nil
}

// zitiListener closes the overlay context with the listener, so a control
// plane that stops serving also stops holding a session with the controller.
type zitiListener struct {
	net.Listener
	overlay ziti.Context
}

func (l *zitiListener) Close() error {
	err := l.Listener.Close()
	l.overlay.Close()
	return err
}
