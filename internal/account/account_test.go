package account_test

import (
	"encoding/hex"
	"testing"

	"github.com/bitllm-org/bitllm/internal/account"
	"github.com/bitllm-org/bitllm/internal/receipts"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// The well-known Hardhat/Anvil test mnemonic and its first account.
const testMnemonic = "test test test test test test test test test test test junk"
const testAddress = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"

func TestMnemonicDerivation(t *testing.T) {
	k, err := account.KeyFromMnemonic(testMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	if got := account.ChecksumAddress(account.EthAddress(k.PubKey())); got != testAddress {
		t.Fatalf("address %s, want %s", got, testAddress)
	}
	if _, err := account.KeyFromMnemonic("not a mnemonic at all"); err == nil {
		t.Fatal("accepted a bad mnemonic")
	}
}

// A signature made by ethers.js (wallet.signMessage("hello") with the Hardhat
// account 0 key), so our recovery matches what browser wallets produce.
func TestRecoverWalletSignature(t *testing.T) {
	k, _ := account.KeyFromMnemonic(testMnemonic)
	sig := account.EthSign(k, "hello")
	if addr, err := account.EthRecover("hello", sig); err != nil || account.ChecksumAddress(addr) != testAddress {
		t.Fatalf("recover own sig: %s %v", addr, err)
	}
	ext, _ := hex.DecodeString("f16ea9a3478698f695fd1401bfe27e9e4a7e8e3da94aa72b021125e31fa899cc573c48ea3fe1d4ab61a9db10c19032026e3ed2dbccba5a178235ac27f94504311c")
	if addr, err := account.EthRecover("hello", ext); err != nil || account.ChecksumAddress(addr) != testAddress {
		t.Fatalf("recover ethers sig: %s %v", addr, err)
	}
}

func TestClaimNeedsBothSignatures(t *testing.T) {
	owner, _ := account.KeyFromMnemonic(testMnemonic)
	nodeKey, _, _ := crypto.GenerateEd25519Key(nil)
	nodeID, _ := peer.IDFromPrivateKey(nodeKey)
	c, err := account.NewClaim(nodeID, testAddress, "alice")
	if err != nil {
		t.Fatal(err)
	}
	c.OwnerSig = hex.EncodeToString(account.EthSign(owner, c.Message()))
	if c.Valid() {
		t.Fatal("valid without the node's signature")
	}
	if err := account.SignClaimAsNode(c, nodeKey); err != nil {
		t.Fatal(err)
	}
	if !c.Valid() {
		t.Fatal("fully signed claim invalid")
	}
	// Someone else's wallet cannot claim this node.
	thief := *c
	thief.Owner = "0x000000000000000000000000000000000000dead"
	if thief.Valid() {
		t.Fatal("claim with a swapped owner is valid")
	}
	// Renaming invalidates both signatures.
	renamed := *c
	renamed.Name = "mallory"
	if renamed.Valid() {
		t.Fatal("renamed claim is valid")
	}
}

func signedClaim(t *testing.T, owner string, at int64, name string) account.Claim {
	nodeKey, _, _ := crypto.GenerateEd25519Key(nil)
	return signedClaimWith(t, nodeKey, owner, at, name)
}

func signedClaimWith(t *testing.T, nodeKey crypto.PrivKey, owner string, at int64, name string) account.Claim {
	k, _ := account.KeyFromMnemonic(testMnemonic)
	nodeID, _ := peer.IDFromPrivateKey(nodeKey)
	c, err := account.NewClaim(nodeID, owner, name)
	if err != nil {
		t.Fatal(err)
	}
	c.Time = at
	c.OwnerSig = hex.EncodeToString(account.EthSign(k, c.Message()))
	if err := account.SignClaimAsNode(c, nodeKey); err != nil {
		t.Fatal(err)
	}
	return *c
}

func TestClaimsKeepNewestAndReload(t *testing.T) {
	path := t.TempDir() + "/claims.json"
	cs := account.OpenClaims(path)
	nodeKey, _, _ := crypto.GenerateEd25519Key(nil)
	newer := signedClaimWith(t, nodeKey, testAddress, 200, "alice")
	if !cs.Add(newer) {
		t.Fatal("valid claim rejected")
	}
	if _, ok := cs.Get(newer.Node); !ok {
		t.Fatal("claim not stored")
	}
	older := signedClaimWith(t, nodeKey, testAddress, 100, "alice")
	if cs.Add(older) {
		t.Fatal("older claim replaced the newer one")
	}
	// The store survives a reopen, as a node restart would.
	again := account.OpenClaims(path)
	if got, ok := again.Get(newer.Node); !ok || got.Time != newer.Time {
		t.Fatalf("after reopen: %+v %v", got, ok)
	}
	if len(again.All()) != 1 {
		t.Fatalf("stored %d claims, want 1", len(again.All()))
	}
}

func TestOwnerTableGroupsNodes(t *testing.T) {
	cs := account.OpenClaims(t.TempDir() + "/claims.json")
	claimed := signedClaim(t, testAddress, 100, "alice")
	cs.Add(claimed)
	table := account.OwnerTable(map[string]receipts.ServerCredits{
		claimed.Node:     {Credits: 10, Jobs: 2},
		"unclaimed-node": {Credits: 3, Jobs: 1},
	}, cs)
	if len(table) != 2 {
		t.Fatalf("rows = %d, want 2", len(table))
	}
	for _, r := range table {
		switch r.Name {
		case "alice":
			if r.Owner != testAddress || r.Credits != 10 || len(r.Nodes) != 1 {
				t.Fatalf("claimed row: %+v", r)
			}
		default:
			if r.Owner != "" || r.Credits != 3 {
				t.Fatalf("unclaimed row: %+v", r)
			}
		}
	}
}
