//go:build linux

// Reading the kernel's clock discipline through adjtimex.
//
// The call is made with an all-zero `Timex`, which means mode zero, which means
// read without adjusting. Getting that wrong would have the control plane
// *setting* the system clock, so it is worth saying out loud: nothing here
// writes.

package timecode

import (
	"time"

	"golang.org/x/sys/unix"
)

func readHealth() (Health, error) {
	// Zero modes: a query. Anything non-zero here would apply an adjustment.
	buffer := unix.Timex{}
	state, err := unix.Adjtimex(&buffer)
	if err != nil {
		return Health{}, err
	}

	// Two independent ways the kernel says it is adrift, and both are worth
	// checking. STA_UNSYNC is set by the daemon when it gives up; TIME_ERROR
	// is the call's own verdict. A host whose chrony was killed shows the
	// first without the second for a while.
	synchronised := state != unix.TIME_ERROR && buffer.Status&unix.STA_UNSYNC == 0

	// Maxerror is microseconds regardless of STA_NANO, which only changes the
	// units of the offset and precision fields. The conversion is written to
	// survive the field being int32 on some architectures and int64 on others.
	maxError := time.Duration(buffer.Maxerror) * time.Microsecond
	if maxError < 0 {
		maxError = 0
	}

	return Health{
		Synchronised: synchronised,
		MaxError:     maxError,
		LeapPending: state == unix.TIME_INS || state == unix.TIME_DEL ||
			buffer.Status&(unix.STA_INS|unix.STA_DEL) != 0,
	}, nil
}
