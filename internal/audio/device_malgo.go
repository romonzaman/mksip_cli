//go:build cgo

package audio

import (
	"fmt"
	"strings"
	"unsafe"

	"github.com/gen2brain/malgo"
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
	desc     string
}

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

func deviceIDPtr(info *malgo.DeviceInfo) unsafe.Pointer {
	id := info.ID
	return id.Pointer()
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
	for _, dev := range []*malgo.Device{d.capture, d.playback} {
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
	for _, dev := range []*malgo.Device{d.capture, d.playback} {
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

	var out []DeviceInfo
	for _, kind := range []struct {
		t         malgo.DeviceType
		isCapture bool
	}{{malgo.Capture, true}, {malgo.Playback, false}} {
		items, err := ctx.Devices(kind.t)
		if err != nil {
			return nil, err
		}
		for i := range items {
			out = append(out, DeviceInfo{
				Name:      items[i].Name(),
				IsDefault: items[i].IsDefault != 0,
				IsCapture: kind.isCapture,
			})
		}
	}
	return out, nil
}
