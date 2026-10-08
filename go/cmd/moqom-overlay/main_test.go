package main

import (
	"io"
	"net"
	"strings"
	"testing"
)

func TestRunRefusesToListenBeyondLoopback(t *testing.T) {
	for _, address := range []string{"0.0.0.0:0", "192.0.2.1:0", ":0"} {
		err := run("id.json", "svc", "", address)
		if err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("%s: got %v, want a loopback refusal", address, err)
		}
	}
	if err := run("", "svc", "", "127.0.0.1:0"); err == nil {
		t.Error("ran with no identity")
	}
}

func TestPipeCopiesBothWaysAndClosesBoth(t *testing.T) {
	clientA, serverA := net.Pipe()
	clientB, serverB := net.Pipe()
	done := make(chan struct{})
	go func() { pipe(serverA, serverB); close(done) }()

	go func() { _, _ = clientA.Write([]byte("ping")) }()
	got := make([]byte, 4)
	if _, err := io.ReadFull(clientB, got); err != nil || string(got) != "ping" {
		t.Fatalf("read %q (%v)", got, err)
	}
	go func() { _, _ = clientB.Write([]byte("pong")) }()
	if _, err := io.ReadFull(clientA, got); err != nil || string(got) != "pong" {
		t.Fatalf("read %q (%v)", got, err)
	}
	_ = clientA.Close()
	<-done
	if _, err := clientB.Read(got); err == nil {
		t.Error("the far side stayed open after the near side closed")
	}
}
