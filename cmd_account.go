package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bitllm-network/bitllm/internal/account"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/tyler-smith/go-bip39"
)

const accountUsage = `bitllm account: tie this node to an Ethereum address so its credits add up
with your other machines under one account. Only the signature is used; nothing
touches a chain.

  bitllm account               show which account owns this node
  bitllm account new  [--name N]   create a new account (12 words) and link this node
  bitllm account link [--name N]   link this node to an existing account (asks for the words)

In the dashboard (http://127.0.0.1:7778) you can link with a browser wallet instead.
`

func accountCmd(args []string, home string) {
	fs := flag.NewFlagSet("account", flag.ExitOnError)
	data := fs.String("data", filepath.Join(home, ".bitllm"), "identity and ledger directory")
	gateway := fs.String("gateway", "127.0.0.1:7778", "gateway of the running node, if any")
	name := fs.String("name", "", "display name shown in the credits table (max 32 bytes)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, accountUsage) }
	sub := "show"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs.Parse(args)

	os.MkdirAll(*data, 0o700)
	nodeKey, err := loadKey(filepath.Join(*data, "identity.key"))
	if err != nil {
		fail(err)
	}
	self, _ := peer.IDFromPrivateKey(nodeKey)
	claims := account.OpenClaims(filepath.Join(*data, "claims.json"))

	var key *secp256k1.PrivateKey
	switch sub {
	case "show":
		if c, ok := claims.Get(self.String()); ok {
			fmt.Printf("node %s\nowner %s\nname %q\nlinked %s\n", self, account.ChecksumAddress(c.Owner), c.Name, time.Unix(c.Time, 0).Format(time.RFC3339))
		} else {
			fmt.Printf("node %s is not linked to an account.\n\n%s", self, accountUsage)
		}
		return
	case "new":
		entropy, _ := bip39.NewEntropy(128)
		words, _ := bip39.NewMnemonic(entropy)
		key, _ = account.KeyFromMnemonic(words)
		fmt.Printf("Your account: %s\n\nRecovery words (write them down; they are shown once and are the only way\nto link other machines or prove this account later):\n\n    %s\n\n", account.ChecksumAddress(account.EthAddress(key.PubKey())), words)
		fmt.Println("They also work in any Ethereum wallet (MetaMask, Rabby...), first account.")
	case "link":
		fmt.Print("Recovery words: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if key, err = account.KeyFromMnemonic(line); err != nil {
			fail(err)
		}
	default:
		fs.Usage()
		os.Exit(2)
	}

	c, err := account.NewClaim(self, account.EthAddress(key.PubKey()), *name)
	if err != nil {
		fail(err)
	}
	c.OwnerSig = hex.EncodeToString(account.EthSign(key, c.Message()))

	// Prefer the running node, which also gossips the claim right away.
	body, _ := json.Marshal(map[string]any{"owner": c.Owner, "name": c.Name, "time": c.Time, "owner_sig": c.OwnerSig})
	if resp, err := http.Post("http://"+*gateway+"/account/link", "application/json", bytes.NewReader(body)); err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			var e struct{ Error struct{ Message string } }
			json.NewDecoder(resp.Body).Decode(&e)
			fail(fmt.Errorf("node refused: %s", e.Error.Message))
		}
		fmt.Printf("Linked node %s to %s.\n", self, account.ChecksumAddress(c.Owner))
		return
	}
	if err := account.SignClaimAsNode(c, nodeKey); err != nil {
		fail(err)
	}
	if !claims.Add(*c) {
		fail(fmt.Errorf("claim rejected"))
	}
	fmt.Printf("Linked node %s to %s. It is announced when the node starts.\n", self, account.ChecksumAddress(c.Owner))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

const inviteUsage = `bitllm invite: let another account into the network.

  bitllm invite 0xADDRESS            invite an account (asks for your recovery words)
  bitllm invite --revoke 0xADDRESS   remove an account (roots only)

Members invite up to 3 accounts, contributors (served a job in the last 7 days)
up to 10. Needs this machine's node running, which gossips the signed invite.
`

func inviteCmd(args []string) {
	fs := flag.NewFlagSet("invite", flag.ExitOnError)
	gateway := fs.String("gateway", "127.0.0.1:7778", "gateway of the running node")
	revoke := fs.Bool("revoke", false, "revoke the account instead (roots only)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, inviteUsage) }
	var addr string
	var rest []string
	for _, a := range args {
		if strings.HasPrefix(a, "0x") || strings.HasPrefix(a, "0X") {
			addr = strings.ToLower(a)
		} else {
			rest = append(rest, a)
		}
	}
	fs.Parse(rest)
	if !validAddr(addr) {
		fs.Usage()
		os.Exit(2)
	}
	fmt.Print("Your recovery words: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	key, err := account.KeyFromMnemonic(line)
	if err != nil {
		fail(err)
	}
	me := account.EthAddress(key.PubKey())
	var it memberItem
	if *revoke {
		r := &Revoke{Account: addr, Root: me, Time: time.Now().Unix()}
		r.Sig = hex.EncodeToString(account.EthSign(key, r.Message()))
		it.Revoke = r
	} else {
		i := &Invite{Inviter: me, Invitee: addr, Time: time.Now().Unix()}
		i.Sig = hex.EncodeToString(account.EthSign(key, i.Message()))
		it.Invite = i
	}
	body, _ := json.Marshal(it)
	resp, err := http.Post("http://"+*gateway+"/members", "application/json", bytes.NewReader(body))
	if err != nil {
		fail(fmt.Errorf("no node running at %s; start bitllm first", *gateway))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var e struct{ Error struct{ Message string } }
		json.NewDecoder(resp.Body).Decode(&e)
		fail(fmt.Errorf("node refused: %s", e.Error.Message))
	}
	if *revoke {
		fmt.Printf("Revoked %s.\n", account.ChecksumAddress(addr))
	} else {
		fmt.Printf("Invited %s from %s. It counts once the network sees it (seconds).\n", account.ChecksumAddress(addr), account.ChecksumAddress(me))
	}
}
