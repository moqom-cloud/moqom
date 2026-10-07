package mls

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// PathPrefix is the subtree the handler serves.
const PathPrefix = "/mls/v1/"

// maxBodyBytes caps a request. The largest legitimate one is a commit with its
// Welcome, each bounded by maxCommitBytes, base64-inflated by a third.
const maxBodyBytes = 3 * maxCommitBytes

// Authenticate turns a request into its caller, or fails.
//
// Supplied by the control plane, which holds the token verifier. Kept out of
// this package so the delivery service depends on nothing about how tokens are
// minted, only on what a verified one says.
type Authenticate func(*http.Request) (Caller, error)

// Handler serves the delivery service's routes.
//
// Every route is the caller's own room, taken from its token: there is no room
// in a path or a body, so a device cannot read or write another room's group
// by naming it.
func Handler(service *Service, authenticate Authenticate) http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, serve func(http.ResponseWriter, *http.Request, Caller) error) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			caller, err := authenticate(r)
			if err != nil {
				// Uninformative, as the join endpoint is.
				http.Error(w, "invalid credentials", http.StatusUnauthorized)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
			// Handshake messages are per device and never worth caching.
			w.Header().Set("Cache-Control", "no-store")
			if err := serve(w, r, caller); err != nil {
				code := statusOf(err)
				if code == http.StatusInternalServerError {
					slog.Error("mls delivery", "route", pattern, "error", err)
					http.Error(w, "internal error", code)
					return
				}
				http.Error(w, err.Error(), code)
			}
		})
	}

	route("PUT "+PathPrefix+"keypackages", func(w http.ResponseWriter, r *http.Request, caller Caller) error {
		var body struct {
			KeyPackages [][]byte `json:"keyPackages"`
			LastResort  []byte   `json:"lastResort"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return errors.Join(ErrInvalid, err)
		}
		if err := service.PublishKeyPackages(r.Context(), caller, body.KeyPackages, body.LastResort); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})

	route("POST "+PathPrefix+"keypackages/claim", func(w http.ResponseWriter, r *http.Request, caller Caller) error {
		var body struct {
			Device string `json:"device"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return errors.Join(ErrInvalid, err)
		}
		data, err := service.ClaimKeyPackage(r.Context(), caller, body.Device)
		if err != nil {
			return err
		}
		return reply(w, http.StatusOK, map[string][]byte{"keyPackage": data})
	})

	route("GET "+PathPrefix+"sync", func(w http.ResponseWriter, r *http.Request, caller Caller) error {
		after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		seconds, _ := strconv.Atoi(r.URL.Query().Get("wait"))
		// The listener's write timeout is for requests that should be quick;
		// this one is held on purpose.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(MaxWait + 10*time.Second))
		snapshot, err := service.Sync(r.Context(), caller, after, time.Duration(seconds)*time.Second)
		if err != nil {
			return err
		}
		return reply(w, http.StatusOK, syncResponse(snapshot))
	})

	route("POST "+PathPrefix+"group", func(w http.ResponseWriter, r *http.Request, caller Caller) error {
		var body struct {
			GroupID []byte `json:"groupId"`
			// Absent from clients that predate suites, whose groups are 0x0001.
			CipherSuite uint16 `json:"cipherSuite"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return errors.Join(ErrInvalid, err)
		}
		if err := service.CreateGroup(r.Context(), caller, body.GroupID, CipherSuite(body.CipherSuite)); err != nil {
			return err
		}
		w.WriteHeader(http.StatusCreated)
		return nil
	})

	route("POST "+PathPrefix+"commits", func(w http.ResponseWriter, r *http.Request, caller Caller) error {
		var body struct {
			Epoch   uint64   `json:"epoch"`
			Commit  []byte   `json:"commit"`
			Welcome []byte   `json:"welcome"`
			Joiners []string `json:"joiners"`
			Removed []string `json:"removed"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return errors.Join(ErrInvalid, err)
		}
		seq, err := service.SubmitCommit(r.Context(), caller, CommitRequest{
			Epoch: body.Epoch, Commit: body.Commit, Welcome: body.Welcome,
			Joiners: body.Joiners, Removed: body.Removed,
		})
		if err != nil {
			return err
		}
		return reply(w, http.StatusOK, map[string]uint64{"seq": seq})
	})

	route("POST "+PathPrefix+"rejoin", func(w http.ResponseWriter, r *http.Request, caller Caller) error {
		if err := service.Rejoin(r.Context(), caller); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})

	route("DELETE "+PathPrefix+"presence", func(w http.ResponseWriter, r *http.Request, caller Caller) error {
		if err := service.Leave(r.Context(), caller); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})

	return mux
}

type wireGroup struct {
	ID          []byte `json:"id"`
	Epoch       uint64 `json:"epoch"`
	CipherSuite uint16 `json:"cipherSuite"`
}

type wireMessage struct {
	Seq  uint64      `json:"seq"`
	Kind MessageKind `json:"kind"`
	Data []byte      `json:"data"`
}

type wireSync struct {
	Group    *wireGroup    `json:"group"`
	Messages []wireMessage `json:"messages"`
	Members  []string      `json:"members"`
	Pending  []string      `json:"pending"`
	Cursor   uint64        `json:"cursor"`
}

// syncResponse is a snapshot as a device reads it. Who sent each message and
// who else a Welcome was for are the service's bookkeeping and are left out.
func syncResponse(snapshot Snapshot) wireSync {
	out := wireSync{
		Messages: []wireMessage{},
		Members:  snapshot.Members,
		Pending:  snapshot.Pending,
		Cursor:   snapshot.Cursor,
	}
	if snapshot.Group != nil {
		out.Group = &wireGroup{
			ID: snapshot.Group.ID, Epoch: snapshot.Group.Epoch, CipherSuite: uint16(snapshot.Group.CipherSuite),
		}
	}
	for _, message := range snapshot.Messages {
		out.Messages = append(out.Messages, wireMessage{Seq: message.Seq, Kind: message.Kind, Data: message.Data})
	}
	return out
}

func reply(w http.ResponseWriter, code int, body any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	return json.NewEncoder(w).Encode(body)
}

func statusOf(err error) int {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrConflict), errors.Is(err, ErrStale):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
