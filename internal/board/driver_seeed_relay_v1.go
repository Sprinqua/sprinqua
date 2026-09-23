package board

import (
	"fmt"
	"sort"

	"github.com/OrbitOS-org/sdk-go/v26/client"
	"github.com/OrbitOS-org/sdk-go/v26/logger"
)

// I2C relay protocol for the Seeed Studio Raspberry Pi Relay Board v1.0,
// confirmed against the vendor's reference driver:
// https://wiki.seeedstudio.com/Raspberry_Pi_Relay_Board_v1.0/
//
//	i2cset -y <bus> 0x20 0x06 <byte>   # one byte, all 4 relays on one board
//
// Unlike the EP-0099 (one register per channel), each board exposes a single
// output register where each bit controls one of its 4 relays, active-low
// (bit=0 means on). There's no per-channel write, so the driver keeps the
// last written byte per board address in memory and flips one bit per
// SetChannel call before resending the whole byte — matching the reference
// driver's DEVICE_REG_DATA pattern.
const seeedRelayReg byte = 0x06

// Each board is set via 3 address-select switches (A0-A2) to one of 8
// addresses (0x20-0x27), normally to stack up to seeedRelayMaxStack boards
// on one bus. A lone board doesn't have to be set to seeedRelayBaseAddr
// specifically — see NewSeeedRelayDriver.
const (
	seeedRelayBaseAddr         uint32 = 0x20
	seeedRelayMaxStack                = 8
	seeedRelayChannelsPerBoard        = 4
)

// seeedRelayDriver is a RelayDriver bound to whichever Seeed Studio Relay
// Board v1.0 units actually responded on the bus when it was opened. It
// also implements StackedRelayDriver so the setup wizard's I2C scan can
// report what it found.
type seeedRelayDriver struct {
	bus   *client.I2CBus
	addrs []uint32        // discovered board addresses, ascending; addrs[slot] is that board's address
	state map[uint32]byte // last written byte per board address
}

// NewSeeedRelayDriver scans the bus for Seeed Relay Board v1.0 units and
// builds a RelayDriver bound to whichever of the 8 valid addresses
// (0x20-0x27) actually responded, ordered ascending — so a lone board set
// to any of those addresses is found and controlled correctly, not just one
// set to the default 0x20. If the scan itself fails (e.g. unsupported on
// this bus), falls back to assuming a single board at the default address
// — but a successful scan that finds nothing is trusted as-is (no boards
// discovered), since that's a real answer, not a missing one.
func NewSeeedRelayDriver(bus *client.I2CBus) RelayDriver {
	found, err := bus.Scan()
	if err != nil {
		logger.Warnf(logTag, "Seeed relay I2C scan failed, assuming single board at 0x%02X: %v", seeedRelayBaseAddr, err)
		return &seeedRelayDriver{bus: bus, addrs: []uint32{seeedRelayBaseAddr}, state: make(map[uint32]byte)}
	}
	addrs := seeedRelayValidAddrs(found)
	logger.Infof(logTag, "Seeed relay I2C scan found addresses: %s — using: %s", formatI2CAddrs(found), formatI2CAddrs(addrs))
	return &seeedRelayDriver{bus: bus, addrs: addrs, state: make(map[uint32]byte)}
}

// seeedRelayValidAddrs filters addrs to the board's valid range (0x20-0x27)
// and returns them sorted ascending.
func seeedRelayValidAddrs(addrs []uint32) []uint32 {
	var valid []uint32
	for _, a := range addrs {
		if a >= seeedRelayBaseAddr && a < seeedRelayBaseAddr+seeedRelayMaxStack {
			valid = append(valid, a)
		}
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i] < valid[j] })
	return valid
}

func (d *seeedRelayDriver) SetChannel(channel int, on bool) error {
	slot := (channel - 1) / seeedRelayChannelsPerBoard
	if slot >= len(d.addrs) {
		return fmt.Errorf("channel %d: no board discovered at stack slot %d", channel, slot)
	}
	addr := d.addrs[slot]
	localChannel := (channel-1)%seeedRelayChannelsPerBoard + 1
	bit := byte(1) << (localChannel - 1)

	state, ok := d.state[addr]
	if !ok {
		state = 0xFF // all relays off (active-low) until this board is first written
	}
	if on {
		state &^= bit // active-low: clear bit = on
	} else {
		state |= bit
	}
	d.state[addr] = state
	_, err := d.bus.Transfer(addr, []byte{seeedRelayReg, state}, 0, 0)
	return err
}

func (d *seeedRelayDriver) Discovered() (boards, channels int) {
	return len(d.addrs), len(d.addrs) * seeedRelayChannelsPerBoard
}
