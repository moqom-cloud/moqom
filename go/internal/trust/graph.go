// Copyright 2026 Elliott Wave LLC. Licensed under BUSL-1.1; see LICENSING.md.
// CONFIDENTIAL: see doc.go.

package trust

import (
	"math"
	"sort"
	"time"
)

// Graph finds rings: groups of accounts that are run together.
//
// A farm can make each account look human. What it cannot cheaply do is make
// the accounts independent of each other, because they share the things that
// cost money -- devices, egress, the scheduler that drives them. So edges come
// from two places:
//
//   - Shared device keys. A device key lives in a Secure Enclave and cannot be
//     copied, so several accounts on one key are one device. A family iPad
//     shares a key between two or three accounts; a farm shares one between
//     dozens.
//   - Repeated co-arrival. Two strangers joining the same room within a
//     couple of seconds is chance. The same two doing it in room after room is
//     a scheduler. An edge only counts once a pair has co-arrived in MinRooms
//     distinct rooms, which keeps a popular room's go-live spike from wiring
//     its whole audience together.
//   - Co-audience. A farm can randomise its timing, but it cannot stop its
//     accounts watching the rooms its customers paid for, and a farm serves
//     several unrelated customers at once. So its accounts share an unusual
//     combination of small rooms that no two independent people share. Rooms
//     are weighted by rarity (inverse document frequency) and rooms above
//     PopularShare of the tenant's accounts are ignored, so that "both watch
//     the biggest streamer" links nobody. An edge needs at least MinShared
//     rare rooms in common and a weighted Jaccard of at least MinJaccard.
//
// Connected components of the strong edges with at least MinRing members are
// rings.
type Graph struct {
	// Window is how close two joins must be to count as co-arrival. Default 2s.
	Window time.Duration
	// MinRooms distinct co-arrivals make a timing edge. Default 3.
	MinRooms int
	// MinRing is the smallest component reported. Default 4.
	MinRing int
	// PopularShare excludes rooms attended by more than this share of
	// accounts from co-audience. Default 0.1.
	PopularShare float64
	// MinShared and MinJaccard define a co-audience edge. Defaults 3 and 0.6.
	MinShared  int
	MinJaccard float64

	recent   map[string][]arrival // room -> recent joins
	pairs    map[[2]Pseudonym]*pair
	devices  map[Pseudonym]map[Pseudonym]bool // device -> accounts
	attended map[Pseudonym]map[string]bool    // account -> rooms
	ringOf   map[Pseudonym]int
	linkOf   map[Pseudonym]Link
	rings    []Ring
}

// Link is how an account is tied into its ring, as a bit set.
type Link uint8

const (
	LinkDevice Link = 1 << iota
	LinkTimed
	LinkCoAudience
)

type arrival struct {
	account Pseudonym
	at      time.Time
	prefix  Pseudonym
}

type pair struct {
	rooms        map[string]bool
	samePrefix   int
	sharedDevice bool
}

// Ring is one detected group.
type Ring struct {
	ID      int
	Members []Pseudonym
	// Devices is how many member pairs share a device key, Timed how many are
	// linked by repeated co-arrival, and CoAudience how many by shared rare
	// rooms.
	Devices, Timed, CoAudience int
}

// NewGraph returns an empty graph.
func NewGraph() *Graph {
	return &Graph{
		recent:   map[string][]arrival{},
		pairs:    map[[2]Pseudonym]*pair{},
		devices:  map[Pseudonym]map[Pseudonym]bool{},
		attended: map[Pseudonym]map[string]bool{},
		ringOf:   map[Pseudonym]int{},
	}
}

func (g *Graph) defaults() {
	if g.Window == 0 {
		g.Window = 2 * time.Second
	}
	if g.MinRooms == 0 {
		g.MinRooms = 3
	}
	if g.MinRing == 0 {
		g.MinRing = 4
	}
	if g.PopularShare == 0 {
		g.PopularShare = 0.1
	}
	if g.MinShared == 0 {
		g.MinShared = 3
	}
	if g.MinJaccard == 0 {
		g.MinJaccard = 0.6
	}
}

func key(a, b Pseudonym) [2]Pseudonym {
	if a > b {
		a, b = b, a
	}
	return [2]Pseudonym{a, b}
}

func (g *Graph) pair(a, b Pseudonym) *pair {
	k := key(a, b)
	p := g.pairs[k]
	if p == nil {
		p = &pair{rooms: map[string]bool{}}
		g.pairs[k] = p
	}
	return p
}

// Device records that account used device.
func (g *Graph) Device(account, device Pseudonym) {
	if device == "" || account == "" {
		return
	}
	accounts := g.devices[device]
	if accounts == nil {
		accounts = map[Pseudonym]bool{}
		g.devices[device] = accounts
	}
	if accounts[account] {
		return
	}
	// A key shared by hundreds is not a device any more, it is a bug or a test
	// rig, and wiring every pair would be quadratic in it.
	if len(accounts) < 256 {
		for other := range accounts {
			g.pair(account, other).sharedDevice = true
		}
	}
	accounts[account] = true
}

// AccountsOn is how many accounts have used device.
func (g *Graph) AccountsOn(device Pseudonym) int { return len(g.devices[device]) }

// Join records an arrival.
func (g *Graph) Join(room string, account Pseudonym, at time.Time, prefix Pseudonym) {
	g.defaults()
	rooms := g.attended[account]
	if rooms == nil {
		rooms = map[string]bool{}
		g.attended[account] = rooms
	}
	if len(rooms) < 64 {
		rooms[room] = true
	}
	recent := g.recent[room]
	kept := recent[:0]
	for _, a := range recent {
		if at.Sub(a.at) <= g.Window {
			kept = append(kept, a)
		}
	}
	for _, a := range kept {
		if a.account == account {
			continue
		}
		p := g.pair(account, a.account)
		if len(p.rooms) < 16 {
			p.rooms[room] = true
		}
		if prefix != "" && prefix == a.prefix {
			p.samePrefix++
		}
	}
	kept = append(kept, arrival{account: account, at: at, prefix: prefix})
	if len(kept) > 64 {
		kept = kept[len(kept)-64:]
	}
	g.recent[room] = kept
}

func (g *Graph) strong(p *pair) (device, timed bool) {
	return p.sharedDevice, len(p.rooms) >= g.MinRooms
}

// Detect recomputes rings.
func (g *Graph) Detect() []Ring {
	g.defaults()
	parent := map[Pseudonym]Pseudonym{}
	var find func(Pseudonym) Pseudonym
	find = func(x Pseudonym) Pseudonym {
		if parent[x] == "" || parent[x] == x {
			parent[x] = x
			return x
		}
		root := find(parent[x])
		parent[x] = root
		return root
	}
	links := map[Pseudonym]Link{}
	union := func(x, y Pseudonym, link Link) {
		links[x] |= link
		links[y] |= link
		a, b := find(x), find(y)
		if a != b {
			parent[a] = b
		}
	}
	for k, p := range g.pairs {
		device, timed := g.strong(p)
		if device {
			union(k[0], k[1], LinkDevice)
		}
		if timed {
			union(k[0], k[1], LinkTimed)
		}
	}
	coaudience := g.coAudience()
	for k := range coaudience {
		union(k[0], k[1], LinkCoAudience)
	}
	components := map[Pseudonym][]Pseudonym{}
	for member := range parent {
		root := find(member)
		components[root] = append(components[root], member)
	}
	g.rings, g.ringOf, g.linkOf = nil, map[Pseudonym]int{}, map[Pseudonym]Link{}
	var roots []Pseudonym
	for root, members := range components {
		if len(members) >= g.MinRing {
			roots = append(roots, root)
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i] < roots[j] })
	for i, root := range roots {
		members := components[root]
		sort.Slice(members, func(a, b int) bool { return members[a] < members[b] })
		ring := Ring{ID: i + 1, Members: members}
		for k, p := range g.pairs {
			if find(k[0]) != root {
				continue
			}
			device, timed := g.strong(p)
			if device {
				ring.Devices++
			}
			if timed {
				ring.Timed++
			}
		}
		for k := range coaudience {
			if find(k[0]) == root {
				ring.CoAudience++
			}
		}
		g.rings = append(g.rings, ring)
		for _, m := range members {
			g.ringOf[m] = ring.ID
			g.linkOf[m] = links[m]
		}
	}
	return g.rings
}

// coAudience finds co-audience edges. Candidates come from an inverted index
// over rare rooms only, which bounds the work by the square of the largest rare
// room rather than the square of the tenant. At larger scale the same test
// runs over MinHash signatures with locality-sensitive banding.
func (g *Graph) coAudience() map[[2]Pseudonym]bool {
	accounts := len(g.attended)
	if accounts == 0 {
		return nil
	}
	// Sorted throughout, so that float sums -- and so borderline edges -- come
	// out the same on every run.
	accountList := make([]Pseudonym, 0, accounts)
	for account := range g.attended {
		accountList = append(accountList, account)
	}
	sort.Slice(accountList, func(i, j int) bool { return accountList[i] < accountList[j] })
	members := map[string][]Pseudonym{}
	var roomList []string
	for _, account := range accountList {
		for room := range g.attended[account] {
			if members[room] == nil {
				roomList = append(roomList, room)
			}
			members[room] = append(members[room], account)
		}
	}
	sort.Strings(roomList)
	popular := int(math.Max(g.PopularShare*float64(accounts), 8))
	idf := map[string]float64{}
	for room, list := range members {
		if len(list) <= popular {
			idf[room] = math.Log(float64(accounts) / float64(len(list)))
		}
	}
	weights := map[Pseudonym]float64{}
	for _, room := range roomList {
		for _, account := range members[room] {
			weights[account] += idf[room]
		}
	}
	weightOf := func(account Pseudonym) float64 { return weights[account] }
	shared := map[[2]Pseudonym]float64{}
	count := map[[2]Pseudonym]int{}
	for _, room := range roomList {
		w, rare := idf[room]
		if !rare {
			continue
		}
		list := members[room]
		for i := 0; i < len(list); i++ {
			for j := i + 1; j < len(list); j++ {
				k := key(list[i], list[j])
				shared[k] += w
				count[k]++
			}
		}
	}
	edges := map[[2]Pseudonym]bool{}
	for k, w := range shared {
		if count[k] < g.MinShared {
			continue
		}
		union := weightOf(k[0]) + weightOf(k[1]) - w
		if union > 0 && w/union >= g.MinJaccard {
			edges[k] = true
		}
	}
	return edges
}

// RingOf is the ring an account was in at the last Detect, or 0.
func (g *Graph) RingOf(account Pseudonym) int { return g.ringOf[account] }

// LinkOf is how an account was tied into its ring at the last Detect.
//
// It matters because co-audience alone is the edge most likely to catch a
// bystander: the real fans of a streamer who bought views watch the same rare
// rooms as the farm. A shared device or repeated co-arrival is something the
// account did with the farm; co-audience may only be something it did near it.
func (g *Graph) LinkOf(account Pseudonym) Link { return g.linkOf[account] }

// Bursts finds synchronised arrivals in one room.
//
// Joins are counted in fixed buckets and compared with an EWMA of the room's own
// rate. A bucket whose count is improbable under a Poisson model of that rate
// is a burst and its members are marked. A go-live spike is also a burst, which
// is why membership alone is a weak reason and the ring detector is what
// separates a notification from a scheduler.
type Bursts struct {
	// Bucket width. Default 5s.
	Bucket time.Duration
	// Tail is the Poisson tail probability below which a bucket is a burst.
	// Default 1e-4.
	Tail float64
	// MinCount is the smallest burst. Default 8.
	MinCount int

	rate    float64
	start   time.Time
	members []string
	seen    bool
}

// Join adds an arrival and returns the members of any burst the previous bucket
// turned out to be.
func (b *Bursts) Join(at time.Time, member string) []string {
	if b.Bucket == 0 {
		b.Bucket = 5 * time.Second
	}
	if b.Tail == 0 {
		b.Tail = 1e-4
	}
	if b.MinCount == 0 {
		b.MinCount = 8
	}
	var burst []string
	if !b.seen {
		b.start, b.seen, b.rate = at.Truncate(b.Bucket), true, 1
	}
	for at.Sub(b.start) >= b.Bucket {
		burst = append(burst, b.close()...)
		b.start = b.start.Add(b.Bucket)
		if at.Sub(b.start) > 100*b.Bucket {
			b.start = at.Truncate(b.Bucket)
		}
	}
	b.members = append(b.members, member)
	return burst
}

func (b *Bursts) close() []string {
	count := len(b.members)
	members := b.members
	b.members = nil
	var burst []string
	if count >= b.MinCount && poissonTail(b.rate, count) < b.Tail {
		burst = members
	}
	// A burst updates the baseline only a little, so a farm cannot teach a room
	// that bursts are normal by bursting every few minutes.
	alpha := 0.1
	if burst != nil {
		alpha = 0.01
	}
	b.rate = (1-alpha)*b.rate + alpha*float64(count)
	b.rate = math.Max(b.rate, 0.2)
	return burst
}

// poissonTail is P(X >= k) for X ~ Poisson(lambda).
func poissonTail(lambda float64, k int) float64 {
	term, cumulative := math.Exp(-lambda), 0.0
	for i := 0; i < k; i++ {
		cumulative += term
		term *= lambda / float64(i+1)
	}
	return math.Max(0, 1-cumulative)
}

// GlobalIndex links confirmed bot devices across tenants without telling any
// tenant about any other.
//
// It holds platform-scope device pseudonyms and pseudonyms of the tenants that
// confirmed them -- not tenant names, not accounts. A tenant asking about a
// device learns one bit, "confirmed elsewhere", and only when at least
// MinTenants other tenants confirmed it, so the answer cannot be used to test
// whether a particular competitor saw a particular device.
type GlobalIndex struct {
	// MinTenants other tenants must have confirmed. Default 3.
	MinTenants int
	confirmed  map[Pseudonym]map[Pseudonym]bool
}

// NewGlobalIndex returns an empty index.
func NewGlobalIndex() *GlobalIndex {
	return &GlobalIndex{confirmed: map[Pseudonym]map[Pseudonym]bool{}}
}

// Confirm records a human-confirmed bot device.
func (x *GlobalIndex) Confirm(device, tenant Pseudonym) {
	if device == "" {
		return
	}
	set := x.confirmed[device]
	if set == nil {
		set = map[Pseudonym]bool{}
		x.confirmed[device] = set
	}
	set[tenant] = true
}

// Retract removes a confirmation, after a successful appeal.
func (x *GlobalIndex) Retract(device, tenant Pseudonym) { delete(x.confirmed[device], tenant) }

// Elsewhere reports whether enough other tenants confirmed device.
func (x *GlobalIndex) Elsewhere(device, tenant Pseudonym) bool {
	minimum := x.MinTenants
	if minimum == 0 {
		minimum = 3
	}
	others := len(x.confirmed[device])
	if x.confirmed[device][tenant] {
		others--
	}
	return others >= minimum
}
