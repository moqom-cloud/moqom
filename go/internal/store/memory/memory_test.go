package memory_test

import (
	"testing"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/store"
	"github.com/moqom-cloud/moqom/go/internal/store/memory"
	"github.com/moqom-cloud/moqom/go/internal/store/storetest"
)

// The whole contract, against the map.
//
// There is nothing else in this file on purpose: a behaviour worth asserting
// about this store is a behaviour Postgres has to have too, and asserting it here
// would let the two drift.
func TestConformance(t *testing.T) {
	storetest.Run(t, func(_ *testing.T, now func() time.Time) (store.Store, func()) {
		return memory.New(now, nil), func() {}
	})
}
