package account

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/tyler-smith/go-bip39"
	"golang.org/x/crypto/sha3"
)

// Accounts are Ethereum addresses. Only the signature scheme is borrowed
// (secp256k1, personal_sign, BIP39/BIP44 mnemonics): nothing here talks to a
// chain. An owner claims a node with a statement signed by both the owner's
// wallet and the node's own key, so nobody can claim someone else's node.
// Credits are then summed per owner across all their machines.

func keccak(b ...[]byte) []byte {
	h := sha3.NewLegacyKeccak256()
	for _, x := range b {
		h.Write(x)
	}
	return h.Sum(nil)
}

func EthAddress(pub *secp256k1.PublicKey) string {
	return "0x" + hex.EncodeToString(keccak(pub.SerializeUncompressed()[1:])[12:])
}

// ChecksumAddress renders an address in EIP-55 mixed case, for display.
func ChecksumAddress(addr string) string {
	a := strings.ToLower(strings.TrimPrefix(addr, "0x"))
	h := hex.EncodeToString(keccak([]byte(a)))
	out := []byte(a)
	for i, c := range out {
		if c >= 'a' && h[i] >= '8' {
			out[i] = c - 32
		}
	}
	return "0x" + string(out)
}

func personalHash(msg string) []byte {
	return keccak([]byte("\x19Ethereum Signed Message:\n" + strconv.Itoa(len(msg)) + msg))
}

// EthSign returns a 65-byte r||s||v signature, as wallets produce for personal_sign.
func EthSign(key *secp256k1.PrivateKey, msg string) []byte {
	c := ecdsa.SignCompact(key, personalHash(msg), false) // [27+recid][r][s]
	return append(c[1:], c[0])
}

// EthRecover returns the lowercase address that signed msg.
func EthRecover(msg string, sig []byte) (string, error) {
	if len(sig) != 65 {
		return "", fmt.Errorf("signature must be 65 bytes")
	}
	v := sig[64]
	if v < 27 {
		v += 27
	}
	pub, _, err := ecdsa.RecoverCompact(append([]byte{v}, sig[:64]...), personalHash(msg))
	if err != nil {
		return "", err
	}
	return EthAddress(pub), nil
}

// KeyFromMnemonic derives the first account of a standard Ethereum wallet, m/44'/60'/0'/0/0.
func KeyFromMnemonic(mnemonic string) (*secp256k1.PrivateKey, error) {
	mnemonic = strings.Join(strings.Fields(mnemonic), " ")
	if !bip39.IsMnemonicValid(mnemonic) {
		return nil, fmt.Errorf("not a valid BIP39 mnemonic")
	}
	mac := hmac.New(sha512.New, []byte("Bitcoin seed"))
	mac.Write(bip39.NewSeed(mnemonic, ""))
	I := mac.Sum(nil)
	k, c := I[:32], I[32:]
	for _, i := range []uint32{44 | 1<<31, 60 | 1<<31, 0 | 1<<31, 0, 0} {
		var data []byte
		if i >= 1<<31 {
			data = append([]byte{0}, k...)
		} else {
			data = secp256k1.PrivKeyFromBytes(k).PubKey().SerializeCompressed()
		}
		data = binary.BigEndian.AppendUint32(data, i)
		mac := hmac.New(sha512.New, c)
		mac.Write(data)
		I := mac.Sum(nil)
		var il, kk secp256k1.ModNScalar
		if il.SetByteSlice(I[:32]) {
			return nil, fmt.Errorf("invalid derivation, try another mnemonic")
		}
		kk.SetByteSlice(k)
		kk.Add(&il)
		b := kk.Bytes()
		k, c = b[:], I[32:]
	}
	return secp256k1.PrivKeyFromBytes(k), nil
}
