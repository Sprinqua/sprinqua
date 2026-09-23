package board

import (
	"fmt"

	"github.com/OrbitOS-org/sdk-go/v26/client"
)

// ChannelManager opens the right RelayDriver for a board — GPIO or I2C —
// so callers don't need to branch on Board.Kind themselves.
type ChannelManager struct {
	gpio *client.GpioManager
	i2c  *client.I2CManager
}

// NewChannelManager wraps the GPIO and I2C managers behind one entry point.
func NewChannelManager(gpio *client.GpioManager, i2c *client.I2CManager) *ChannelManager {
	return &ChannelManager{gpio: gpio, i2c: i2c}
}

// Open returns a RelayDriver ready to switch b's channels on or off.
func (m *ChannelManager) Open(b *Board) (RelayDriver, error) {
	if b.Kind == KindI2C {
		return OpenI2CDriver(m.i2c, b)
	}
	return NewGPIODriver(m.gpio, b), nil
}

// DetectStackedChannels opens board b's driver (which scans the I2C bus as
// part of opening — see StackedRelayDriver) and reports how many boards and
// total channels were actually discovered. Returns an error if b isn't a
// stackable I2C board.
func (m *ChannelManager) DetectStackedChannels(b *Board) (channels, boards int, err error) {
	drv, err := m.Open(b)
	if err != nil {
		return 0, 0, err
	}
	sd, ok := drv.(StackedRelayDriver)
	if !ok {
		return 0, 0, fmt.Errorf("board %q does not support I2C stack detection", b.ID)
	}
	boards, channels = sd.Discovered()
	return channels, boards, nil
}
