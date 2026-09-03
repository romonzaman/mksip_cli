// Command sipclient is a CLI SIP user agent with two call channels, built for
// exercising PBX call control -- in particular warm (attended) transfer.
//
// See requirements.md for the specification this implements.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sipclient/internal/applog"
	"sipclient/internal/audio"
	"sipclient/internal/channel"
	"sipclient/internal/cli"
	"sipclient/internal/config"
	"sipclient/internal/media"
	"sipclient/internal/sipua"
	"sipclient/internal/transfer"
)

// Exit codes per FR-9.9.
const (
	exitOK           = 0
	exitConfig       = 1
	exitRegistration = 2
	exitScript       = 3
)

var version = "1.0.0"

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "config.json", "path to config.json")
	showVersion := flag.Bool("version", false, "print version and exit")
	noAudio := flag.Bool("no-audio", false,
		"run without opening an audio device (signalling only)")
	waitReg := flag.Duration("wait-register", 10*time.Second,
		"how long to wait for the first registration before giving up")
	flag.Parse()

	if *showVersion {
		fmt.Printf("sipclient %s\n", version)
		return exitOK
	}

	// 1. Configuration (FR-1.x). Any problem here is fatal and specific.
	cfg, insecure, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", *configPath, err)
		return exitConfig
	}

	// 2. Logging, with the console sink wired to the CLI later so log lines
	//    land above the prompt rather than through it.
	var pendingCLI *cli.CLI
	consoleSink := deferredWriter{get: func() (writerTarget, bool) {
		if pendingCLI == nil {
			return nil, false
		}
		return pendingCLI.Writer(), true
	}}

	logger, err := applog.New(applog.Options{
		File:         cfg.Logging.File,
		Level:        cfg.Logging.Level,
		ConsoleLevel: cfg.Logging.ConsoleLevel,
		SIPTrace:     cfg.Logging.SIPTrace,
		SIPTraceFile: cfg.Logging.SIPTraceFile,
		Console:      consoleSink,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "logging: %v\n", err)
		return exitConfig
	}
	defer logger.Close()

	logger.Info("starting", "version", version, "config", *configPath,
		"aor", cfg.AOR(), "server", cfg.ServerAddr())
	if insecure {
		logger.Warn("config file is group- or world-readable and holds a password",
			"path", *configPath, "fix", fmt.Sprintf("chmod 600 %s", *configPath))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 3. Audio device. A device failure is not fatal: signalling still works,
	//    which is what -no-audio and headless runs rely on (NFR-1).
	frameBytes := media.FrameBytes(cfg.Media.PtimeMS)
	router := audio.NewRouter(frameBytes, 8)

	var device audio.Device = audio.NullDevice{}
	if *noAudio {
		logger.Info("audio disabled by -no-audio")
	} else {
		opened, err := audio.Open(audio.Config{
			SampleRate:   media.SampleRate,
			Channels:     1,
			FrameSamples: media.FrameSamples(cfg.Media.PtimeMS),
			InputDevice:  cfg.Audio.InputDevice,
			OutputDevice: cfg.Audio.OutputDevice,
			Router:       router,
		})
		if err != nil {
			logger.Error("audio unavailable, continuing without it", "error", err)
		} else {
			device = opened
			if err := device.Start(); err != nil {
				logger.Error("could not start audio, continuing without it", "error", err)
				_ = device.Close()
				device = audio.NullDevice{}
			}
		}
	}
	defer device.Close()
	logger.Info("audio", "device", device.Description())

	// 4. Channels, SIP stack and transfer logic.
	manager := channel.NewManager(cfg, logger.Logger, router)

	ua, err := sipua.New(cfg, logger.Logger, logger.Tracer(), manager.Handlers())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		logger.Error("SIP setup failed", "error", err)
		return exitConfig
	}
	defer ua.Close()
	manager.SetUA(ua)

	if err := ua.Listen(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		logger.Error("SIP listen failed", "error", err)
		return exitConfig
	}

	// Warn early if peers cannot reach the address we advertise: it is the
	// difference between "inbound calls silently never arrive" and knowing why.
	ua.CheckAdvertisedReachable()

	transferor := transfer.New(cfg, manager, logger.Logger)

	// 5. Registration runs for the whole session (FR-2.2).
	go ua.RunRegistration(ctx)

	// 6. CLI.
	shell := cli.New(cli.Options{
		Config:     cfg,
		UA:         ua,
		Manager:    manager,
		Transferor: transferor,
		Tracer:     logger.Tracer(),
		DeviceDesc: device.Description(),
	})
	pendingCLI = shell

	// Wait briefly for the first registration so a bad password is reported as
	// a startup failure rather than a puzzling status line (FR-2.4, exit 2).
	if !awaitRegistration(ctx, ua, *waitReg) {
		reg := ua.Registration()
		if reg.State == sipua.RegFailed {
			fmt.Fprintf(os.Stderr, "registration failed: %s\n", reg.LastError)
			logger.Error("registration failed at startup", "error", reg.LastError)
			return exitRegistration
		}
		logger.Warn("not registered yet, continuing to retry in the background",
			"waited", waitReg.String())
	}

	runErr := shell.Run(ctx)

	// 7. Graceful shutdown: hang up, de-register, stop (FR-9.7, NFR-5).
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	manager.HangupAll(shutdown)
	if err := ua.Unregister(shutdown); err != nil {
		logger.Warn("de-registration failed", "error", err)
	}
	logger.Info("stopped")

	switch {
	case runErr != nil:
		fmt.Fprintf(os.Stderr, "%v\n", runErr)
		return exitConfig
	case shell.ScriptFailed():
		return exitScript
	}
	return exitOK
}

// awaitRegistration blocks until registration settles or the timeout expires.
func awaitRegistration(ctx context.Context, ua *sipua.UA, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		switch ua.Registration().State {
		case sipua.RegRegistered:
			return true
		case sipua.RegFailed:
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
	return ua.Registered()
}

type writerTarget interface {
	Write(p []byte) (int, error)
}

// deferredWriter lets the logger be built before the CLI exists, then routes
// console records through the CLI once it is ready.
type deferredWriter struct {
	get func() (writerTarget, bool)
}

func (d deferredWriter) Write(p []byte) (int, error) {
	if w, ok := d.get(); ok {
		return w.Write(p)
	}
	return os.Stderr.Write(p)
}
