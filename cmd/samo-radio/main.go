// Command samo-radio is a headless Samo playback device.
//
// It holds the sound card open and plays whatever Samo tells it to — a
// programmed channel by default, or an ad-hoc queue somebody sent from a phone.
// Think of it as a Chromecast that only speaks Samo and outputs to the box's
// own line-out.
//
// The box can be the one running samo-server, or any other Linux machine on the
// network: a Pi with speakers in the kitchen is the same daemon with the same
// config, reached over the LAN instead of over loopback.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/bouliehaan/samo-radio/internal/config"
	"github.com/bouliehaan/samo-radio/internal/httpapi"
	"github.com/bouliehaan/samo-radio/internal/player"
	"github.com/bouliehaan/samo-radio/internal/sink"
)

func main() {
	configPath := flag.String("config", envOr("SAMO_RADIO_CONFIG", config.DefaultPath), "path to config.json")
	listDevices := flag.Bool("devices", false, "list audio output devices and exit")
	showPairing := flag.Bool("pairing", false, "print what to type into Samo to add this device, then exit")
	fixMixer := flag.Bool("unmute", false, "unmute and raise silenced mixer controls, then exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmsgprefix)
	logger.SetPrefix("samo-radio: ")

	if *showVersion {
		fmt.Println("samo-radio " + httpapi.Version)
		return
	}

	store, err := config.Load(*configPath)
	if err != nil {
		logger.Fatalf("config: %v", err)
	}

	// `--devices` is the one thing an operator genuinely needs a shell for:
	// finding out what the card is called before anything is configured.
	// Everything after that is doable from the Samo UI.
	if *listDevices {
		if err := printDevices(store); err != nil {
			logger.Fatalf("list devices: %v", err)
		}
		return
	}

	// `--pairing` answers the only question a fresh box raises: what do I type
	// into Samo? On a device that is not the server, the address is not
	// guessable and the token is not memorable, so both are printed here rather
	// than left to be dug out of a JSON file over SSH.
	if *showPairing {
		if err := printPairing(store); err != nil {
			logger.Fatalf("pairing: %v", err)
		}
		return
	}

	// A muted card is the single most common reason a fresh install plays
	// "successfully" into silence, so the installer runs this. It only touches
	// controls that are muted or at zero — a level somebody set is a decision.
	if *fixMixer {
		if err := fixMixerLevels(store, logger); err != nil {
			logger.Fatalf("mixer: %v", err)
		}
		return
	}

	// Before anything is served. The control token is the only thing standing
	// between the speakers and the rest of the network, so a device that came
	// up without one gets one now rather than serving unprotected until an
	// operator notices.
	token, minted, err := store.EnsureControlToken()
	if err != nil {
		logger.Fatalf("control token: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	engine := player.New(store, logger)
	if err := engine.Start(ctx); err != nil {
		logger.Fatalf("start playback: %v", err)
	}
	defer func() { _ = engine.Close() }()

	snapshot := store.Snapshot()
	if minted {
		// Logged on the boot that created it and never again: after this the
		// secret stays out of the journal and `--pairing` is how to read it
		// back. Said even on a paired device, because a token minted there is a
		// token Samo does not have, and every command will be refused until it
		// is pasted in again.
		logger.Printf("control token: %s", token)
	}
	if !snapshot.Paired() {
		// Everything needed to finish the setup, in the journal, on the box
		// that has the problem. A device across the house is otherwise a
		// guessing game of which address it landed on.
		for _, endpoint := range httpapi.Endpoints(snapshot.ListenAddr) {
			logger.Printf("not paired yet — add this device in Samo (RADIO → SAMO-RADIO) at %s", endpoint)
		}
		if !minted {
			logger.Printf("run `samo-radio --pairing` to print the control token")
		}
	}

	handler := httpapi.New(engine, store, logger)
	if err := httpapi.Serve(ctx, snapshot.ListenAddr, handler, logger); err != nil {
		logger.Fatalf("control api: %v", err)
	}
	logger.Printf("shutting down")
}

// fixMixerLevels makes the card audible without stamping on existing settings.
func fixMixerLevels(store *config.Store, logger *log.Logger) error {
	snapshot := store.Snapshot()
	card := sink.CardFromDevice(snapshot.Output.Device)
	report, err := sink.EnsureAudible(context.Background(), card)
	if err != nil {
		return err
	}
	for _, control := range report.LeftAlone {
		// Reported, not fixed: another output is live, so this mute is how the
		// machine is wired rather than a broken default. Say so, because if it
		// IS the socket the cable is in, the operator needs to know.
		logger.Printf("mixer card %s: %s is %s — left alone, another output is live",
			card, control.Name, describeLeftAlone(control))
	}
	if len(report.Changed) == 0 {
		logger.Printf("mixer on card %s needed nothing", card)
		return nil
	}
	for _, change := range report.Changed {
		logger.Printf("mixer card %s: %s %s -> %s", card, change.Control, change.Was, change.Now)
	}
	if err := sink.PersistMixer(context.Background()); err != nil {
		logger.Printf("could not persist mixer state (a reboot may come back silent): %v", err)
	}
	return nil
}

func describeLeftAlone(control sink.MixerControl) string {
	if control.Muted {
		return "muted"
	}
	return "at 0%"
}

// printPairing prints the three things Samo's ADD DEVICE form asks for.
func printPairing(store *config.Store) error {
	// Minting here as well as at startup means this works on a box where the
	// service has never run — the installer calls it before the first start.
	token, _, err := store.EnsureControlToken()
	if err != nil {
		return err
	}
	snapshot := store.Snapshot()

	fmt.Printf("device name  : %s\n", snapshot.DeviceName)
	for index, endpoint := range httpapi.Endpoints(snapshot.ListenAddr) {
		label := "control url  :"
		if index > 0 {
			label = "             :"
		}
		fmt.Printf("%s %s\n", label, endpoint)
	}
	fmt.Printf("control token: %s\n", token)

	if snapshot.Paired() {
		fmt.Printf("\nalready paired with %s\n", snapshot.Server.BaseURL)
		return nil
	}
	fmt.Printf("\nIn Samo: RADIO → SAMO-RADIO → + ADD DEVICE, then paste the URL and token above.\n")
	if snapshot.LoopbackOnly() {
		// A deliberate choice, but worth saying out loud: from any other
		// machine this device does not answer at all.
		fmt.Printf("This device listens on %s, so only Samo running on this same machine can reach it.\n",
			snapshot.ListenAddr)
	}
	return nil
}

func printDevices(store *config.Store) error {
	snapshot := store.Snapshot()
	backend, err := sink.ParseBackend(snapshot.Output.Backend)
	if err != nil {
		return err
	}
	devices, resolved, err := sink.ListDevices(context.Background(), backend)
	if err != nil {
		return err
	}
	fmt.Printf("backend: %s\n\n", resolved)
	for _, device := range devices {
		marker := " "
		if device.Recommended {
			marker = "*"
		}
		fmt.Printf("%s %-40s %s\n", marker, device.ID, device.Name)
		if device.Detail != "" {
			fmt.Printf("  %-40s %s\n", "", device.Detail)
		}
	}
	fmt.Printf("\n* = recommended (handles format conversion; try these first)\n")
	return nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
