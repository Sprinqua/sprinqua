package board

import (
	"fmt"
	"strings"

	"github.com/OrbitOS-org/sdk-go/v26/client"
)

const logTag = "board"

// formatI2CAddrs renders a list of I2C addresses as hex for logging, e.g.
// "0x10, 0x11".
func formatI2CAddrs(addrs []uint32) string {
	if len(addrs) == 0 {
		return "(none)"
	}
	parts := make([]string, len(addrs))
	for i, a := range addrs {
		parts[i] = fmt.Sprintf("0x%02X", a)
	}
	return strings.Join(parts, ", ")
}

// RelayDriver switches a single relay channel on or off, with whatever
// bus/pins/address it needs already bound. Implemented by each board kind
// (see driver_gpio.go, driver_ep0099.go) — callers go through
// ChannelManager.Open instead of constructing one directly.
type RelayDriver interface {
	SetChannel(channel int, on bool) error
}

// StackedRelayDriver is implemented by RelayDrivers for I2C boards that can
// be stacked at multiple addresses on one bus (see driver_ep0099.go,
// driver_seeed_relay_v1.go). Each driver scans the bus when it's opened and
// binds its channels to whichever of its valid addresses actually
// responded — so a lone board jumpered to any valid address (not just the
// documented default) is still found and controlled correctly, instead of
// the app assuming board 1 always sits at a fixed base address.
type StackedRelayDriver interface {
	RelayDriver
	// Discovered reports how many boards responded when the driver was
	// opened, and the total channel count across them.
	Discovered() (boards, channels int)
}

// OpenI2CDriver opens the I2C bus configured for board b (standard
// 100kHz/7-bit-address settings) and returns a RelayDriver ready to switch
// its channels.
func OpenI2CDriver(mgr *client.I2CManager, b *Board) (RelayDriver, error) {
	bus, err := mgr.Open(b.I2CBus, 100000, false, false)
	if err != nil {
		return nil, err
	}
	return b.I2CNewDriver(bus), nil
}
