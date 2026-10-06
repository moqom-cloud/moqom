package byoc_test

import (
	"testing"

	"github.com/moqom-cloud/moqom/go/internal/byoc"
	"github.com/moqom-cloud/moqom/go/internal/byoc/byoctest"
)

func TestMemoryConformance(t *testing.T) {
	byoctest.Run(t, func(*testing.T) byoc.Store { return byoc.NewMemory() })
}
