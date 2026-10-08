package recording

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Storage classes, as billing names them.
const (
	ClassStandard = "standard"
	ClassNearline = "nearline"
	ClassColdline = "coldline"
	ClassArchive  = "archive"
)

const day = 24 * time.Hour

// Lifecycle is the bucket's lifecycle rules: how many days after an object is
// written it moves to each colder class, and when it is deleted. Zero is
// never. It must match the bucket's rules, which Terraform sets, or storage
// is billed in the wrong class.
type Lifecycle struct {
	Nearline, Coldline, Archive, Delete int
}

// ParseLifecycle reads "nearline=30,coldline=90,archive=365,delete=0". A
// missing rule is zero. Empty is everything Standard, kept for ever.
func ParseLifecycle(text string) (Lifecycle, error) {
	var l Lifecycle
	for part := range strings.SplitSeq(text, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, _ := strings.Cut(part, "=")
		days, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || days < 0 {
			return Lifecycle{}, fmt.Errorf("recording lifecycle %q: %q is not a number of days", text, part)
		}
		switch strings.TrimSpace(name) {
		case ClassNearline:
			l.Nearline = days
		case ClassColdline:
			l.Coldline = days
		case ClassArchive:
			l.Archive = days
		case "delete":
			l.Delete = days
		default:
			return Lifecycle{}, fmt.Errorf("recording lifecycle %q: unknown rule %q", text, name)
		}
	}
	return l, nil
}

// classAt is the class an object is in at an age of whole days, matching
// Cloud Storage's Age condition: a rule of N days applies from N days on.
func (l Lifecycle) classAt(days int) string {
	switch {
	case l.Archive > 0 && days >= l.Archive:
		return ClassArchive
	case l.Coldline > 0 && days >= l.Coldline:
		return ClassColdline
	case l.Nearline > 0 && days >= l.Nearline:
		return ClassNearline
	}
	return ClassStandard
}

// StorageCharge is the bytes one attempt held in one class on one UTC day.
// ID names the recording, attempt, day and class, so a pass that runs twice
// reports the same charge twice and billing takes it once.
type StorageCharge struct {
	ID        string
	Tenant    string
	Room      string
	Recording string
	Class     string
	Bytes     int64
	Seconds   int64
}

// StorageBiller charges storage. Billing implements it; it must be
// idempotent on StorageCharge.ID.
type StorageBiller interface {
	BillStorage(ctx context.Context, charges []StorageCharge) error
}

// StorageBilling bills what recordings hold in the bucket, a closed UTC day at
// a time, until the lifecycle deletes it.
//
// Each attempt's files are written together and age together, so an attempt
// is one quantity of bytes that changes class on the lifecycle's days. A
// recording's StorageBilledThrough is the midnight it has been billed to; a
// pass bills from there to the last midnight and moves it on. Files an
// attempt adds later were written after that midnight, so nothing is missed.
type StorageBilling struct {
	Store     Store
	Biller    StorageBiller
	Lifecycle Lifecycle
	Now       func() time.Time
	// Batch is how many recordings a pass takes. The rest wait for the next.
	Batch int
}

// errStale is a recording that changed while it was being billed. The next
// pass bills it again, and the charges already made are not repeated.
var errStale = errors.New("recording changed while its storage was billed")

// Run is one pass. It returns how many recordings it billed.
func (b *StorageBilling) Run(ctx context.Context) (int, error) {
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	batch := b.Batch
	if batch <= 0 {
		batch = 500
	}
	through := now().UTC().Truncate(day)
	due, err := b.Store.StorageDue(ctx, through, batch)
	if err != nil {
		return 0, err
	}
	billed := 0
	for _, r := range due {
		charged, err := b.bill(ctx, r, through)
		if errors.Is(err, errStale) {
			continue
		}
		if err != nil {
			return billed, fmt.Errorf("recording %s: %w", r.ID, err)
		}
		if charged {
			billed++
		}
	}
	return billed, nil
}

// Loop runs a pass every interval until ctx ends.
func (b *StorageBilling) Loop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if n, err := b.Run(ctx); err != nil {
			slog.WarnContext(ctx, "could not bill recording storage", "error", err, "billed", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// held is one attempt's files.
type held struct {
	attempt uint32
	at      time.Time
	bytes   int64
}

func (r Recording) held() []held {
	byAttempt := map[uint32]*held{}
	for _, a := range r.Artifacts {
		at := a.StoredAt
		if at.IsZero() {
			// Written before artifacts were timestamped.
			at = r.EndedAt
		}
		if at.IsZero() {
			at = r.StartedAt
		}
		h := byAttempt[a.Attempt]
		if h == nil {
			h = &held{attempt: a.Attempt, at: at}
			byAttempt[a.Attempt] = h
		}
		if at.Before(h.at) {
			h.at = at
		}
		h.bytes += int64(min(a.Bytes, uint64(1)<<62))
	}
	out := make([]held, 0, len(byAttempt))
	for _, h := range byAttempt {
		out = append(out, *h)
	}
	slices.SortFunc(out, func(a, b held) int { return int(a.attempt) - int(b.attempt) })
	return out
}

// StorageCharges is what r owes for storage from its watermark to through,
// and whether everything it stored has then been deleted.
func (l Lifecycle) StorageCharges(r Recording, through time.Time) ([]StorageCharge, bool) {
	files := r.held()
	if len(files) == 0 {
		return nil, false
	}
	from := r.StorageBilledThrough
	if from.IsZero() {
		from = files[0].at
		for _, h := range files {
			from = minTime(from, h.at)
		}
	}
	var charges []StorageCharge
	done := l.Delete > 0
	for _, h := range files {
		end := through
		if l.Delete > 0 {
			gone := h.at.Add(time.Duration(l.Delete) * day)
			end = minTime(end, gone)
			done = done && !through.Before(gone)
		}
		start := maxTime(from, h.at)
		for d := start.UTC().Truncate(day); d.Before(end); d = d.Add(day) {
			seconds := map[string]int64{}
			lo, hi := maxTime(start, d), minTime(end, d.Add(day))
			for lo.Before(hi) {
				age := int(lo.Sub(h.at) / day)
				class := l.classAt(age)
				next := minTime(hi, h.at.Add(time.Duration(age+1)*day))
				seconds[class] += int64(next.Sub(lo).Round(time.Second) / time.Second)
				lo = next
			}
			for _, class := range []string{ClassStandard, ClassNearline, ClassColdline, ClassArchive} {
				if seconds[class] == 0 || h.bytes == 0 {
					continue
				}
				charges = append(charges, StorageCharge{
					ID:        fmt.Sprintf("%s:%d:%s:%s", r.ID, h.attempt, d.Format(time.DateOnly), class),
					Tenant:    r.Tenant,
					Room:      r.Room,
					Recording: r.ID,
					Class:     class,
					Bytes:     h.bytes,
					Seconds:   seconds[class],
				})
			}
		}
	}
	return charges, done
}

func (b *StorageBilling) bill(ctx context.Context, r Recording, through time.Time) (bool, error) {
	charges, done := b.Lifecycle.StorageCharges(r, through)
	if len(charges) == 0 && !done {
		// Written since the last midnight: nothing is owed yet.
		return false, nil
	}
	if len(charges) > 0 {
		if err := b.Biller.BillStorage(ctx, charges); err != nil {
			return false, err
		}
	}
	files, watermark := len(r.Artifacts), r.StorageBilledThrough
	_, err := b.Store.Update(ctx, r.ID, func(next *Recording) error {
		if len(next.Artifacts) != files || !next.StorageBilledThrough.Equal(watermark) {
			return errStale
		}
		next.StorageBilledThrough = through
		next.StorageDone = done
		return nil
	})
	return err == nil, err
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
