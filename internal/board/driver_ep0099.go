package board

import (
	"fmt"
	"sort"

	"github.com/OrbitOS-org/sdk-go/v26/client"
	"github.com/OrbitOS-org/sdk-go/v26/logger"
)

// I2C relay command bytes for the 52Pi EP-0099, confirmed against:
//
//	i2cset -y <bus> 0x10 <channel> 0xFF   # relay on
//	i2cset -y <bus> 0x10 <channel> 0x00   # relay off
//
// The channel number doubles as the device register — no separate bitmask
// or channel→register table is needed for this board. Other I2C boards may
// use a completely different protocol (e.g. a single bitmask register) —
// each gets its own driver type and file, referenced from its registry
// entry's I2CNewDriver field, rather than sharing this one.
const (
	ep0099RelayOn  byte = 0xFF
	ep0099RelayOff byte = 0x00
)

// Each EP-0099 is jumpered to one of 4 addresses (0x10-0x13), normally to
// stack up to ep0099MaxStack boards on one bus. A lone board doesn't have
// to be jumpered to ep0099BaseAddr specifically — see NewEP0099Driver.
const (
	ep0099BaseAddr         uint32 = 0x10
	ep0099MaxStack                = 4
	ep0099ChannelsPerBoard        = 4
)

// ep0099Driver is a RelayDriver bound to whichever EP-0099s actually
// responded on the bus when it was opened. It also implements
// StackedRelayDriver so the setup wizard's I2C scan can report what it
// found.
type ep0099Driver struct {
	bus   *client.I2CBus
	addrs []uint32 // discovered board addresses, ascending; addrs[slot] is that board's address
}

// NewEP0099Driver scans the bus for EP-0099s and builds a RelayDriver bound
// to whichever of the 4 valid addresses (0x10-0x13) actually responded,
// ordered ascending — so a lone board jumpered to any of those addresses is
// found and controlled correctly, not just one jumpered to the default
// 0x10. If the scan itself fails (e.g. unsupported on this bus), falls back
// to assuming a single board at the default address, matching this
// driver's behavior before stack scanning existed — but a successful scan
// that finds nothing is trusted as-is (no boards discovered), since that's
// a real answer, not a missing one.
func NewEP0099Driver(bus *client.I2CBus) RelayDriver {
	found, err := bus.Scan()
	if err != nil {
		logger.Warnf(logTag, "EP-0099 I2C scan failed, assuming single board at 0x%02X: %v", ep0099BaseAddr, err)
		return &ep0099Driver{bus: bus, addrs: []uint32{ep0099BaseAddr}}
	}
	addrs := ep0099ValidAddrs(found)
	logger.Infof(logTag, "EP-0099 I2C scan found addresses: %s — using: %s", formatI2CAddrs(found), formatI2CAddrs(addrs))
	return &ep0099Driver{bus: bus, addrs: addrs}
}

// ep0099ValidAddrs filters addrs to the EP-0099's valid range (0x10-0x13)
// and returns them sorted ascending.
func ep0099ValidAddrs(addrs []uint32) []uint32 {
	var valid []uint32
	for _, a := range addrs {
		if a >= ep0099BaseAddr && a < ep0099BaseAddr+ep0099MaxStack {
			valid = append(valid, a)
		}
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i] < valid[j] })
	return valid
}

func (d *ep0099Driver) SetChannel(channel int, on bool) error {
	slot := (channel - 1) / ep0099ChannelsPerBoard
	if slot >= len(d.addrs) {
		return fmt.Errorf("channel %d: no EP-0099 discovered at stack slot %d", channel, slot)
	}
	localChannel := byte((channel-1)%ep0099ChannelsPerBoard + 1)
	val := ep0099RelayOff
	if on {
		val = ep0099RelayOn
	}
	_, err := d.bus.Transfer(d.addrs[slot], []byte{localChannel, val}, 0, 0)
	return err
}

func (d *ep0099Driver) Discovered() (boards, channels int) {
	return len(d.addrs), len(d.addrs) * ep0099ChannelsPerBoard
}
