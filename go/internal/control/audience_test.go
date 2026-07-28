package control_test

import (
	"strconv"
	"testing"

	"github.com/moqom-cloud/moqom/go/internal/control"
)

// viewer is somebody with nothing to publish, which is what makes them audience.
func viewer(username, device, via string) control.Participant {
	return control.Participant{
		Identity:   control.Identity{Username: username, Device: device},
		Presence:   control.PresenceConnected,
		ArrivedVia: via,
	}
}

func cohost(username, device string) control.Participant {
	participant := viewer(username, device, "")
	participant.Grants = control.Grants{
		Publish:    control.PublishCamera,
		Moderation: control.CapForceMute,
	}
	return participant
}

func find(t *testing.T, cohorts []control.Audience, via string) control.Audience {
	t.Helper()
	for _, cohort := range cohorts {
		if cohort.Via == via {
			return cohort
		}
	}
	return control.Audience{Via: via}
}

func TestAudiencesDivideViewersByWhoBroughtThem(t *testing.T) {
	cohorts := control.Audiences([]control.Participant{
		cohost("ana", "d1"),
		cohost("bea", "d1"),
		viewer("v1", "d1", "bea"),
		viewer("v2", "d1", "bea"),
		viewer("v3", "d1", "ana"),
		viewer("v4", "d1", ""),
	})

	if got := find(t, cohorts, "bea").Viewers; got != 2 {
		t.Errorf("bea brought 2 viewers, got %d", got)
	}
	if got := find(t, cohorts, "ana").Viewers; got != 1 {
		t.Errorf("ana brought 1 viewer, got %d", got)
	}
	// The organic cohort, which is a cohort and not a remainder.
	if got := find(t, cohorts, "").Viewers; got != 1 {
		t.Errorf("1 viewer arrived alone, got %d", got)
	}
}

func TestAudiencesCountPeopleNotDevices(t *testing.T) {
	// One person watching on a phone and a television is one viewer. Reach is a
	// question about people; concurrency is the question that counts devices, and
	// it is answered elsewhere.
	cohorts := control.Audiences([]control.Participant{
		viewer("v1", "phone", "bea"),
		viewer("v1", "tv", "bea"),
	})

	if got := find(t, cohorts, "bea").Viewers; got != 1 {
		t.Errorf("one person on two devices is one viewer, got %d", got)
	}
}

func TestAudiencesAttributeAPersonOnceEvenWhenDevicesDisagree(t *testing.T) {
	// Two devices of one viewer carrying different provenance: they opened the
	// room from a link on one and cold on the other. Attribution wins over none,
	// whatever order the records arrive in, or the same room would report
	// different numbers on successive reads.
	first := control.Audiences([]control.Participant{
		viewer("v1", "phone", ""),
		viewer("v1", "tv", "bea"),
	})
	second := control.Audiences([]control.Participant{
		viewer("v1", "tv", "bea"),
		viewer("v1", "phone", ""),
	})

	if find(t, first, "bea").Viewers != 1 || find(t, second, "bea").Viewers != 1 {
		t.Errorf("attribution should win over its absence regardless of order: %v, %v", first, second)
	}
	if find(t, first, "").Viewers != 0 || find(t, second, "").Viewers != 0 {
		t.Errorf("the viewer should not also be counted as organic: %v, %v", first, second)
	}
}

func TestAudiencesExcludeAnybodyOnStage(t *testing.T) {
	// A guest promoted mid-stream stops being a number and becomes a tile.
	brought := viewer("guest", "d1", "bea")
	brought.Grants = control.Grants{Publish: control.PublishCamera}

	cohorts := control.Audiences([]control.Participant{cohost("bea", "d1"), brought})

	if got := find(t, cohorts, "bea").Viewers; got != 0 {
		t.Errorf("a publisher is not audience, got %d", got)
	}
}

func TestAudiencesKeepReconnectingViewers(t *testing.T) {
	// Their seat is being held and they are about to be back. Dropping them would
	// make every audience figure flicker with the network weather.
	cohorts := control.Audiences([]control.Participant{
		viewer("v1", "d1", "bea"),
		func() control.Participant {
			p := viewer("v2", "d1", "bea")
			p.Presence = control.PresenceReconnecting
			return p
		}(),
		func() control.Participant {
			p := viewer("v3", "d1", "bea")
			p.Presence = control.PresenceLeft
			return p
		}(),
	})

	if got := find(t, cohorts, "bea").Viewers; got != 2 {
		t.Errorf("a reconnecting viewer counts and a departed one does not, got %d", got)
	}
}

func TestAudiencesAreOrderedLargestFirstWithTiesByName(t *testing.T) {
	var participants []control.Participant
	add := func(via string, n int) {
		for i := 0; i < n; i++ {
			participants = append(participants, viewer(via+"-"+strconv.Itoa(i), "d1", via))
		}
	}
	add("cy", 2)
	add("bea", 2)
	add("ana", 5)

	cohorts := control.Audiences(participants)
	var order []string
	for _, cohort := range cohorts {
		order = append(order, cohort.Via)
	}

	// Ordered here rather than by the caller, so two readers cannot disagree about
	// which of two equal cohorts comes first.
	want := []string{"ana", "bea", "cy"}
	for i, expected := range want {
		if order[i] != expected {
			t.Fatalf("ordered largest first, ties by name: got %v, want %v", order, want)
		}
	}
}

func TestAudiencesCoarsenLargeCountsAndSayThatTheyDid(t *testing.T) {
	var participants []control.Participant
	for i := 0; i < 1_234; i++ {
		participants = append(participants, viewer("v"+strconv.Itoa(i), "d1", "bea"))
	}

	cohort := find(t, control.Audiences(participants), "bea")

	if cohort.Resolution != 10 {
		t.Fatalf("a count in the thousands is reported to the nearest ten, got %d", cohort.Resolution)
	}
	if cohort.Viewers%cohort.Resolution != 0 {
		t.Errorf("%d is not a multiple of its resolution %d", cohort.Viewers, cohort.Resolution)
	}
	if cohort.IsExact() {
		// The point of carrying the resolution: a consumer that assumed exactness
		// would print a precise-looking number that is not.
		t.Error("a coarsened count must not claim to be exact")
	}
}

func TestSmallAudiencesAreExact(t *testing.T) {
	cohorts := control.Audiences([]control.Participant{viewer("v1", "d1", "bea")})
	cohort := find(t, cohorts, "bea")

	if !cohort.IsExact() || cohort.Viewers != 1 {
		t.Errorf("a small count is reported exactly, got %d at resolution %d", cohort.Viewers, cohort.Resolution)
	}
}

func TestAudiencesOfARoomNobodyWasBroughtTo(t *testing.T) {
	// Nothing to report rather than a row of zeroes, so a tenant that does not
	// attribute arrivals reads as nobody knowing rather than as nobody bringing.
	if cohorts := control.Audiences(nil); len(cohorts) != 0 {
		t.Errorf("an empty room has no cohorts, got %v", cohorts)
	}
	if cohorts := control.Audiences([]control.Participant{cohost("ana", "d1")}); len(cohorts) != 0 {
		t.Errorf("a room with only publishers has no audience, got %v", cohorts)
	}
}

func TestAttributeIsFirstWriterWins(t *testing.T) {
	// The rule that keeps a token refresh from moving somebody between audiences
	// halfway through a stream.
	participant := viewer("v1", "d1", "")

	if !participant.Attribute("bea") {
		t.Fatal("the first attribution should take")
	}
	if participant.Attribute("ana") {
		t.Error("a later session must not overwrite provenance")
	}
	if participant.ArrivedVia != "bea" {
		t.Errorf("provenance should still be bea, got %q", participant.ArrivedVia)
	}
}

func TestAttributeIgnoresNothingAndSelf(t *testing.T) {
	participant := viewer("v1", "d1", "")

	if participant.Attribute("") {
		t.Error("an empty attribution is not an attribution")
	}
	if participant.Attribute("v1") {
		t.Error("nobody brings themselves")
	}
	if participant.ArrivedVia != "" {
		t.Errorf("nothing should have been recorded, got %q", participant.ArrivedVia)
	}
}
