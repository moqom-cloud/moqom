// Who brought the audience.
//
// The one thing about a room that cannot be worked out after the fact. Everything
// else here is reconstructible -- who was present, what they were allowed to do,
// what a moderator did and when -- because it is either stored or derivable from
// what is stored. Provenance is not: an hour into a stream, a viewer who tapped a
// cohost's link is indistinguishable from one who found the room on the front
// page, and no amount of data kept about the room recovers the difference.
//
// So it is captured at the only moment it exists, carried in the token because
// the token is the only thing that reaches a relay in another region without a
// lookup on the join path, and aggregated here.
//
// # Why this is counts and not a list
//
// Because of what it is for. "Whose audience is this" is asked by a host deciding
// whether a collaboration worked, and by whoever divides the money afterwards --
// both of which want a handful of integers. Naming the viewers instead would mean
// putting a million identities somewhere for a question nobody asked, and it
// would make the audience of a room a thing that can be enumerated and lifted,
// which is a different product with different obligations.

package control

// Audience is one cohort of viewers and the cohost who brought them.
type Audience struct {
	// Via is the username whose following they came through, empty for the ones
	// who arrived by themselves.
	Via string

	// Viewers is how many are here now, rounded to Resolution.
	Viewers int

	// Resolution is what Viewers was rounded to. One means exact.
	Resolution int
}

// IsExact reports whether this count is precise.
func (a Audience) IsExact() bool { return a.Resolution <= 1 }

// Audiences divides a room's viewers by who brought them.
//
// Counts people rather than devices, because an audience of one person watching
// on a phone and a television is an audience of one. That is the opposite of the
// choice Room.ParticipantCount makes, and deliberately so: concurrency is a
// question about load and is counted per device, reach is a question about people
// and is counted per person.
//
// Counts viewers rather than everybody in the room. Somebody publishing is on the
// stage, not in the audience, so a guest promoted mid-stream leaves the cohort
// that brought them -- which reads oddly written down and is right on screen,
// where they have stopped being a number and become a tile.
//
// Ordered largest first with ties broken by name, so that a caller rendering this
// does not have to, and two callers cannot disagree.
func Audiences(participants []Participant) []Audience {
	// By username, so a person on several devices is counted once. Nested rather
	// than counted directly because two devices of one viewer may carry different
	// provenance -- they joined through different links -- and the first
	// attribution has to win consistently rather than by map order.
	people := make(map[string]string)
	for _, participant := range participants {
		if !isAudience(participant) {
			continue
		}
		username := participant.Identity.Username
		via, seen := people[username]
		if !seen || (via == "" && participant.ArrivedVia != "") {
			people[username] = participant.ArrivedVia
		}
	}

	totals := make(map[string]int, len(people))
	for _, via := range people {
		totals[via]++
	}

	cohorts := make([]Audience, 0, len(totals))
	for via, viewers := range totals {
		resolution := resolutionFor(viewers)
		cohorts = append(cohorts, Audience{
			Via:        via,
			Viewers:    round(viewers, resolution),
			Resolution: resolution,
		})
	}

	sortAudiences(cohorts)
	return cohorts
}

// isAudience reports whether a participant counts towards an audience.
//
// Excludes anyone who can publish, and anyone who has gone for good. A
// reconnecting viewer is still counted: their seat is being held, they are about
// to be back, and dropping them would make every audience figure flicker with the
// network weather.
func isAudience(p Participant) bool {
	return p.Grants.Publish == 0 && p.Presence != PresenceLeft
}

// resolutionFor is how coarse a count of this size is reported.
//
// Roughly three significant figures throughout, which is both as much as anybody
// reads off a viewer count and as much precision as the number deserves -- a
// figure that changes forty times a second is not exact in any useful sense even
// when it is measured exactly.
//
// The coarsening is what keeps the room control track quiet. Publishing every
// change of an exact count in a busy room would spend a track that every single
// client subscribes to on a digit nobody looks at; publishing when the rounded
// figure moves means a room of a million sends an update per ten thousand
// arrivals.
func resolutionFor(viewers int) int {
	switch {
	case viewers < 1_000:
		return 1
	case viewers < 10_000:
		return 10
	case viewers < 100_000:
		return 100
	case viewers < 1_000_000:
		return 1_000
	default:
		return 10_000
	}
}

// round to the nearest multiple, halves upwards.
func round(value, resolution int) int {
	if resolution <= 1 {
		return value
	}
	return ((value + resolution/2) / resolution) * resolution
}

func sortAudiences(cohorts []Audience) {
	// Insertion sort: this is at most a handful of entries -- one per cohost plus
	// the organic cohort -- and reaching for the sort package to order four things
	// costs more than it saves.
	for i := 1; i < len(cohorts); i++ {
		for j := i; j > 0 && precedes(cohorts[j], cohorts[j-1]); j-- {
			cohorts[j], cohorts[j-1] = cohorts[j-1], cohorts[j]
		}
	}
}

func precedes(a, b Audience) bool {
	if a.Viewers != b.Viewers {
		return a.Viewers > b.Viewers
	}
	return a.Via < b.Via
}
