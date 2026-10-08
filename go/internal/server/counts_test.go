package server

import (
	"context"
	"testing"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/store/memory"
)

type leaveOut map[string]bool

func (l leaveOut) Countable(_, _ string, p control.Participant) bool { return !l[p.Identity.Device] }

func TestAudienceFilterChangesCountsOnly(t *testing.T) {
	ctx := context.Background()
	now := func() time.Time { return time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC) }
	st := memory.New(now, nil)
	if _, err := st.CreateRoom(ctx, control.Room{Tenant: "t", Room: "r"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []control.Identity{{Username: "ana", Device: "d1"}, {Username: "bot", Device: "d2"}, {Username: "bot", Device: "d3"}} {
		if err := st.SaveParticipant(ctx, "t", "r", control.Participant{Identity: id}); err != nil {
			t.Fatal(err)
		}
	}
	c := &core{store: st}
	all, err := c.counts(ctx, "t", "r")
	if err != nil || all.devices != 3 || all.people != 2 {
		t.Fatalf("unfiltered %+v %v", all, err)
	}
	c.audience = leaveOut{"d2": true, "d3": true}
	some, err := c.counts(ctx, "t", "r")
	if err != nil || some.devices != 1 || some.people != 1 {
		t.Fatalf("filtered %+v %v", some, err)
	}
}
