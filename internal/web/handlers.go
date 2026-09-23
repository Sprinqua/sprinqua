package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OrbitOS-org/sdk-go/v26/logger"
	"sprinqua/internal/adjustment"
	"sprinqua/internal/board"
	"sprinqua/internal/config"
	"sprinqua/internal/history"
	"sprinqua/internal/i18n"
	"sprinqua/internal/scheduler"
	"sprinqua/internal/weather"
	"sprinqua/internal/zone"
)

// ── Language switcher ─────────────────────────────────────────────────────────

func (s *Server) handleLang(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	supported := i18n.Supported()
	ok := false
	for _, s := range supported {
		if code == s {
			ok = true
			break
		}
	}
	if !ok {
		http.Error(w, "unsupported language", http.StatusBadRequest)
		return
	}
	setLangCookie(w, code)
	ref := r.Referer()
	if ref == "" {
		ref = "/"
	}
	http.Redirect(w, r, ref, http.StatusFound)
}

// ── Root ─────────────────────────────────────────────────────────────────────

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SetupDone {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/setup/wizard", http.StatusFound)
}

// ── Setup Wizard ──────────────────────────────────────────────────────────────

type step1Data struct {
	basePage
	Boards  []*board.Board
	HwModel string // raw model string from Gravity RT
	HwOK    bool   // true only if a Raspberry Pi was detected
}

type step2Data struct {
	basePage
	Board *board.Board
	Zones []config.Zone
}

type step3Data struct {
	basePage
	Board *board.Board
	Zones []config.Zone
}

type step4Data struct {
	basePage
	MQTT       config.MQTTConfig
	TimeFormat string
}

type langOption struct {
	Code  string
	Label string
}

type settingsData struct {
	basePage
	MQTT           config.MQTTConfig
	TimeFormat     string
	WinterMode     bool
	PassiveMode    bool
	MQTTConnected  bool
	SmartWatering  config.SmartWateringConfig
	EToCalculating bool
	BoardName      string
	ZoneCount      int
	Zones          []config.Zone
	SupportedLangs []langOption
	ImportErr      string // "invalid" | "incomplete" | ""
}

var langLabels = map[string]string{
	"en": "🇬🇧 English",
	"pt": "🇵🇹 Português",
	"de": "🇩🇪 Deutsch",
	"es": "🇪🇸 Español",
	"fr": "🇫🇷 Français",
	"it": "🇮🇹 Italiano",
}

// handleSetup renders the Settings page (always the entry point for the Setup tab).
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.SetupDone {
		http.Redirect(w, r, "/setup/wizard", http.StatusFound)
		return
	}
	tf := s.cfg.TimeFormat
	if tf == "" {
		tf = "24h"
	}
	var boardName string
	if b := board.Find(s.cfg.Board); b != nil {
		boardName = b.Name
	}
	var langs []langOption
	for _, code := range i18n.Supported() {
		langs = append(langs, langOption{Code: code, Label: langLabels[code]})
	}
	sw := s.cfg.SmartWatering
	if sw.RainThresholdMM <= 0 {
		sw.RainThresholdMM = 2.0
	}
	s.etoMu.Lock()
	etoCalc := s.etoCalculating
	s.etoMu.Unlock()

	s.render(w, "settings", settingsData{
		basePage:       s.page(r),
		MQTT:           s.cfg.MQTT,
		TimeFormat:     tf,
		WinterMode:     s.cfg.WinterMode,
		PassiveMode:    s.cfg.MQTT.IsPassive(),
		MQTTConnected:  s.mqttClient.IsConnected(),
		SmartWatering:  sw,
		EToCalculating: etoCalc,
		BoardName:      boardName,
		ZoneCount:      len(s.cfg.Zones),
		Zones:          s.cfg.Zones,
		SupportedLangs: langs,
		ImportErr:      r.URL.Query().Get("import_err"),
	})
}

// isRaspberryPi reports whether the hardware model string identifies a Raspberry Pi.
func isRaspberryPi(model string) bool {
	return strings.Contains(strings.ToLower(model), "raspberry pi")
}

// handleSetupWizard starts the hardware wizard (step 1).
// It blocks the wizard if the detected hardware is not a Raspberry Pi.
func (s *Server) handleSetupWizard(w http.ResponseWriter, r *http.Request) {
	s.render(w, "wizard", step1Data{
		basePage: s.page(r),
		Boards:   board.All,
		HwModel:  s.hwModel,
		HwOK:     isRaspberryPi(s.hwModel),
	})
}

// handleSettingsSave persists clock format and MQTT preferences.
func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	tf := r.FormValue("time_format")
	if tf != "12h" {
		tf = "24h"
	}
	s.cfg.TimeFormat = tf

	if r.FormValue("mqtt_enabled") == "1" {
		port, _ := strconv.Atoi(r.FormValue("mqtt_port"))
		if port <= 0 {
			port = 1883
		}
		prefix := r.FormValue("mqtt_prefix")
		if prefix == "" {
			prefix = "sprinqua"
		}
		mode := r.FormValue("mqtt_mode")
		if mode != "passive" {
			mode = "active"
		}
		s.cfg.MQTT = config.MQTTConfig{
			Enabled:  true,
			Mode:     mode,
			Broker:   r.FormValue("mqtt_broker"),
			Port:     port,
			Username: r.FormValue("mqtt_user"),
			Password: r.FormValue("mqtt_pass"),
			Prefix:   prefix,
		}
	} else {
		s.cfg.MQTT.Enabled = false
		s.cfg.MQTT.Mode = "active"
	}
	s.cfg.WinterMode = r.FormValue("winter_mode") == "1"
	s.sched.SetPaused(s.cfg.MQTT.IsPassive() || s.cfg.WinterMode)

	swEnabled := r.FormValue("sw_enabled") == "1"
	swSkipEnabled := r.FormValue("sw_skip_enabled") == "1"
	swLat, _ := strconv.ParseFloat(r.FormValue("sw_lat"), 64)
	swLon, _ := strconv.ParseFloat(r.FormValue("sw_lon"), 64)
	swThresh, _ := strconv.ParseFloat(r.FormValue("sw_threshold"), 64)
	if swThresh <= 0 {
		swThresh = 2.0
	}
	swFrost, _ := strconv.ParseFloat(r.FormValue("sw_frost_threshold"), 64)
	if swFrost < 0 {
		swFrost = 0
	}
	swMethod := r.FormValue("sw_method")
	switch swMethod {
	case "manual", "monthly", "zimmerman", "eto":
	default:
		swMethod = ""
	}
	swManualPct, _ := strconv.ParseFloat(r.FormValue("sw_manual_pct"), 64)
	if swManualPct <= 0 {
		swManualPct = 100
	}
	var swMonthly [12]float64
	for i := 0; i < 12; i++ {
		v, err := strconv.ParseFloat(r.FormValue(fmt.Sprintf("sw_monthly_%d", i)), 64)
		if err != nil || v < 0 {
			v = 100
		}
		swMonthly[i] = v
	}

	// Zimmerman parameters
	zimmBT, _ := strconv.ParseFloat(r.FormValue("zimm_bt"), 64)
	zimmBH, _ := strconv.ParseFloat(r.FormValue("zimm_bh"), 64)
	zimmBP, _ := strconv.ParseFloat(r.FormValue("zimm_bp"), 64)
	zimmWT, _ := strconv.ParseFloat(r.FormValue("zimm_wt"), 64)
	zimmWH, _ := strconv.ParseFloat(r.FormValue("zimm_wh"), 64)
	zimmWP, _ := strconv.ParseFloat(r.FormValue("zimm_wp"), 64)
	altitude, _ := strconv.ParseFloat(r.FormValue("sw_altitude"), 64)

	// Preserve calculated ETo baseline and rain delay override across settings saves
	prevBaseline := s.cfg.SmartWatering.EToBaseline
	prevBaselineAt := s.cfg.SmartWatering.EToBaselineCalculatedAt
	prevRainDelayCleared := s.cfg.SmartWatering.RainDelayClearedRainDate

	swRainDelayDays, _ := strconv.Atoi(r.FormValue("sw_rain_delay_days"))
	if swRainDelayDays < 0 {
		swRainDelayDays = 0
	}
	if swRainDelayDays > 14 {
		swRainDelayDays = 14
	}
	if swRainDelayDays == 0 {
		prevRainDelayCleared = ""
	}

	s.cfg.SmartWatering = config.SmartWateringConfig{
		Enabled:                  swEnabled,
		SkipEnabled:              swSkipEnabled,
		Lat:                      swLat,
		Lon:                      swLon,
		RainThresholdMM:          swThresh,
		FrostThresholdC:          swFrost,
		RainDelayDays:            swRainDelayDays,
		RainDelayClearedRainDate: prevRainDelayCleared,
		Method:                   swMethod,
		ManualPct:                swManualPct,
		MonthlyPct:               swMonthly,
		ZimmBT:                   zimmBT,
		ZimmBH:                   zimmBH,
		ZimmBP:                   zimmBP,
		ZimmWT:                   zimmWT,
		ZimmWH:                   zimmWH,
		ZimmWP:                   zimmWP,
		Altitude:                 altitude,
		EToBaseline:              prevBaseline,
		EToBaselineCalculatedAt:  prevBaselineAt,
	}

	if err := s.cfg.Save(s.dataDir); err != nil {
		logger.Errorf(logTag, "save settings: %v", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}

	// Auto-trigger ETo baseline calculation on first selection of "eto" method
	if swMethod == "eto" && prevBaseline == 0 && swLat != 0 {
		go s.triggerEToBaseline()
	}

	// Reconnect MQTT with updated config.
	if s.engine != nil {
		s.mqttClient.Connect(s.cfg.MQTT, s.cfg.Zones, s.engine, s.hist)
	}

	http.Redirect(w, r, "/dashboard", http.StatusFound)
}

// handleZonesSave updates the existing zones' name, type, max duration and
// enabled flag without touching the board, schedules or history.
func (s *Server) handleZonesSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	for i := range s.cfg.Zones {
		id := s.cfg.Zones[i].ID

		name := strings.TrimSpace(r.FormValue(fmt.Sprintf("zone_%d_name", id)))
		if name == "" {
			name = fmt.Sprintf("Zone %d", id)
		}

		zoneType := r.FormValue(fmt.Sprintf("zone_%d_type", id))
		switch zoneType {
		case "drip", "sprinkler", "mist":
		default:
			zoneType = "sprinkler"
		}

		maxMins, _ := strconv.Atoi(r.FormValue(fmt.Sprintf("zone_%d_max", id)))
		if maxMins <= 0 {
			maxMins = 30
		}
		pulseMins := parseZonePulseMins(r, id, maxMins)

		s.cfg.Zones[i].Name = name
		s.cfg.Zones[i].Type = zoneType
		s.cfg.Zones[i].MaxSecs = maxMins * 60
		s.cfg.Zones[i].PulseSecs = pulseMins * 60
		s.cfg.Zones[i].Enabled = r.FormValue(fmt.Sprintf("zone_%d_enabled", id)) == "1"
	}

	if err := s.cfg.Save(s.dataDir); err != nil {
		logger.Errorf(logTag, "save zones: %v", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}

	if s.engine != nil {
		s.engine.SetZones(s.cfg.Zones)
		s.mqttClient.Connect(s.cfg.MQTT, s.cfg.Zones, s.engine, s.hist)
	}

	http.Redirect(w, r, "/dashboard", http.StatusFound)
}

// handleSetupReset clears zones, schedules and history, then redirects to the wizard.
func (s *Server) handleSetupReset(w http.ResponseWriter, r *http.Request) {
	s.cfg.SetupDone = false
	s.cfg.Board = ""
	s.cfg.Zones = nil
	s.cfg.Schedules = nil
	s.cfg.SmartWatering = config.SmartWateringConfig{}
	// Intentionally keep TimeFormat and MQTT — they are preferences, not HW config.

	s.engine = nil
	s.board = nil
	if s.sched != nil {
		s.sched.SetEngine(nil)
	}

	if err := s.cfg.Save(s.dataDir); err != nil {
		logger.Errorf(logTag, "reset config: %v", err)
	}
	if s.hist != nil {
		s.hist.Clear()
	}
	http.Redirect(w, r, "/setup/wizard", http.StatusFound)
}

// initBoardPins opens the board's relay driver and ensures every channel
// starts OFF. Called once when a board is selected.
func (s *Server) initBoardPins(b *board.Board) {
	drv, err := s.chMgr.Open(b)
	if err != nil {
		logger.Warnf(logTag, "open relay driver for board %q: %v", b.ID, err)
		return
	}
	for ch := 1; ch <= b.Channels; ch++ {
		if err := drv.SetChannel(ch, false); err != nil {
			logger.Warnf(logTag, "init ch%d off: %v", ch, err)
		}
	}
	logger.Infof(logTag, "board %q: %d channels initialized OFF", b.ID, b.Channels)
}

func (s *Server) handleSetupChannels(w http.ResponseWriter, r *http.Request) {
	b := board.Find(r.URL.Query().Get("board_id"))
	if b == nil {
		b = board.All[0]
	}
	strs := i18n.Strings(s.lang(r))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if b.Description != "" {
		fmt.Fprintf(w, `<p class="text-xs text-slate-400 w-full">%s</p>`, b.Description)
	}
	if b.SKU != "" {
		fmt.Fprintf(w, `<p class="text-[11px] font-mono text-slate-400 w-full mb-1">SKU: %s</p>`, b.SKU)
	}
	previewChannels := b.Channels
	if b.Kind == board.KindI2C {
		channels, boards, err := s.chMgr.DetectStackedChannels(b)
		previewChannels = channels
		switch {
		case err != nil:
			logger.Warnf(logTag, "i2c scan board %q: %v", b.ID, err)
			fmt.Fprintf(w, `<p class="text-xs text-red-500 w-full">%s</p>`, strs["setup_i2c_scan_failed"])
		case channels == 0:
			fmt.Fprintf(w, `<p class="text-xs text-amber-500 w-full">%s</p>`, strs["setup_i2c_scan_none"])
		default:
			fmt.Fprintf(w, `<input type="hidden" name="detected_channels" value="%d">`, channels)
			fmt.Fprintf(w, `<p class="text-xs text-emerald-600 w-full">%s</p>`,
				fmt.Sprintf(strs["setup_i2c_scan_found"], boards, channels))
		}
	}
	s.renderContinueButton(w, strs, previewChannels > 0)
	if previewChannels == 0 {
		return
	}
	fmt.Fprintf(w, `<span class="text-xs text-slate-400">%s</span>`, strs["step1_channels_label"])
	for ch := 1; ch <= previewChannels; ch++ {
		color := zoneColors[(ch-1)%len(zoneColors)]
		fmt.Fprintf(w, ` <span class="text-xs font-semibold text-white px-2.5 py-1 rounded-full" style="background-color: %s">CH%d</span>`,
			color, ch)
	}
}

// renderContinueButton emits an out-of-band swap of the step 1 submit
// button so it lives outside #channel-badges (htmx finds it by id
// regardless of where it appears in this fragment). Used to disable
// continuing when an I2C board is selected but no boards were detected —
// otherwise the wizard would silently fall back to the registered maximum
// and the user wouldn't notice until testing relays in step 3.
func (s *Server) renderContinueButton(w http.ResponseWriter, strs map[string]string, enabled bool) {
	disabledAttr := ""
	if !enabled {
		disabledAttr = " disabled"
	}
	fmt.Fprintf(w, `<button type="submit" id="step1-continue-btn" hx-swap-oob="true"
		class="w-full bg-emerald-600 hover:bg-emerald-700 active:bg-emerald-800 text-white font-semibold py-3 px-4 rounded-xl transition-colors text-sm disabled:opacity-50 disabled:cursor-not-allowed"%s>%s</button>`,
		disabledAttr, strs["btn_continue"])
}

func (s *Server) handleSetupStep1(w http.ResponseWriter, r *http.Request) {
	boardID := r.FormValue("board_id")
	b := board.Find(boardID)
	if b == nil {
		http.Error(w, "invalid board", http.StatusBadRequest)
		return
	}
	channels := b.Channels
	if b.Kind == board.KindI2C {
		detected, err := strconv.Atoi(r.FormValue("detected_channels"))
		if err != nil || detected <= 0 || detected > b.Channels {
			http.Error(w, "no I2C boards detected — check wiring and address, then go back and retry", http.StatusBadRequest)
			return
		}
		channels = detected
	}
	s.cfg.Board = boardID

	zones := make([]config.Zone, channels)
	for i := range zones {
		zones[i] = config.Zone{
			ID:        i + 1,
			Name:      fmt.Sprintf("Zone %d", i+1),
			Channel:   i + 1,
			Type:      "sprinkler",
			MaxSecs:   30 * 60,
			PulseSecs: config.DefaultPulseSecs,
			Enabled:   true,
		}
	}
	s.cfg.Zones = zones

	s.render(w, "step2", step2Data{basePage: s.page(r), Board: b, Zones: zones})
}

func (s *Server) handleSetupStep2(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	b := board.Find(s.cfg.Board)
	if b == nil {
		http.Error(w, "board not configured", http.StatusBadRequest)
		return
	}

	for i := range s.cfg.Zones {
		id := s.cfg.Zones[i].ID
		name := r.FormValue(fmt.Sprintf("zone_%d_name", id))
		if name == "" {
			name = fmt.Sprintf("Zone %d", id)
		}
		zoneType := r.FormValue(fmt.Sprintf("zone_%d_type", id))
		if zoneType == "" {
			zoneType = "sprinkler"
		}
		maxMins, _ := strconv.Atoi(r.FormValue(fmt.Sprintf("zone_%d_max", id)))
		if maxMins <= 0 {
			maxMins = 30
		}
		pulseMins := parseZonePulseMins(r, id, maxMins)
		s.cfg.Zones[i].Name = name
		s.cfg.Zones[i].Type = zoneType
		s.cfg.Zones[i].MaxSecs = maxMins * 60
		s.cfg.Zones[i].PulseSecs = pulseMins * 60
		s.cfg.Zones[i].Enabled = r.FormValue(fmt.Sprintf("zone_%d_enabled", id)) == "1"
	}

	s.initBoardPins(b)
	s.render(w, "step3", step3Data{basePage: s.page(r), Board: b, Zones: s.cfg.Zones})
}

func (s *Server) handleSetupTest(w http.ResponseWriter, r *http.Request) {
	ch, err := strconv.Atoi(r.PathValue("channel"))
	if err != nil || ch < 1 {
		http.Error(w, "invalid channel", http.StatusBadRequest)
		return
	}
	b := board.Find(s.cfg.Board)
	if b == nil {
		http.Error(w, "board not configured", http.StatusBadRequest)
		return
	}
	if ch > b.Channels {
		http.Error(w, "channel not in board", http.StatusBadRequest)
		return
	}
	drv, err := s.chMgr.Open(b)
	if err != nil {
		http.Error(w, "relay driver unavailable", http.StatusInternalServerError)
		return
	}
	setLevel := func(on bool) error { return drv.SetChannel(ch, on) }

	// Allow only one relay test at a time — reject if already active.
	s.testMu.Lock()
	if s.testCancel != nil {
		s.testMu.Unlock()
		http.Error(w, "test already active", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.testCancel = cancel
	s.testMu.Unlock()

	_ = setLevel(false)
	if err := setLevel(true); err != nil {
		logger.Warnf(logTag, "test ch%d ON: %v", ch, err)
	}
	go func() {
		defer func() {
			s.testMu.Lock()
			s.testCancel = nil
			s.testMu.Unlock()
		}()
		select {
		case <-time.After(3 * time.Second):
			if err := setLevel(false); err != nil {
				logger.Warnf(logTag, "test ch%d OFF: %v", ch, err)
			} else {
				logger.Infof(logTag, "test ch%d OFF", ch)
			}
		case <-ctx.Done():
			_ = setLevel(false)
		}
	}()

	strs := i18n.Strings(s.lang(r))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<span class="text-emerald-600 text-sm font-medium">%s</span>`,
		fmt.Sprintf(strs["test_activated"], ch))
}

// hxRedirectPath returns the absolute path to use for an HX-Redirect header.
// Unlike the standard "Location" header, HX-Redirect is not rewritten by
// path-prefix reverse proxies (e.g. the OrbitOS AppHub portal serving the app
// under "/sprinqua"), so a hardcoded "/path" would drop that prefix and send
// the browser to the portal root instead of back into the app. We recover the
// prefix from the Referer (the browser's current, prefix-aware URL) by
// swapping out everything from "/setup" onwards.
func hxRedirectPath(r *http.Request, path string) string {
	ref := r.Referer()
	if ref == "" {
		return path
	}
	u, err := url.Parse(ref)
	if err != nil {
		return path
	}
	if idx := strings.Index(u.Path, "/setup"); idx >= 0 {
		return u.Path[:idx] + path
	}
	return path
}

// handleSetupStep3 finalizes the wizard: saves config, initializes engine, redirects to settings.
func (s *Server) handleSetupStep3(w http.ResponseWriter, r *http.Request) {
	s.cfg.SetupDone = true
	if err := s.cfg.Save(s.dataDir); err != nil {
		logger.Errorf(logTag, "save config: %v", err)
		http.Error(w, "failed to save config", http.StatusInternalServerError)
		return
	}
	b := board.Find(s.cfg.Board)
	if b != nil {
		s.board = b
		eng := zone.New(s.chMgr, b, s.cfg.Zones, s.cfg.IsExclusiveMode())
		eng.Init()
		eng.SetHistory(s.hist)
		s.engine = eng
		if s.sched != nil {
			s.sched.SetEngine(eng)
		}
		logger.Infof(logTag, "zone engine initialized with %d zones", len(s.cfg.Zones))
		s.mqttClient.Connect(s.cfg.MQTT, s.cfg.Zones, eng, s.hist)
	}
	w.Header().Set("HX-Redirect", hxRedirectPath(r, "/setup"))
	w.WriteHeader(http.StatusOK)
}

// ── Dashboard ─────────────────────────────────────────────────────────────────

type dashboardData struct {
	basePage
	Zones              []zone.State
	WinterMode         bool
	PassiveMode        bool
	RainDelayActive    bool
	RainDelayDaysLeft  int
	HasNextRun         bool
	NextRunName        string
	NextRunWhen        string
	NextSwBadge        string
	NextHasAdj         bool
	NextSwEstimate     bool
	NextTotalRunMins   int
	IsRunning          bool
	RunningSchedName   string
	RunningSwBadge     string
	RunningHasAdj      bool
	RunningSwEstimate  bool
	RunningTotalMins   int
	HasWeather         bool
	WeatherTempMinC    float64
	WeatherTempMaxC    float64
	WeatherHumidityPct float64
	WeatherWindKmh     float64
	WeatherRainMM      float64
}

func scheduleDisplayName(sc config.Schedule, strs map[string]string) string {
	if sc.Name != "" {
		return sc.Name
	}
	return fmt.Sprintf(strs["sched_program_num"], sc.ID)
}

// fetchYesterdayFor returns yesterday's weather actuals when the configured method needs
// them for an estimate (zimmerman/eto), or nil otherwise.
func fetchYesterdayFor(sw config.SmartWateringConfig) *weather.DailyData {
	if !sw.Enabled || sw.Lat == 0 || (sw.Method != "zimmerman" && sw.Method != "eto") {
		return nil
	}
	d, err := weather.FetchYesterday(sw.Lat, sw.Lon)
	if err != nil {
		return nil
	}
	return d
}

// computeScheduleSWDisplay returns smart-watering badge and effective total run minutes for UI.
func computeScheduleSWDisplay(sc config.Schedule, sw config.SmartWateringConfig, yesterday *weather.DailyData) (mult float64, swBadge string, hasAdj, swEstimate bool, totalRunMins int) {
	mult = 1.0
	totalRunMins = sc.TotalRunMins()
	if !sc.SmartWatering || !sw.Enabled || sw.Method == "" {
		return
	}
	switch sw.Method {
	case "manual", "monthly":
		mult = adjustment.Calc(sw, nil)
		if mult != 1.0 {
			hasAdj = true
			swBadge = fmt.Sprintf("×%.2g", mult)
		}
	default:
		swBadge = "~"
		if yesterday != nil {
			mult = adjustment.Calc(sw, yesterday)
			if mult != 1.0 {
				hasAdj = true
				swEstimate = true
				swBadge = fmt.Sprintf("~×%.2g", mult)
			}
		}
	}
	if hasAdj {
		totalRunMins = 0
		for j, z := range sc.Zones {
			totalRunMins += int(math.Round(float64(z.DurMins) * mult))
			if j < len(sc.Zones)-1 {
				totalRunMins += z.SoakAfterMins
			}
		}
	}
	return
}

func formatNextRunWhen(t time.Time, use12h bool, strs map[string]string) string {
	now := time.Now()
	loc := now.Location()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	runDay := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)

	var dayPart string
	switch {
	case runDay.Equal(today):
		dayPart = strs["dash_next_today"]
	case runDay.Equal(today.AddDate(0, 0, 1)):
		dayPart = strs["dash_next_tomorrow"]
	default:
		dayPart = strs[fmt.Sprintf("day_%d", int(t.Weekday()))]
	}

	var timePart string
	if use12h {
		timePart = t.Format("3:04 PM")
	} else {
		timePart = t.Format("15:04")
	}
	return dayPart + " " + timePart
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	var states []zone.State
	if s.engine != nil {
		states = s.engine.States()
	}
	pg := s.page(r)
	use12h := s.cfg.TimeFormat == "12h"

	data := dashboardData{
		basePage:    pg,
		Zones:       states,
		WinterMode:  s.cfg.WinterMode,
		PassiveMode: s.cfg.MQTT.IsPassive(),
	}

	sw := s.cfg.SmartWatering
	if sw.Enabled && sw.SkipEnabled && sw.RainDelayDays > 0 && sw.Lat != 0 {
		if delay, err := weather.RainDelayStatus(sw, time.Now()); err == nil && delay.Active {
			data.RainDelayActive = true
			data.RainDelayDaysLeft = delay.DaysLeft
		}
	}

	if sw.Enabled && sw.Lat != 0 {
		if res, err := weather.FetchToday(sw.Lat, sw.Lon); err == nil {
			data.HasWeather = true
			data.WeatherTempMinC = res.TempMinC
			data.WeatherTempMaxC = res.TempMaxC
			data.WeatherHumidityPct = res.HumidityPct
			data.WeatherWindKmh = res.WindKmh
			data.WeatherRainMM = res.RainMM
		}
	}

	if s.sched != nil {
		if id, ok := s.sched.RunningID(); ok {
			for _, sc := range s.cfg.Schedules {
				if sc.ID == id {
					data.IsRunning = true
					data.RunningSchedName = scheduleDisplayName(sc, pg.S)
					if s.sched.RunningManual() {
						// Run now always uses raw durations — showing the SW
						// badge here would wrongly imply an adjustment is applied.
						data.RunningTotalMins = sc.TotalRunMins()
					} else {
						_, badge, hasAdj, est, totalRun := computeScheduleSWDisplay(sc, sw, fetchYesterdayFor(sw))
						data.RunningSwBadge = badge
						data.RunningHasAdj = hasAdj
						data.RunningSwEstimate = est
						data.RunningTotalMins = totalRun
					}
					break
				}
			}
		}
	}

	if !data.IsRunning && !data.WinterMode && !data.PassiveMode {
		if sc, when, ok := scheduler.NextRunGlobal(s.cfg.Schedules); ok {
			_, badge, hasAdj, est, totalRun := computeScheduleSWDisplay(sc, sw, fetchYesterdayFor(sw))
			data.HasNextRun = true
			data.NextRunName = scheduleDisplayName(sc, pg.S)
			data.NextRunWhen = formatNextRunWhen(when, use12h, pg.S)
			data.NextSwBadge = badge
			data.NextHasAdj = hasAdj
			data.NextSwEstimate = est
			data.NextTotalRunMins = totalRun
		}
	}

	s.render(w, "dashboard", data)
}

// ── Zone Fragment (HTMX polling) ──────────────────────────────────────────────

type zonesFragData struct {
	basePage
	Zones []zone.State
}

func (s *Server) handleZonesFragment(w http.ResponseWriter, r *http.Request) {
	var states []zone.State
	if s.engine != nil {
		states = s.engine.States()
	}
	s.render(w, "zones_fragment", zonesFragData{basePage: s.page(r), Zones: states})
}

// ── Zone Controls ─────────────────────────────────────────────────────────────

func (s *Server) handleZoneOn(w http.ResponseWriter, r *http.Request) {
	id, eng := s.zonePrecheck(w, r)
	if eng == nil {
		return
	}
	if err := eng.TurnOn(id); err != nil {
		logger.Errorf(logTag, "zone %d ON: %v", id, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if s.hist != nil {
		s.hist.Start(id, history.Manual)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleZoneOff(w http.ResponseWriter, r *http.Request) {
	id, eng := s.zonePrecheck(w, r)
	if eng == nil {
		return
	}
	if err := eng.TurnOff(id); err != nil {
		logger.Errorf(logTag, "zone %d OFF: %v", id, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if s.hist != nil {
		s.hist.Stop(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleZonePulse(w http.ResponseWriter, r *http.Request) {
	id, eng := s.zonePrecheck(w, r)
	if eng == nil {
		return
	}
	secs := config.DefaultPulseSecs
	if z, ok := s.cfg.ZoneMap()[id]; ok {
		secs = z.EffectivePulseSecs()
	}
	if err := eng.Pulse(id, secs); err != nil {
		logger.Errorf(logTag, "zone %d pulse: %v", id, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if s.hist != nil {
		hist := s.hist
		s.hist.Start(id, history.Pulse)
		go func() {
			time.Sleep(time.Duration(secs) * time.Second)
			hist.Stop(id)
		}()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) zonePrecheck(w http.ResponseWriter, r *http.Request) (int, *zone.Engine) {
	if s.engine == nil {
		http.Error(w, "engine not ready", http.StatusServiceUnavailable)
		return 0, nil
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		http.Error(w, "invalid zone id", http.StatusBadRequest)
		return 0, nil
	}
	return id, s.engine
}

func parseZonePulseMins(r *http.Request, zoneID, maxMins int) int {
	pulseMins, _ := strconv.Atoi(r.FormValue(fmt.Sprintf("zone_%d_pulse", zoneID)))
	if pulseMins <= 0 {
		pulseMins = config.DefaultPulseSecs / 60
	}
	if pulseMins > 120 {
		pulseMins = 120
	}
	if maxMins > 0 && pulseMins > maxMins {
		pulseMins = maxMins
	}
	return pulseMins
}

// ── Schedule ──────────────────────────────────────────────────────────────────

var zoneColors = []string{
	"#10b981", // emerald
	"#3b82f6", // blue
	"#8b5cf6", // violet
	"#f59e0b", // amber
	"#f43f5e", // rose
	"#0ea5e9", // sky
	"#f97316", // orange
	"#14b8a6", // teal
}

type legendEntry struct {
	Name  string
	Color string
}

type chartBar struct {
	Label     string
	Color     string
	LeftPct   string
	WidthPct  string
	TopPx     int
	StartTime string
	DurMins   int
}

type chartDay struct {
	Bars     []chartBar
	HeightPx int
}

type schedulePageData struct {
	basePage
	Schedules   []scheduleView
	Chart       []chartDay
	ZoneLegend  []legendEntry
	Ruler       [5]string // time labels at 0%, 25%, 50%, 75%, 100% of chart range
	PassiveMode bool      // true when MQTT passive mode is active
	WinterMode  bool      // true when winter mode is pausing the scheduler
}

type zoneStepView struct {
	ZoneID        int
	Name          string
	DurMins       int
	AdjDurMins    int    // effective duration after smart watering multiplier; 0 = same as DurMins
	HasAdj        bool   // true when a smart watering adjustment applies
	SoakAfterMins int    // pause after this zone before the next
	Color         string // persistent hex color matching the zone
}

type scheduleView struct {
	config.Schedule
	ZoneSteps       []zoneStepView
	TotalMins       int    // irrigation only
	TotalRunMins    int    // irrigation + soak pauses
	AdjTotalMins    int    // adjusted irrigation total; 0 = same as TotalMins
	AdjTotalRunMins int    // adjusted total including soak pauses
	HasAdj          bool   // true when SW adjustment differs from 1× (deterministic or estimated)
	SwEstimate      bool   // true when adjustment is a weather-based estimate (~×N)
	SwBadge         string // "×1.5" deterministic, "~×0.8" estimated, "~" if unknown, "" if SW off
	NextRun         string
	DisplayTime     string
	Running         bool // this program is the one currently mid-run
	OtherRunning    bool // a different program is mid-run, so Run now is blocked
}

type scheduleFormData struct {
	basePage
	Schedule             config.Schedule
	Zones                []config.Zone
	IsNew                bool
	DaysError            bool
	ZonesError           bool
	SmartWateringEnabled bool
	Use12h               bool
}

func (s *Server) buildSchedulePage(r *http.Request) schedulePageData {
	zmap := s.cfg.ZoneMap()
	pg := s.page(r)
	use12h := s.cfg.TimeFormat == "12h"

	// Stable color per zone (ordered by zone list).
	zoneColor := make(map[int]string, len(s.cfg.Zones))
	for i, z := range s.cfg.Zones {
		zoneColor[z.ID] = zoneColors[i%len(zoneColors)]
	}

	// Schedule card views.
	runningID, anyRunning := s.sched.RunningID()
	sw := s.cfg.SmartWatering

	// Fetch yesterday's weather once for Zimmerman/ETo schedule estimates.
	var yesterdayData *weather.DailyData
	if sw.Enabled && sw.Lat != 0 && (sw.Method == "zimmerman" || sw.Method == "eto") {
		if d, err := weather.FetchYesterday(sw.Lat, sw.Lon); err == nil {
			yesterdayData = d
		}
	}

	views := make([]scheduleView, len(s.cfg.Schedules))
	for i, sc := range s.cfg.Schedules {
		next := scheduler.NextRunFor(sc)
		nextStr := pg.S["sched_no_next"]
		if next != nil {
			if use12h {
				nextStr = next.Format("Mon 3:04 PM")
			} else {
				nextStr = next.Format("Mon 15:04")
			}
		}

		// Determine smart watering multiplier for display.
		mult, swBadge, hasAdj, swEstimate, adjRunTotal := computeScheduleSWDisplay(sc, sw, yesterdayData)
		adjTotal := 0
		if hasAdj {
			for _, z := range sc.Zones {
				adjTotal += int(math.Round(float64(z.DurMins) * mult))
			}
		}

		steps := make([]zoneStepView, len(sc.Zones))
		for j, z := range sc.Zones {
			adjDur := 0
			if hasAdj {
				adjDur = int(math.Round(float64(z.DurMins) * mult))
			}
			steps[j] = zoneStepView{
				ZoneID:        z.ZoneID,
				Name:          zmap[z.ZoneID].Name,
				DurMins:       z.DurMins,
				AdjDurMins:    adjDur,
				HasAdj:        hasAdj,
				SoakAfterMins: z.SoakAfterMins,
				Color:         zoneColor[z.ZoneID],
			}
		}

		views[i] = scheduleView{
			Schedule:        sc,
			ZoneSteps:       steps,
			TotalMins:       sc.TotalMins(),
			TotalRunMins:    sc.TotalRunMins(),
			AdjTotalMins:    adjTotal,
			AdjTotalRunMins: adjRunTotal,
			HasAdj:          hasAdj,
			SwEstimate:      swEstimate,
			SwBadge:         swBadge,
			NextRun:         nextStr,
			DisplayTime:     formatStartTime(sc.StartTime, use12h),
			Running:         sc.ID == runningID,
			OtherRunning:    anyRunning && sc.ID != runningID,
		}
	}

	// ── Weekly chart ─────────────────────────────────────────────────────────
	// Step 1: collect raw bars and find the time range of all enabled schedules.
	type rawBar struct {
		name     string
		color    string
		startMin int
		endMin   int
		dispTime string
		durMins  int
	}
	dayRaw := [7][]rawBar{}
	legendSeen := make(map[int]bool)
	var legend []legendEntry

	cStart, cEnd := 1440, 0
	for _, sc := range s.cfg.Schedules {
		if !sc.Enabled || sc.TotalMins() <= 0 {
			continue
		}
		t, err := time.Parse("15:04", sc.StartTime)
		if err != nil {
			continue
		}
		baseMin := t.Hour()*60 + t.Minute()
		offset := 0
		for i, step := range sc.Zones {
			sm := baseMin + offset
			em := sm + step.DurMins
			offset += step.DurMins
			if step.DurMins <= 0 {
				if i < len(sc.Zones)-1 {
					offset += step.SoakAfterMins
				}
				continue
			}
			if sm < cStart {
				cStart = sm
			}
			if em > cEnd {
				cEnd = em
			}
			color := zoneColor[step.ZoneID]
			name := zmap[step.ZoneID].Name
			if !legendSeen[step.ZoneID] {
				legendSeen[step.ZoneID] = true
			}
			rb := rawBar{
				name: name, color: color,
				startMin: sm, endMin: em,
				dispTime: formatStartTime(fmt.Sprintf("%02d:%02d", sm/60, sm%60), use12h),
				durMins:  step.DurMins,
			}
			for _, d := range sc.Days {
				if d >= 0 && d <= 6 {
					dayRaw[d] = append(dayRaw[d], rb)
				}
			}
			if i < len(sc.Zones)-1 {
				offset += step.SoakAfterMins
			}
		}
	}

	// Step 2: adaptive chart range — zoom to where schedules actually are,
	// with a 30-minute buffer and a 60-minute minimum span.
	if cStart > cEnd {
		cStart, cEnd = 0, 1440 // no enabled schedules, fall back to full day
	} else {
		cStart -= 30
		if cStart < 0 {
			cStart = 0
		}
		cEnd += 30
		if cEnd > 1440 {
			cEnd = 1440
		}
		if cEnd-cStart < 60 { // ensure minimum visible range
			mid := (cStart + cEnd) / 2
			cStart, cEnd = mid-30, mid+30
			if cStart < 0 {
				cStart, cEnd = 0, 60
			}
			if cEnd > 1440 {
				cStart, cEnd = 1380, 1440
			}
		}
	}
	cRange := cEnd - cStart

	// Step 3: build 5 ruler labels evenly across the adaptive range.
	var ruler [5]string
	for i := range ruler {
		m := cStart + cRange*i/4
		if m > 1440 {
			m = 1440
		}
		h, mn := m/60, m%60
		if use12h {
			tt, _ := time.Parse("15:04", fmt.Sprintf("%02d:%02d", h, mn))
			ruler[i] = tt.Format("3:04PM")
		} else {
			ruler[i] = fmt.Sprintf("%d:%02d", h, mn)
		}
	}

	// Step 4: lane packing per day using temporal positions.
	// With adaptive zoom bars are wide enough that temporal packing is correct.
	const minBarPct = 1.0
	chart := make([]chartDay, 7)
	for d := range chart {
		bars := dayRaw[d]
		sort.Slice(bars, func(i, j int) bool { return bars[i].startMin < bars[j].startMin })

		laneEnds := []int{}
		for _, rb := range bars {
			leftPct := float64(rb.startMin-cStart) / float64(cRange) * 100
			w := float64(rb.durMins) / float64(cRange) * 100
			if w < minBarPct {
				w = minBarPct
			}
			if leftPct+w > 100 {
				w = 100 - leftPct
			}

			lane := -1
			for i, end := range laneEnds {
				if rb.startMin >= end {
					lane = i
					break
				}
			}
			if lane == -1 {
				lane = len(laneEnds)
				laneEnds = append(laneEnds, 0)
			}
			laneEnds[lane] = rb.endMin

			chart[d].Bars = append(chart[d].Bars, chartBar{
				Label:     rb.name,
				Color:     rb.color,
				LeftPct:   fmt.Sprintf("%.2f", leftPct),
				WidthPct:  fmt.Sprintf("%.2f", w),
				TopPx:     lane * 22,
				StartTime: rb.dispTime,
				DurMins:   rb.durMins,
			})
		}
		if n := len(laneEnds); n == 0 {
			chart[d].HeightPx = 20
		} else {
			chart[d].HeightPx = n*22 + 4
		}
	}

	// Build legend ordered by zone list.
	for _, z := range s.cfg.Zones {
		if legendSeen[z.ID] {
			legend = append(legend, legendEntry{Name: zmap[z.ID].Name, Color: zoneColor[z.ID]})
		}
	}

	return schedulePageData{
		basePage:    pg,
		Schedules:   views,
		Chart:       chart,
		ZoneLegend:  legend,
		Ruler:       ruler,
		PassiveMode: s.cfg.MQTT.IsPassive(),
		WinterMode:  s.cfg.WinterMode,
	}
}

// formatStartTime converts a stored "HH:MM" string to display format.
func formatStartTime(hhmm string, use12h bool) string {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return hhmm
	}
	if use12h {
		return t.Format("3:04 PM")
	}
	return t.Format("15:04")
}

func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	s.render(w, "schedule", s.buildSchedulePage(r))
}

// handleScheduleFragment re-renders the self-polling program list so a
// "Run now" in progress (or one finishing) is reflected without a manual
// page reload, even after navigating away and back.
func (s *Server) handleScheduleFragment(w http.ResponseWriter, r *http.Request) {
	s.render(w, "schedule_list_wrapper", s.buildSchedulePage(r))
}

// enabledZones returns only the zones available for picking in a program —
// disabled zones can't be turned on, so they shouldn't be selectable here.
func enabledZones(zones []config.Zone) []config.Zone {
	out := make([]config.Zone, 0, len(zones))
	for _, z := range zones {
		if z.Enabled {
			out = append(out, z)
		}
	}
	return out
}

func (s *Server) handleScheduleNew(w http.ResponseWriter, r *http.Request) {
	s.render(w, "schedule_form", scheduleFormData{
		basePage:             s.page(r),
		Schedule:             config.Schedule{Enabled: true, StartTime: "08:00"},
		Zones:                enabledZones(s.cfg.Zones),
		IsNew:                true,
		DaysError:            r.URL.Query().Get("err") == "days",
		ZonesError:           r.URL.Query().Get("err") == "zones",
		SmartWateringEnabled: s.cfg.SmartWatering.Enabled,
		Use12h:               s.cfg.TimeFormat == "12h",
	})
}

func (s *Server) handleScheduleEdit(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	for _, sc := range s.cfg.Schedules {
		if sc.ID == id {
			s.render(w, "schedule_form", scheduleFormData{
				basePage:             s.page(r),
				Schedule:             sc,
				Zones:                enabledZones(s.cfg.Zones),
				IsNew:                false,
				DaysError:            r.URL.Query().Get("err") == "days",
				ZonesError:           r.URL.Query().Get("err") == "zones",
				SmartWateringEnabled: s.cfg.SmartWatering.Enabled,
				Use12h:               s.cfg.TimeFormat == "12h",
			})
			return
		}
	}
	http.NotFound(w, r)
}

func (s *Server) handleScheduleCreate(w http.ResponseWriter, r *http.Request) {
	sc := s.parseScheduleForm(r)
	if len(sc.Zones) == 0 {
		http.Redirect(w, r, "/schedule/new?err=zones", http.StatusFound)
		return
	}
	if len(sc.Days) == 0 {
		http.Redirect(w, r, "/schedule/new?err=days", http.StatusFound)
		return
	}
	sc.ID = s.cfg.NextScheduleID()
	s.cfg.Schedules = append(s.cfg.Schedules, sc)
	s.saveAsync()
	http.Redirect(w, r, "/schedule", http.StatusFound)
}

func (s *Server) handleScheduleUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	sc := s.parseScheduleForm(r)
	if len(sc.Zones) == 0 {
		http.Redirect(w, r, fmt.Sprintf("/schedule/%d/edit?err=zones", id), http.StatusFound)
		return
	}
	if len(sc.Days) == 0 {
		http.Redirect(w, r, fmt.Sprintf("/schedule/%d/edit?err=days", id), http.StatusFound)
		return
	}
	sc.ID = id
	for i, existing := range s.cfg.Schedules {
		if existing.ID == id {
			s.cfg.Schedules[i] = sc
			break
		}
	}
	s.saveAsync()
	http.Redirect(w, r, "/schedule", http.StatusFound)
}

// handleScheduleRun starts a program's zone sequence immediately, ignoring
// its enabled flag, days, start time and Smart Watering adjustment. Refuses
// with 409 if the program is already mid-run (clock-triggered or manual).
func (s *Server) handleScheduleRun(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	switch err := s.sched.RunNow(id); {
	case err == nil:
		// Render the full wrapper so the polling timer resets to t=0,
		// giving 3 clean seconds before any poll can race with this response.
		s.render(w, "schedule_list_wrapper", s.buildSchedulePage(r))
	case errors.Is(err, scheduler.ErrScheduleRunning):
		w.WriteHeader(http.StatusConflict)
	case errors.Is(err, scheduler.ErrScheduleNotFound):
		http.NotFound(w, r)
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}

// handleScheduleStop cancels a program's in-progress run: the zone it
// currently has on is turned off immediately and remaining zones in its
// sequence are skipped.
func (s *Server) handleScheduleStop(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	switch err := s.sched.StopRun(id); {
	case err == nil:
		// Render the full wrapper so the polling timer resets to t=0,
		// giving 3 clean seconds before any poll can race with this response.
		s.render(w, "schedule_list_wrapper", s.buildSchedulePage(r))
	case errors.Is(err, scheduler.ErrScheduleNotRunning):
		w.WriteHeader(http.StatusConflict)
	default:
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func (s *Server) handleScheduleToggle(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	for i := range s.cfg.Schedules {
		if s.cfg.Schedules[i].ID == id {
			s.cfg.Schedules[i].Enabled = !s.cfg.Schedules[i].Enabled
			break
		}
	}
	s.saveAsync()
	s.render(w, "schedule_list", s.buildSchedulePage(r))
}

func (s *Server) handleScheduleDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	kept := s.cfg.Schedules[:0]
	for _, sc := range s.cfg.Schedules {
		if sc.ID != id {
			kept = append(kept, sc)
		}
	}
	s.cfg.Schedules = kept
	s.saveAsync()
	s.render(w, "schedule_list", s.buildSchedulePage(r))
}

// saveAsync persists the current config in a background goroutine so the HTTP
// handler can respond immediately, avoiding SD-card write latency on the Pi.
// A mutex ensures at most one write is in flight at a time.
func (s *Server) saveAsync() {
	go func() {
		s.saveMu.Lock()
		defer s.saveMu.Unlock()
		if err := s.cfg.Save(s.dataDir); err != nil {
			logger.Errorf(logTag, "async save: %v", err)
		}
	}()
}

// ── Backup / Restore ─────────────────────────────────────────────────────────

func (s *Server) handleExportConfig(w http.ResponseWriter, r *http.Request) {
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		http.Error(w, "marshal failed", http.StatusInternalServerError)
		return
	}
	filename := "sprinqua-config-" + time.Now().Format("2006-01-02") + ".json"
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Write(data)
}

func (s *Server) handleImportConfig(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil { // 1 MB limit
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("config_file")
	if err != nil {
		http.Error(w, "missing file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	var imported config.Config
	if err := json.NewDecoder(file).Decode(&imported); err != nil {
		http.Redirect(w, r, "/setup?import_err=invalid", http.StatusFound)
		return
	}
	if !imported.SetupDone {
		http.Redirect(w, r, "/setup?import_err=incomplete", http.StatusFound)
		return
	}

	*s.cfg = imported

	if err := s.cfg.Save(s.dataDir); err != nil {
		logger.Errorf(logTag, "import save: %v", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}

	// Re-initialize board and engine with imported zones.
	if b := board.Find(s.cfg.Board); b != nil {
		s.board = b
		eng := zone.New(s.chMgr, b, s.cfg.Zones, s.cfg.IsExclusiveMode())
		eng.Init()
		eng.SetHistory(s.hist)
		s.engine = eng
		if s.sched != nil {
			s.sched.SetEngine(eng)
		}
		s.mqttClient.Connect(s.cfg.MQTT, s.cfg.Zones, eng, s.hist)
	}
	s.sched.SetPaused(s.cfg.MQTT.IsPassive() || s.cfg.WinterMode)

	http.Redirect(w, r, "/setup", http.StatusFound)
}

// ── History ───────────────────────────────────────────────────────────────────

type historyEntryView struct {
	history.Entry
	Color     string
	TrigLabel string
	StartStr  string
	EndStr    string
	DurStr    string
	Active    bool
}

type historyChartBar struct {
	LeftPct  string
	WidthPct string
	Color    string
	Title    string // tooltip
}

type historyChartRow struct {
	ZoneName string
	Channel  int
	Color    string
	Bars     []historyChartBar
}

type historyStats struct {
	DurStr  string // "2h 35min" | "45min" | "—"
	Runs    int
	Skipped int
	TopZone string // zone with most runs, empty if 0
}

type historyPageData struct {
	basePage
	Stats         historyStats
	Entries       []historyEntryView
	ChartRows     []historyChartRow
	ChartRuler    [5]string
	ChartRangeStr string
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	pg := s.page(r)
	use12h := s.cfg.TimeFormat == "12h"

	zoneIdx := make(map[int]int, len(s.cfg.Zones))
	for i, z := range s.cfg.Zones {
		zoneIdx[z.ID] = i
	}

	// ── Entry list ────────────────────────────────────────────────────────────
	var allEntries []history.Entry
	if s.hist != nil {
		allEntries = s.hist.Recent(0)
	}

	timeFmt := "15:04"
	if use12h {
		timeFmt = "3:04 PM"
	}

	var views []historyEntryView
	for _, e := range allEntries {
		idx := zoneIdx[e.ZoneID]
		color := zoneColors[idx%len(zoneColors)]

		var trigLabel string
		if e.Skipped {
			switch e.Trigger {
			case history.SkipFrost:
				trigLabel = pg.S["hist_trigger_skipped_frost"]
			case history.SkipRainDelay:
				trigLabel = pg.S["hist_trigger_skipped_rain_delay"]
			case history.SkipRain:
				trigLabel = pg.S["hist_trigger_skipped_sw"]
			default:
				trigLabel = pg.S["hist_trigger_skipped_sw"]
			}
		} else {
			switch e.Trigger {
			case history.Manual:
				trigLabel = pg.S["hist_trigger_manual"]
			case history.Schedule:
				trigLabel = pg.S["hist_trigger_schedule"]
			case history.Pulse:
				trigLabel = pg.S["hist_trigger_pulse"]
			case history.MQTT:
				trigLabel = pg.S["hist_trigger_mqtt"]
			}
		}

		startStr := e.StartedAt.Format("02 Jan · " + timeFmt)
		endStr := pg.S["hist_active"]
		durStr := ""
		active := e.EndedAt == nil
		if !active {
			endStr = e.EndedAt.Format(timeFmt)
			m := e.DurSecs / 60
			sec := e.DurSecs % 60
			if m > 0 {
				durStr = fmt.Sprintf("%dm %ds", m, sec)
			} else {
				durStr = fmt.Sprintf("%ds", sec)
			}
		}

		views = append(views, historyEntryView{
			Entry:     e,
			Color:     color,
			TrigLabel: trigLabel,
			StartStr:  startStr,
			EndStr:    endStr,
			DurStr:    durStr,
			Active:    active,
		})
	}

	// ── 24h timeline chart — adaptive zoom ───────────────────────────────────
	now := time.Now()
	windowStart := now.Add(-24 * time.Hour)

	// Pass 1: find the actual span of activations in the last 24h.
	var rMin, rMax time.Time
	hasData := false
	for _, e := range allEntries {
		if e.Skipped {
			continue
		}
		eEnd := now
		if e.EndedAt != nil {
			eEnd = *e.EndedAt
		}
		if eEnd.Before(windowStart) || e.StartedAt.After(now) {
			continue
		}
		if !hasData || e.StartedAt.Before(rMin) {
			rMin = e.StartedAt
		}
		if !hasData || eEnd.After(rMax) {
			rMax = eEnd
		}
		hasData = true
	}

	// Derive adaptive window: pad around actual span, clamp to 24h, enforce 2h minimum.
	adaptStart, adaptEnd := windowStart, now
	if hasData {
		pad := rMax.Sub(rMin) / 5
		if pad < 30*time.Minute {
			pad = 30 * time.Minute
		}
		adaptStart = rMin.Add(-pad)
		adaptEnd = rMax.Add(pad)
		if adaptStart.Before(windowStart) {
			adaptStart = windowStart
		}
		if adaptEnd.After(now) {
			adaptEnd = now
		}
		const minSpan = 2 * time.Hour
		if adaptEnd.Sub(adaptStart) < minSpan {
			mid := adaptStart.Add(adaptEnd.Sub(adaptStart) / 2)
			ns := mid.Add(-minSpan / 2)
			ne := mid.Add(minSpan / 2)
			if ns.Before(windowStart) {
				ns = windowStart
				ne = ns.Add(minSpan)
			}
			if ne.After(now) {
				ne = now
				ns = ne.Add(-minSpan)
				if ns.Before(windowStart) {
					ns = windowStart
				}
			}
			adaptStart, adaptEnd = ns, ne
		}
	}
	windowSecs := adaptEnd.Sub(adaptStart).Seconds()

	// Ruler: 5 marks evenly spaced across the adaptive window.
	var ruler [5]string
	for i := range ruler {
		t := adaptStart.Add(time.Duration(float64(adaptEnd.Sub(adaptStart)) * float64(i) / 4.0))
		if use12h {
			ruler[i] = t.Format("3:04PM")
		} else {
			ruler[i] = t.Format("15:04")
		}
	}

	// Chart range label for the footer.
	spanDur := adaptEnd.Sub(adaptStart)
	spanH := int(spanDur.Hours())
	spanM := int(spanDur.Minutes()) % 60
	var chartRangeStr string
	switch {
	case spanH > 0 && spanM > 0:
		chartRangeStr = fmt.Sprintf("%dh %dmin", spanH, spanM)
	case spanH > 0:
		chartRangeStr = fmt.Sprintf("%dh", spanH)
	default:
		chartRangeStr = fmt.Sprintf("%dmin", spanM)
	}

	// Pass 2: build bars clipped to the adaptive window.
	chartRows := make([]historyChartRow, 0, len(s.cfg.Zones))
	for _, z := range s.cfg.Zones {
		if !z.Enabled {
			continue
		}
		idx := zoneIdx[z.ID]
		color := zoneColors[idx%len(zoneColors)]
		var bars []historyChartBar
		for _, e := range allEntries {
			if e.ZoneID != z.ID || e.Skipped {
				continue
			}
			eEnd := now
			if e.EndedAt != nil {
				eEnd = *e.EndedAt
			}
			if eEnd.Before(adaptStart) || e.StartedAt.After(adaptEnd) {
				continue
			}
			cStart := e.StartedAt
			if cStart.Before(adaptStart) {
				cStart = adaptStart
			}
			cEnd := eEnd
			if cEnd.After(adaptEnd) {
				cEnd = adaptEnd
			}
			leftPct := cStart.Sub(adaptStart).Seconds() / windowSecs * 100
			widthPct := cEnd.Sub(cStart).Seconds() / windowSecs * 100
			if widthPct < 0.5 {
				widthPct = 0.5
			}
			var ttDur string
			if e.EndedAt != nil {
				dm := e.DurSecs / 60
				sec := e.DurSecs % 60
				if dm > 0 {
					ttDur = fmt.Sprintf("%dm%ds", dm, sec)
				} else {
					ttDur = fmt.Sprintf("%ds", sec)
				}
			} else {
				ttDur = pg.S["hist_active"]
			}
			bars = append(bars, historyChartBar{
				LeftPct:  fmt.Sprintf("%.3f", leftPct),
				WidthPct: fmt.Sprintf("%.3f", widthPct),
				Color:    color,
				Title:    fmt.Sprintf("%s · %s · %s", z.Name, e.StartedAt.Format(timeFmt), ttDur),
			})
		}
		chartRows = append(chartRows, historyChartRow{
			ZoneName: z.Name,
			Channel:  z.Channel,
			Color:    color,
			Bars:     bars,
		})
	}

	// ── Stats (last 7 days) ───────────────────────────────────────────────────
	week := now.Add(-7 * 24 * time.Hour)
	var totalSecs, runs, skipped int
	zoneCounts := make(map[int]int)
	for _, e := range allEntries {
		if e.StartedAt.Before(week) {
			continue
		}
		if e.Skipped {
			skipped++
		} else {
			runs++
			totalSecs += e.DurSecs
			zoneCounts[e.ZoneID]++
		}
	}
	var durStr string
	if totalSecs == 0 {
		durStr = "—"
	} else {
		h := totalSecs / 3600
		m := (totalSecs % 3600) / 60
		if h > 0 {
			durStr = fmt.Sprintf("%dh %dmin", h, m)
		} else {
			durStr = fmt.Sprintf("%dmin", m)
		}
	}
	var topZone string
	var topCount int
	zmap := s.cfg.ZoneMap()
	for zid, cnt := range zoneCounts {
		if cnt > topCount {
			topCount = cnt
			topZone = zmap[zid].Name
		}
	}
	if len(zoneCounts) <= 1 {
		topZone = ""
	}

	s.render(w, "history", historyPageData{
		basePage:      pg,
		Stats:         historyStats{DurStr: durStr, Runs: runs, Skipped: skipped, TopZone: topZone},
		Entries:       views,
		ChartRows:     chartRows,
		ChartRuler:    ruler,
		ChartRangeStr: chartRangeStr,
	})
}

func (s *Server) parseScheduleForm(r *http.Request) config.Schedule {
	if err := r.ParseForm(); err != nil {
		return config.Schedule{}
	}
	zoneIDs := r.Form["zone_id"]
	durs := r.Form["dur_mins"]
	soaks := r.Form["soak_mins"]
	var zones []config.ProgramZone
	for i, zs := range zoneIDs {
		zoneID, err := strconv.Atoi(zs)
		if err != nil || zoneID <= 0 {
			continue
		}
		dur := 0
		if i < len(durs) {
			dur, _ = strconv.Atoi(durs[i])
		}
		if dur <= 0 {
			dur = 10
		}
		soak := 0
		if i < len(soaks) {
			soak, _ = strconv.Atoi(soaks[i])
		}
		if soak < 0 {
			soak = 0
		}
		if soak > 120 {
			soak = 120
		}
		zones = append(zones, config.ProgramZone{ZoneID: zoneID, DurMins: dur, SoakAfterMins: soak})
	}
	var days []int
	for _, d := range r.Form["days"] {
		if n, err := strconv.Atoi(d); err == nil && n >= 0 && n <= 6 {
			days = append(days, n)
		}
	}
	startTime := r.FormValue("start_time")
	if startTime == "" {
		startTime = "08:00"
	}
	return config.Schedule{
		Name:          r.FormValue("name"),
		Zones:         zones,
		Days:          days,
		StartTime:     startTime,
		Enabled:       r.FormValue("enabled") == "1",
		SmartWatering: r.FormValue("smart_watering") == "1",
	}
}

// handleWeatherStatus returns an HTMX fragment showing today's rain forecast
// and whether irrigation would be allowed or skipped.
func (s *Server) handleWeatherStatus(w http.ResponseWriter, r *http.Request) {
	bp := s.page(r)
	str := bp.S
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	sw := s.cfg.SmartWatering
	if !sw.Enabled || (sw.Lat == 0 && sw.Lon == 0) {
		fmt.Fprintf(w, `<div id="sw-status" class="rounded-xl border border-slate-200 bg-slate-50 px-4 py-3 flex items-center justify-between">
  <p class="text-xs text-slate-400">%s</p>
  <button hx-get="/api/weather" hx-swap="outerHTML" hx-target="#sw-status"
    class="text-slate-400 hover:text-slate-600 text-base px-1 flex-shrink-0">↻</button>
</div>`, str["sw_status_no_loc"])
		return
	}

	var rainDelayLine, clearBtn string
	if sw.SkipEnabled && sw.RainDelayDays > 0 {
		if delay, err := weather.RainDelayStatus(sw, time.Now()); err == nil && delay.Active {
			rainDelayLine = fmt.Sprintf(`<p class="text-xs text-amber-700 font-medium mt-1">%s</p>`,
				fmt.Sprintf(str["sw_status_rain_delay"], delay.DaysLeft))
			clearBtn = fmt.Sprintf(`<button hx-post="/api/rain-delay/clear" hx-swap="outerHTML" hx-target="#sw-status"
    class="text-xs font-semibold text-amber-700 hover:text-amber-900 underline flex-shrink-0">%s</button>`,
				str["sw_rain_delay_clear"])
		}
	}

	res, err := weather.FetchToday(sw.Lat, sw.Lon)
	if err != nil {
		logger.Warnf(logTag, "weather status: %v", err)
		fmt.Fprintf(w, `<div id="sw-status" class="rounded-xl border border-red-100 bg-red-50 px-4 py-3 flex items-center justify-between">
  <p class="text-xs text-red-500">%s</p>
  <button hx-get="/api/weather" hx-swap="outerHTML" hx-target="#sw-status"
    class="text-red-400 hover:text-red-600 text-base px-1 flex-shrink-0">↻</button>
</div>`, str["sw_status_error"])
		return
	}

	threshold := sw.EffectiveThreshold()
	allowed := res.RainMM < threshold

	todayLine := fmt.Sprintf(str["sw_status_today"], res.TempMinC, res.TempMaxC, res.HumidityPct, res.WindKmh, res.RainMM)
	var statusLine, icon, borderCls, textCls string
	if allowed {
		icon = "☀️"
		borderCls = "border-emerald-200 bg-emerald-50"
		textCls = "text-emerald-700"
		statusLine = fmt.Sprintf(str["sw_status_ok"], threshold)
	} else {
		icon = "🌧️"
		borderCls = "border-amber-200 bg-amber-50"
		textCls = "text-amber-700"
		statusLine = fmt.Sprintf(str["sw_status_skip"], threshold)
	}
	if rainDelayLine != "" {
		borderCls = "border-amber-200 bg-amber-50"
		icon = "🌧️"
	}

	fmt.Fprintf(w, `<div id="sw-status" class="rounded-xl border %s px-4 py-3 flex items-center gap-3">
  <span class="text-2xl flex-shrink-0">%s</span>
  <div class="flex-1 min-w-0">
    <p class="text-sm font-semibold text-slate-800">%s</p>
    <p class="text-xs %s font-medium mt-0.5">%s</p>
    %s
  </div>
  %s
  <button hx-get="/api/weather" hx-swap="outerHTML" hx-target="#sw-status"
    class="text-slate-400 hover:text-slate-600 text-lg flex-shrink-0 px-1">↻</button>
</div>`, borderCls, icon, todayLine, textCls, statusLine, rainDelayLine, clearBtn)
}

// handleRainDelayClear dismisses the rain event currently driving the delay.
// Any rain that falls after that event still triggers a fresh delay.
func (s *Server) handleRainDelayClear(w http.ResponseWriter, r *http.Request) {
	sw := s.cfg.SmartWatering
	delay, err := weather.RainDelayStatus(sw, time.Now())
	if err != nil || !delay.Active {
		s.handleWeatherStatus(w, r)
		return
	}

	s.cfg.SmartWatering.RainDelayClearedRainDate = delay.RainDate.Format("2006-01-02")
	if err := s.cfg.Save(s.dataDir); err != nil {
		logger.Errorf(logTag, "clear rain delay: %v", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	s.handleWeatherStatus(w, r)
}
