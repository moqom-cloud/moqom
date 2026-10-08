package billing_test

import (
	"testing"

	"github.com/moqom-cloud/moqom/go/internal/billing"
	"github.com/moqom-cloud/moqom/go/internal/billing/billingtest"
)

func TestMemoryConformance(t *testing.T) {
	billingtest.Run(t, func(*testing.T) billing.Store { return billing.NewMemory() })
}
