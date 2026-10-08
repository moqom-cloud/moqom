// Copyright 2026 Elliott Wave LLC. Proprietary and confidential; see LICENSING.md.
// CONFIDENTIAL: see internal/trust/doc.go.

// Command moqom-trust-eval runs the trust adversarial harness and prints its
// results as Markdown, for docs/TRUST-AND-SAFETY.md.
//
// Every number it prints is from synthetic populations whose behaviour is an
// assumption; it says so in its output, and the output must not be quoted
// without that label.
//
//	go run ./cmd/moqom-trust-eval -seeds 3
package main

import (
	"flag"
	"fmt"

	"github.com/moqom-cloud/moqom/go/internal/trust/sim"
)

func main() {
	seeds := flag.Int("seeds", 3, "independent seeds to run and average")
	flag.Parse()

	fmt.Println("> **Synthetic.** Simulated populations; behaviour distributions are assumptions. Not a measurement of real traffic.")
	fmt.Println()

	scenarios := []struct {
		name   string
		mutate func(*sim.Config)
	}{
		{"baseline (defended learner, no poisoning)", func(*sim.Config) {}},
		{"compromised moderator, defended learner", func(c *sim.Config) { c.Poison = true }},
		{"compromised moderator, undefended learner", func(c *sim.Config) { c.Poison = true; c.Defended = false }},
		{"phone farm with one handset per account", func(c *sim.Config) {
			for i := range c.Farms {
				if c.Farms[i].Kind == sim.PhoneFarm {
					c.Farms[i].AccountsPerDevice = 1
				}
			}
		}},
	}
	for _, scenario := range scenarios {
		var results []sim.Result
		for seed := 1; seed <= *seeds; seed++ {
			cfg := sim.DefaultConfig(uint64(seed))
			scenario.mutate(&cfg)
			results = append(results, sim.Run(cfg))
		}
		fmt.Printf("### %s\n\n", scenario.name)
		fmt.Println(sim.Table(results))
	}

	fmt.Println("### Publisher replay detection")
	fmt.Println()
	fmt.Println(sim.PublisherTable(sim.EvaluatePublishers(1, 200)))

	fmt.Println("### Threshold probing")
	fmt.Println()
	fmt.Println(sim.ProbeTable(sim.Probe(1, 40, 30)))

	fmt.Println("### Drift alarm")
	fmt.Println()
	fmt.Println(sim.DriftTable(sim.Drift(1)))
}
