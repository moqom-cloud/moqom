// Command moqom-mute force-mutes a participant through the admin API.
//
// The third of the by-hand tools, after moqom-mint and moqom-ban, and it exists
// for the same reason: the sample application has no backend to issue the call
// from, and this path is worth being able to run against a deployed pair of
// tiers without one.
//
//	MOQOM_MUTE_TARGET=127.0.0.1:8443 \
//	MOQOM_MUTE_CREDENTIAL=$(gcloud secrets versions access latest --secret=moqom-dev-admin-credential) \
//	moqom-mute -user publisher -room bench -media video
//
//	moqom-mute -user publisher -room bench -media video -lift
//
// # What this is really testing
//
// A force-mute travels further than anything else in the admin API. The control
// plane writes it, pushes an ApplyMute over RelayLink, the relay tears down the
// upstream subscription for the muted track, and then tells the participant's
// own session on a `.policy` track it publishes at them — at which point the
// phone closes its camera and the indicator goes out. Five components, three
// languages, and the visible result is a light on somebody's phone.
//
// So what to watch is not this program's output. It is the device: run it with
// the phone in front of you and the green dot should go out within a second of
// the call returning. `enforced=true` says the relays confirmed; the light says
// the client did.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	moqom "github.com/moqom-cloud/moqom-go"

	"github.com/moqom-cloud/moqom/go/internal/admintls"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("moqom-mute: ")
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	user := flag.String("user", "", "the account to mute")
	device := flag.String("device", "", "one device, or empty for every device that account has here")
	room := flag.String("room", "", "which room")
	media := flag.String("media", "", "audio, video, screen, or a comma-separated list; empty means all of it")
	// Everyone by default, because that is the one people mean when they say
	// mute. Audience-only is a narrowing of who receives the media and
	// deliberately does not stop the camera -- the other hosts are still
	// watching, and closing the device would take the participant off their
	// screens too.
	audience := flag.Bool("audience-only", false, "hide from viewers while hosts still receive it")
	minutes := flag.Int("minutes", 0, "lift automatically after this long; zero holds until lifted")
	lift := flag.Bool("lift", false, "lift the mute instead of placing it")
	reason := flag.String("reason", "verifying enforcement", "recorded on the audit entry")
	behalf := flag.String("on-behalf-of", "", "the moderator to attribute this to; they must hold moderation in the room")
	flag.Parse()

	if *user == "" || *room == "" {
		return errors.New("-user and -room are both required")
	}
	kinds, err := kinds(*media)
	if err != nil {
		return err
	}

	target := os.Getenv("MOQOM_MUTE_TARGET")
	if target == "" {
		return errors.New("MOQOM_MUTE_TARGET is required, e.g. 127.0.0.1:8443")
	}
	credential := os.Getenv("MOQOM_MUTE_CREDENTIAL")
	if credential == "" {
		return errors.New("MOQOM_MUTE_CREDENTIAL is required: a service account credential")
	}
	tenant := value("MOQOM_MUTE_TENANT", "moqom")

	options := []moqom.Option{
		moqom.WithCredential(credential),
		moqom.WithTenant(tenant),
	}
	transport, err := admintls.Credentials()
	if err != nil {
		return err
	}
	if transport != nil {
		options = append(options, moqom.WithTLS(transport))
	} else {
		options = append(options, moqom.WithoutTransportSecurity())
	}

	client, err := moqom.Dial(target, options...)
	if err != nil {
		return fmt.Errorf("dialling %s: %w", target, err)
	}
	defer client.Close()

	ctx, done := context.WithTimeout(context.Background(), 20*time.Second)
	defer done()

	who := moqom.ParticipantRef{Username: *user, Device: *device}
	scope := moqom.MuteForEveryone
	if *audience {
		scope = moqom.MuteForAudience
	}
	request := moqom.MuteRequest{
		Target:   who,
		Media:    kinds,
		Scope:    scope,
		Duration: time.Duration(*minutes) * time.Minute,
		Actor:    moqom.Actor{OnBehalfOf: *behalf, Reason: *reason},
	}
	handle := client.Room(*room)

	if *lift {
		// The scope has to match the mute being lifted, and the SDK will not
		// guess: somebody can be under both at once, and lifting the wrong one
		// succeeds while changing nothing anybody can hear.
		entry, err := handle.Unmute(ctx, request)
		if err != nil {
			return fmt.Errorf("unmuting %s: %w", *user, err)
		}
		log.Printf("lifted: %s may publish %s in %s again (entry %s, enforced=%t)",
			*user, describe(kinds), *room, entry.ID, entry.Enforced)
		log.Print("their camera stays off until they turn it back on, which is the client's to do")
		return nil
	}

	entry, err := handle.ForceMute(ctx, request)
	if err != nil {
		return fmt.Errorf("muting %s: %w", *user, err)
	}
	log.Printf("muted: %s stops publishing %s in %s (entry %s, enforced=%t)",
		*user, describe(kinds), *room, entry.ID, entry.Enforced)
	return nil
}

// kinds parses the -media flag. Empty is every kind, which is what a moderator
// dealing with an incident means by "mute them".
func kinds(list string) ([]moqom.MediaKind, error) {
	if strings.TrimSpace(list) == "" {
		return nil, nil
	}
	var parsed []moqom.MediaKind
	for _, name := range strings.Split(list, ",") {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "audio", "mic", "microphone":
			parsed = append(parsed, moqom.MediaAudio)
		case "video", "camera":
			parsed = append(parsed, moqom.MediaVideo)
		case "screen", "screen_share", "screenshare":
			parsed = append(parsed, moqom.MediaScreenShare)
		default:
			return nil, fmt.Errorf("unknown media kind %q: want audio, video or screen", name)
		}
	}
	return parsed, nil
}

func describe(kinds []moqom.MediaKind) string {
	if len(kinds) == 0 {
		return "anything"
	}
	names := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		names = append(names, string(kind))
	}
	return strings.Join(names, " and ")
}

func value(name, fallback string) string {
	if set := os.Getenv(name); set != "" {
		return set
	}
	return fallback
}
