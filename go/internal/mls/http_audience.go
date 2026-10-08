package mls

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

type wireEnvelope struct {
	GroupID []byte `json:"groupId"`
	Epoch   uint64 `json:"epoch"`
	Data    []byte `json:"data"`
}

type wireSeat struct {
	Device    string `json:"device"`
	PublicKey []byte `json:"publicKey"`
}

func audienceRoutes(service *Service, route func(string, func(http.ResponseWriter, *http.Request, Caller) error)) {
	// A viewer's heartbeat and its wait for the next epoch key, in one request.
	route("POST "+PathPrefix+"audience", func(w http.ResponseWriter, r *http.Request, caller Caller) error {
		var body struct {
			PublicKey []byte `json:"publicKey"`
			HaveGroup []byte `json:"haveGroupId"`
			HaveEpoch uint64 `json:"haveEpoch"`
			Wait      int    `json:"wait"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return errors.Join(ErrInvalid, err)
		}
		var have *EpochRef
		if body.HaveGroup != nil {
			have = &EpochRef{GroupID: body.HaveGroup, Epoch: body.HaveEpoch}
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(MaxWait + 10*time.Second))
		envelope, err := service.JoinAudience(r.Context(), caller, body.PublicKey, have, time.Duration(body.Wait)*time.Second)
		if err != nil {
			return err
		}
		if envelope == nil {
			w.WriteHeader(http.StatusNoContent)
			return nil
		}
		return reply(w, http.StatusOK, wireEnvelope{GroupID: envelope.GroupID, Epoch: envelope.Epoch, Data: envelope.Data})
	})

	route("DELETE "+PathPrefix+"audience", func(w http.ResponseWriter, r *http.Request, caller Caller) error {
		if err := service.LeaveAudience(r.Context(), caller); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})

	route("GET "+PathPrefix+"audience/work", func(w http.ResponseWriter, r *http.Request, caller Caller) error {
		work, err := service.AudienceWork(r.Context(), caller)
		if err != nil {
			return err
		}
		seats := make([]wireSeat, 0, len(work.Seats))
		for _, seat := range work.Seats {
			seats = append(seats, wireSeat{Device: seat.Device, PublicKey: seat.Key})
		}
		return reply(w, http.StatusOK, map[string]any{"groupId": work.GroupID, "epoch": work.Epoch, "seats": seats})
	})

	route("POST "+PathPrefix+"audience/envelopes", func(w http.ResponseWriter, r *http.Request, caller Caller) error {
		var body struct {
			GroupID   []byte            `json:"groupId"`
			Epoch     uint64            `json:"epoch"`
			Envelopes map[string][]byte `json:"envelopes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return errors.Join(ErrInvalid, err)
		}
		if err := service.SealAudience(r.Context(), caller, EpochRef{GroupID: body.GroupID, Epoch: body.Epoch}, body.Envelopes); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
}
