package control

import (
	"errors"
	"testing"
)

func TestStunEndpointsParseWithTheDefaultPort(t *testing.T) {
	for value, want := range map[string]StunEndpoint{
		"relay.moqom.cloud":       {Host: "relay.moqom.cloud", Port: 3478},
		" relay.moqom.cloud:3479": {Host: "relay.moqom.cloud", Port: 3479},
		"203.0.113.7":             {Host: "203.0.113.7", Port: 3478},
		"203.0.113.7:5349":        {Host: "203.0.113.7", Port: 5349},
		"[2001:db8::1]:3478":      {Host: "2001:db8::1", Port: 3478},
		"[2001:db8::1]":           {Host: "2001:db8::1", Port: 3478},
	} {
		got, err := ParseStunEndpoint(value)
		if err != nil || got != want {
			t.Errorf("%q reads as %+v, %v; want %+v", value, got, err, want)
		}
	}
}

func TestMalformedStunEndpointsAreRefused(t *testing.T) {
	for _, value := range []string{"", "  ", "relay:0", "relay:70000", "relay:stun", ":3478", "a b", "https://relay/"} {
		if got, err := ParseStunEndpoint(value); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q reads as %+v, %v; want refused", value, got, err)
		}
	}
}
