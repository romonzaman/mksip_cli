//go:build cgo

package audio

import (
	"fmt"
	"os"
	"strings"
	"unsafe"

	"github.com/gen2brain/malgo"

	"sipclient/internal/aec"
)

// malgoDevice drives the microphone and speakers through miniaudio.
//
// Capture and playback are opened as two independent devices rather than one
// duplex device: on CoreAudio a duplex device spanning two different physical
// endpoints requires an aggregate device, so separate devices are what actually
// works when the mic and speakers are not the same hardware.
type malgoDevice struct {
	ctx      *malgo.AllocatedContext
	capture  *malgo.Device
	playback *malgo.Device
	// duplex is set instead of capture+playback when echo cancellation is on.
	duplex *malgo.Device
	desc   string
	// echoCancelling records whether duplex was actually achieved, since it
	// silently falls back rather than failing the call.
	echoCancelling bool

	// known is the device list read when this device was opened. Enumerating
	// again while a stream is running can block inside the backend -- and a
	// `devices` command must never be able to hang a call -- so the list is
	// captured once and served from here.
	known []DeviceInfo
}

// EchoCancelling reports whether the device is cancelling acoustic echo.
func (d *malgoDevice) EchoCancelling() bool { return d.echoCancelling }

// Open initialises the audio backend and starts both streams.
func Open(cfg Config) (Device, error) {
	if cfg.Router == nil {
		return nil, fmt.Errorf("audio: router is required")
	}

	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, fmt.Errorf("audio: init backend: %w", err)
	}
	d := &malgoDevice{ctx: ctx}

	captures, err := ctx.Devices(malgo.Capture)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("audio: enumerate capture devices: %w", err)
	}
	playbacks, err := ctx.Devices(malgo.Playback)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("audio: enumerate playback devices: %w", err)
	}

	d.known = toDeviceInfo(captures, true)
	d.known = append(d.known, toDeviceInfo(playbacks, false)...)

	name := func(i malgo.DeviceInfo) string { return i.Name() }
	capIdx, err := matchDevice(cfg.InputDevice, captures, name, "input")
	if err != nil {
		d.Close()
		return nil, err
	}
	playIdx, err := matchDevice(cfg.OutputDevice, playbacks, name, "output")
	if err != nil {
		d.Close()
		return nil, err
	}

	// Ask the backend for 8 kHz mono directly; miniaudio resamples from the
	// hardware rate internally, which is what FR-8.6 relies on.
	newConfig := func(kind malgo.DeviceType) malgo.DeviceConfig {
		dc := malgo.DefaultDeviceConfig(kind)
		dc.SampleRate = uint32(cfg.SampleRate)
		dc.PeriodSizeInFrames = uint32(cfg.FrameSamples)
		dc.Periods = 3
		dc.Capture.Format = malgo.FormatS16
		dc.Capture.Channels = uint32(cfg.Channels)
		dc.Playback.Format = malgo.FormatS16
		dc.Playback.Channels = uint32(cfg.Channels)
		return dc
	}

	// Echo cancellation needs one duplex device, so the microphone frame and
	// the speaker frame that accompanies it arrive together. Two independent
	// devices have independent clocks, and aligning them is the hard part of
	// AEC -- worth avoiding entirely where the backend will do duplex.
	if cfg.EchoCancel {
		if err := d.openDuplex(cfg, captures, playbacks, capIdx, playIdx, newConfig); err == nil {
			return d, nil
		} else {
			// Falling back keeps the phone working; it just cannot cancel echo,
			// which matters enough to say plainly.
			fmt.Fprintf(os.Stderr,
				"audio: echo cancellation unavailable (%v); "+
					"using separate devices, so avoid the speakerphone\n", err)
		}
	}

	capCfg := newConfig(malgo.Capture)
	if capIdx >= 0 {
		capCfg.Capture.DeviceID = deviceIDPtr(&captures[capIdx])
	}
	d.capture, err = malgo.InitDevice(ctx.Context, capCfg, malgo.DeviceCallbacks{
		Data: func(_, in []byte, _ uint32) { cfg.Router.ProcessCapture(in) },
	})
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("audio: open input device: %w", err)
	}

	playCfg := newConfig(malgo.Playback)
	if playIdx >= 0 {
		playCfg.Playback.DeviceID = deviceIDPtr(&playbacks[playIdx])
	}
	d.playback, err = malgo.InitDevice(ctx.Context, playCfg, malgo.DeviceCallbacks{
		Data: func(out, _ []byte, _ uint32) { cfg.Router.ProcessPlayback(out) },
	})
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("audio: open output device: %w", err)
	}

	d.desc = fmt.Sprintf("in=%q out=%q @%dHz mono",
		pickName(captures, capIdx), pickName(playbacks, playIdx), cfg.SampleRate)
	return d, nil
}

// openDuplex opens a single device doing capture and playback together.
func (d *malgoDevice) openDuplex(cfg Config, captures, playbacks []malgo.DeviceInfo,
	capIdx, playIdx int, newConfig func(malgo.DeviceType) malgo.DeviceConfig) error {

	dc := newConfig(malgo.Duplex)
	if capIdx >= 0 {
		dc.Capture.DeviceID = deviceIDPtr(&captures[capIdx])
	}
	if playIdx >= 0 {
		dc.Playback.DeviceID = deviceIDPtr(&playbacks[playIdx])
	}

	dev, err := malgo.InitDevice(d.ctx.Context, dc, malgo.DeviceCallbacks{
		Data: func(out, in []byte, _ uint32) { cfg.Router.ProcessDuplex(out, in) },
	})
	if err != nil {
		return err
	}

	canceller := aec.New(aec.Config{
		SampleRate:   cfg.SampleRate,
		FrameSamples: cfg.FrameSamples,
		TailMS:       cfg.EchoTailMS,
	})
	cfg.Router.SetEchoCanceller(canceller)

	d.duplex = dev
	d.echoCancelling = true
	d.desc = fmt.Sprintf("duplex in=%q out=%q @%dHz mono, echo cancellation on (%dms tail)",
		pickName(captures, capIdx), pickName(playbacks, playIdx),
		cfg.SampleRate, canceller.TailSamples()*1000/cfg.SampleRate)
	return nil
}

// deviceIDPtr hands the backend a device's identifier.
//
// The pointer must refer to the caller's slice element, never to a local copy:
// a pointer to a local escapes its scope the moment this function returns, and
// giving that to C is undefined behaviour that shows up as an occasional
// segmentation fault rather than an honest error.
func deviceIDPtr(info *malgo.DeviceInfo) unsafe.Pointer {
	return info.ID.Pointer()
}

func pickName(items []malgo.DeviceInfo, idx int) string {
	if idx >= 0 && idx < len(items) {
		return items[idx].Name()
	}
	for i := range items {
		if items[i].IsDefault != 0 {
			return items[i].Name() + " (default)"
		}
	}
	return "system default"
}

func (d *malgoDevice) Start() error {
	if d.duplex != nil {
		if err := d.duplex.Start(); err != nil {
			return fmt.Errorf("audio: start duplex device: %w", err)
		}
		return nil
	}
	if d.capture != nil {
		if err := d.capture.Start(); err != nil {
			return fmt.Errorf("audio: start input: %w", err)
		}
	}
	if d.playback != nil {
		if err := d.playback.Start(); err != nil {
			return fmt.Errorf("audio: start output: %w", err)
		}
	}
	return nil
}

func (d *malgoDevice) Stop() error {
	var errs []string
	for _, dev := range []*malgo.Device{d.duplex, d.capture, d.playback} {
		if dev == nil || !dev.IsStarted() {
			continue
		}
		if err := dev.Stop(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("audio: stop: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (d *malgoDevice) Close() error {
	_ = d.Stop()
	for _, dev := range []*malgo.Device{d.duplex, d.capture, d.playback} {
		if dev != nil {
			dev.Uninit()
		}
	}
	if d.ctx != nil {
		_ = d.ctx.Uninit()
		d.ctx.Free()
	}
	return nil
}

func (d *malgoDevice) Description() string { return d.desc }

// List returns the devices seen when this one was opened.
//
// It deliberately does not re-enumerate: asking the backend for the device
// list while a stream is running can block, and a `devices` command must never
// be able to hang a call. A list that is slightly stale is a far better
// outcome, and the set of audio devices rarely changes mid-call.
func (d *malgoDevice) List() ([]DeviceInfo, error) {
	if len(d.known) == 0 {
		return List()
	}
	out := make([]DeviceInfo, len(d.known))
	copy(out, d.known)
	return out, nil
}

// toDeviceInfo converts the backend's device list.
func toDeviceInfo(items []malgo.DeviceInfo, isCapture bool) []DeviceInfo {
	out := make([]DeviceInfo, 0, len(items))
	for i := range items {
		out = append(out, DeviceInfo{
			Name:      items[i].Name(),
			IsDefault: items[i].IsDefault != 0,
			IsCapture: isCapture,
		})
	}
	return out
}

// enumerate reads the device lists from a context.
func enumerate(ctx malgo.Context) ([]DeviceInfo, error) {
	var out []DeviceInfo
	for _, kind := range []struct {
		t         malgo.DeviceType
		isCapture bool
	}{{malgo.Capture, true}, {malgo.Playback, false}} {
		items, err := ctx.Devices(kind.t)
		if err != nil {
			return nil, err
		}
		out = append(out, toDeviceInfo(items, kind.isCapture)...)
	}
	return out, nil
}

// List enumerates the available endpoints for the `devices` command.
func List() ([]DeviceInfo, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, fmt.Errorf("audio: init backend: %w", err)
	}
	defer func() {
		_ = ctx.Uninit()
		ctx.Free()
	}()

	return enumerate(ctx.Context)
}
