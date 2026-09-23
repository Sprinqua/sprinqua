package board

import "github.com/OrbitOS-org/sdk-go/v26/client"

// Channel maps a relay channel number to a GPIO pin.
type Channel struct {
	Number int
	Pin    *client.GpioPin
}

// Kind identifies how a board's relays are driven.
type Kind string

const (
	KindGPIO Kind = "" // zero value — existing boards stay unchanged
	KindI2C  Kind = "i2c"
)

// Board describes a supported relay board.
type Board struct {
	ID          string
	Name        string
	Description string
	SKU         string
	Channels    int
	Kind        Kind      // KindGPIO (default) or KindI2C
	ActiveLow   bool      // GPIO boards only — LOW signal activates the relay
	Pins        []Channel // GPIO boards only

	// I2C boards only (Kind == KindI2C). I2CNewDriver is board-specific —
	// different I2C boards can use completely different wire protocols and
	// addressing schemes (not user-configurable beyond switches on the
	// board itself), so each one provides its own constructor (see
	// driver_*.go) instead of the Engine assuming a single shared
	// implementation. Stackable boards also implement StackedRelayDriver,
	// which exposes their own address space for the setup wizard's I2C scan
	// — there's no shared base-address/channels-per-board convention here
	// because that knowledge belongs to each board, not the registry.
	I2CBus       uint32
	I2CNewDriver func(bus *client.I2CBus) RelayDriver
}

// PinByChannel returns the GPIO pin for the given 1-based channel number.
func (b *Board) PinByChannel(ch int) *client.GpioPin {
	for _, c := range b.Pins {
		if c.Number == ch {
			return c.Pin
		}
	}
	return nil
}

// All is the registry of supported relay boards.
// Order matches docs/index.html (fewer to more channels, same grouping as the site).
var All = []*Board{
	{
		ID:          "sbcomponents-2ch",
		Name:        "SB Components 2-Channel",
		Description: "Zero Relay - 2-channel board. Ideal for compact 2-zone setups.",
		SKU:         "14088",
		Channels:    2,
		ActiveLow:   false,
		Pins: []Channel{
			{Number: 1, Pin: &client.GpioPin{Name: "GPIO22"}}, // Board Pin 15
			{Number: 2, Pin: &client.GpioPin{Name: "GPIO5"}},  // Board Pin 29
		},
	},
	{
		ID:          "waveshare-3ch",
		Name:        "Waveshare 3-Channel",
		Description: "3-channel relay HAT for Raspberry Pi via direct GPIO.",
		SKU:         "11638",
		Channels:    3,
		ActiveLow:   false,
		Pins: []Channel{
			{Number: 1, Pin: &client.GpioPin{Name: "GPIO26"}},
			{Number: 2, Pin: &client.GpioPin{Name: "GPIO20"}},
			{Number: 3, Pin: &client.GpioPin{Name: "GPIO21"}},
		},
	},
	{
		ID:          "seengreat-3ch",
		Name:        "Seengreat 3-Channel",
		Description: "3-channel relay expansion board for Raspberry Pi via direct GPIO.",
		SKU:         "250509",
		Channels:    3,
		ActiveLow:   false,
		Pins: []Channel{
			{Number: 1, Pin: &client.GpioPin{Name: "GPIO26"}},
			{Number: 2, Pin: &client.GpioPin{Name: "GPIO19"}},
			{Number: 3, Pin: &client.GpioPin{Name: "GPIO13"}},
		},
	},
	{
		ID:          "keyestudio-4ch",
		Name:        "Keyestudio 4-Channel",
		Description: "4-channel relay board for Raspberry Pi via direct GPIO.",
		SKU:         "KS0212",
		Channels:    4,
		ActiveLow:   false,
		Pins: []Channel{
			{Number: 1, Pin: &client.GpioPin{Name: "GPIO4"}},
			{Number: 2, Pin: &client.GpioPin{Name: "GPIO22"}},
			{Number: 3, Pin: &client.GpioPin{Name: "GPIO6"}},
			{Number: 4, Pin: &client.GpioPin{Name: "GPIO26"}},
		},
	},
	{
		ID:          "seengreat-4ch",
		Name:        "Seengreat 4-Channel",
		Description: "4-channel relay expansion board for Raspberry Pi via direct GPIO.",
		SKU:         "220741",
		Channels:    4,
		ActiveLow:   false,
		Pins: []Channel{
			{Number: 1, Pin: &client.GpioPin{Name: "GPIO26"}},
			{Number: 2, Pin: &client.GpioPin{Name: "GPIO19"}},
			{Number: 3, Pin: &client.GpioPin{Name: "GPIO13"}},
			{Number: 4, Pin: &client.GpioPin{Name: "GPIO6"}},
		},
	},
	{
		ID:          "bc-robotics-4ch",
		Name:        "BC Robotics 4-Channel",
		Description: "Raspberry Pi 4 channel 10A relay HAT.",
		SKU:         "RAS-193",
		Channels:    4,
		ActiveLow:   false,
		Pins: []Channel{
			{Number: 1, Pin: &client.GpioPin{Name: "GPIO4"}},
			{Number: 2, Pin: &client.GpioPin{Name: "GPIO17"}},
			{Number: 3, Pin: &client.GpioPin{Name: "GPIO27"}},
			{Number: 4, Pin: &client.GpioPin{Name: "GPIO22"}},
		},
	},
	{
		// Up to 4 EP-0099s can be stacked on the same bus at consecutive
		// addresses (0x10-0x13) for up to 16 channels total — see
		// driver_ep0099.go for the channel→address mapping. Registered with
		// the max channel count; users with fewer boards disable the extra
		// zones in Settings after setup (zones 5-16 won't respond otherwise).
		//
		// ID kept as "52pi-4ch-i2c" (from when this only covered 4 channels)
		// even though it now covers 4-16 — config.json persists this ID, and
		// changing it strands anyone who already completed setup (main.go
		// can no longer find their board on the next boot).
		ID:           "52pi-ep-0099-i2c",
		Name:         "52Pi EP-0099 4/8/12/16-Channel",
		Description:  "4-channel I2C relay board, stackable up to 4 boards (max. 16 channels).",
		SKU:          "EP-0099",
		Channels:     16,
		Kind:         KindI2C,
		I2CBus:       1,
		I2CNewDriver: NewEP0099Driver,
	},
	{
		// Up to 8 boards can be stacked on the same bus via the A0-A2
		// address switches (0x20-0x27) for up to 32 channels total — see
		// driver_seeed_relay_v1.go for the channel→address mapping.
		// Registered with the max channel count; users with fewer boards
		// disable the extra zones in Settings after setup.
		ID:           "seeed-relay-v1-i2c",
		Name:         "Seeed Studio 4-Channel SPDT 4/8/../32-Channel",
		Description:  "4-channel SPDT relay board for Raspberry Pi via I2C, stackable up to 8 boards (max. 32 channels).",
		SKU:          "103030029",
		Channels:     32,
		Kind:         KindI2C,
		I2CBus:       1,
		I2CNewDriver: NewSeeedRelayDriver,
	},
	{
		ID:          "waveshare-pi0-6ch",
		Name:        "Waveshare RPi Zero 6-Channel",
		Description: "6-channel Industrial Relay Module for Raspberry Pi Zero.",
		SKU:         "20863",
		Channels:    6,
		ActiveLow:   false,
		Pins: []Channel{
			{Number: 1, Pin: &client.GpioPin{Name: "GPIO5"}},
			{Number: 2, Pin: &client.GpioPin{Name: "GPIO6"}},
			{Number: 3, Pin: &client.GpioPin{Name: "GPIO13"}},
			{Number: 4, Pin: &client.GpioPin{Name: "GPIO16"}},
			{Number: 5, Pin: &client.GpioPin{Name: "GPIO19"}},
			{Number: 6, Pin: &client.GpioPin{Name: "GPIO20"}},
		},
	},
	{
		ID:          "waveshare-8ch",
		Name:        "Waveshare 8-Channel",
		Description: "8-channel relay HAT for Raspberry Pi via direct GPIO.",
		SKU:         "15423",
		Channels:    8,
		ActiveLow:   true,
		Pins: []Channel{
			{Number: 1, Pin: &client.GpioPin{Name: "GPIO5"}},
			{Number: 2, Pin: &client.GpioPin{Name: "GPIO6"}},
			{Number: 3, Pin: &client.GpioPin{Name: "GPIO13"}},
			{Number: 4, Pin: &client.GpioPin{Name: "GPIO16"}},
			{Number: 5, Pin: &client.GpioPin{Name: "GPIO19"}},
			{Number: 6, Pin: &client.GpioPin{Name: "GPIO20"}},
			{Number: 7, Pin: &client.GpioPin{Name: "GPIO21"}},
			{Number: 8, Pin: &client.GpioPin{Name: "GPIO26"}},
		},
	},
	{
		ID:          "seengreat-8ch",
		Name:        "Seengreat 8-Channel",
		Description: "8-channel optocoupler-isolated relay expansion board for Raspberry Pi, 5–12V wide voltage input.",
		SKU:         "260115",
		Channels:    8,
		ActiveLow:   false,
		Pins: []Channel{
			{Number: 1, Pin: &client.GpioPin{Name: "GPIO6"}},
			{Number: 2, Pin: &client.GpioPin{Name: "GPIO13"}},
			{Number: 3, Pin: &client.GpioPin{Name: "GPIO19"}},
			{Number: 4, Pin: &client.GpioPin{Name: "GPIO26"}},
			{Number: 5, Pin: &client.GpioPin{Name: "GPIO12"}},
			{Number: 6, Pin: &client.GpioPin{Name: "GPIO16"}},
			{Number: 7, Pin: &client.GpioPin{Name: "GPIO20"}},
			{Number: 8, Pin: &client.GpioPin{Name: "GPIO21"}},
		},
	},
}

// Find returns the board with the given ID, or nil.
func Find(id string) *Board {
	for _, b := range All {
		if b.ID == id {
			return b
		}
	}
	return nil
}
