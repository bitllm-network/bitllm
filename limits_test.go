package main

import (
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

func randomPeer(t *testing.T) peer.ID {
	k, _, _ := crypto.GenerateEd25519Key(nil)
	p, err := peer.IDFromPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOriginRate(t *testing.T) {
	o := originRate{every: time.Second}
	p := randomPeer(t)
	now := time.Now()
	if !o.allow(p, now) || o.allow(p, now.Add(500*time.Millisecond)) || !o.allow(p, now.Add(time.Second)) {
		t.Fatal("origin rate wrong")
	}
}
