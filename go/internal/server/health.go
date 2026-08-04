package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/store"
	"github.com/moqom-cloud/moqom/go/internal/timecode"
)

// Two probes, because they answer different questions and an orchestrator acts
// on them differently.
//
// `/livez` asks whether this process should be killed and replaced. It is
// deliberately trivial: a control plane whose database has gone away is not
// improved by being restarted, and a liveness probe that fails on a dependency
// turns a database blip into a crash loop across every replica at once.
//
// `/healthz` asks whether this process should be sent traffic, and that does
// depend on the store: a replica that cannot read a room cannot mint a token or
// answer a moderator, and it should leave the load balancer rather than serve
// errors. The relay tier uses the same two names for the same two meanings.
const (
	LivenessPath  = "/livez"
	ReadinessPath = "/healthz"
)

// howLongAProbeWaits bounds the store read behind readiness.
//
// Short on purpose. The probe interval is seconds, so a check that can hang for
// longer than the interval stacks up, and the useful answer when a database is
// slow enough to miss this deadline is the same as when it is down.
const howLongAProbeWaits = 2 * time.Second

// HealthHandler serves the probes.
//
// HTTP rather than only the gRPC health service, because three things need to
// read this and two of them cannot speak gRPC: a Google Cloud uptime check is
// HTTP over TCP, and a load balancer health check is easiest that way. The gRPC
// health service is registered as well, for Kubernetes probes that prefer it.
func (s *Server) HealthHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc(LivenessPath, func(w http.ResponseWriter, _ *http.Request) {
		respond(w, http.StatusOK, map[string]any{"status": "alive"})
	})

	mux.HandleFunc(ReadinessPath, func(w http.ResponseWriter, r *http.Request) {
		ctx, done := context.WithTimeout(r.Context(), howLongAProbeWaits)
		defer done()

		body := map[string]any{"relays": s.core.relayCount()}
		if clock := s.clockReport(); clock != nil {
			body["clock"] = clock
		}
		if err := s.reachable(ctx); err != nil {
			body["status"] = "unready"
			// The reason, not just the verdict. An operator reading a load
			// balancer's view of a dead backend should not have to go and find
			// the log line that says why.
			body["error"] = err.Error()
			respond(w, http.StatusServiceUnavailable, body)
			return
		}
		body["status"] = "ready"
		respond(w, http.StatusOK, body)
	})

	return mux
}

// clockReport is what the host clock says about itself, or nil for a clock that
// does not know -- a fixed one in a test, or an injected wall clock.
//
// On the readiness body rather than a page of its own, and deliberately not
// part of the verdict: a replica whose clock has drifted can still mint tokens
// and answer moderators, so taking it out of the load balancer would turn a
// measurement problem into an outage. What it is for is the question an
// operator has after reading "clock fell back to local time" in a log -- how
// wide is it now, how wide is it allowed to be, and how often has this
// happened -- which otherwise needs a build to answer.
func (s *Server) clockReport() map[string]any {
	reporter, ok := s.core.clock.(interface{ Report() timecode.Report })
	if !ok {
		return nil
	}
	report := reporter.Report()
	return map[string]any{
		"trust":         report.Trust.String(),
		"synchronised":  report.Health.Synchronised,
		"readable":      report.Readable,
		"maxErrorMs":    report.Health.MaxError.Milliseconds(),
		"ceilingMs":     report.Ceiling.Milliseconds(),
		"leapPending":   report.Health.LeapPending,
		"fallbackCount": report.Fallbacks,
	}
}

// reachable is the cheapest question that proves the store answers.
//
// A list of one room under a tenant nobody has is a real query with an empty
// result: it crosses the connection pool, the network and the database, and it
// reads no customer's data doing it.
func (s *Server) reachable(ctx context.Context) error {
	_, err := s.core.store.ListRooms(ctx, store.RoomQuery{
		Tenant: "-probe-",
		Paging: store.Paging{PageSize: 1},
	})
	return err
}

func respond(w http.ResponseWriter, code int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
