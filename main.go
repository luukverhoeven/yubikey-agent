// Copyright 2019 Google LLC
//
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file or at
// https://developers.google.com/open-source/licenses/bsd

package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/go-piv/piv-go/piv"
	"github.com/gopasspw/gopass/pkg/pinentry"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func main() {
	var defaultPath string
	if cacheDir, err := os.UserCacheDir(); err == nil {
		defaultPath = filepath.Join(cacheDir, "yubikey-agent.sock")
	}
	socketPath := flag.String("l", defaultPath, "path of the UNIX socket to listen on")
	flag.Parse()

	a := &Agent{}

	os.Remove(*socketPath)
	l, err := net.Listen("unix", *socketPath)
	if err != nil {
		log.Fatalln("Failed to listen on UNIX socket:", err)
	}
	fmt.Printf("export SSH_AUTH_SOCK=%q\n", *socketPath)

	for {
		c, err := l.Accept()
		if err != nil {
			type temporary interface {
				Temporary() bool
			}
			if err, ok := err.(temporary); ok && err.Temporary() {
				log.Println("Temporary Accept error, sleeping 1s:", err)
				time.Sleep(1 * time.Second)
				continue
			}
			log.Fatalln("Failed to accept connections:", err)
		}
		go a.serveConn(c)
	}
}

type Agent struct {
	yk *piv.YubiKey
}

var _ agent.ExtendedAgent = &Agent{}

func (a *Agent) serveConn(c net.Conn) {
	if err := agent.ServeAgent(a, c); err != io.EOF {
		log.Println("Connection ended with error:", err)
	}
}

func healthy(yk *piv.YubiKey) bool {
	_, err := yk.Serial()
	return err == nil
}

func (a *Agent) ensureYK() error {
	if a.yk == nil || !healthy(a.yk) {
		if a.yk != nil {
			a.yk.Close()
		}
		yk, err := a.connectToYK()
		if err != nil {
			return err
		}
		a.yk = yk
	}
	return nil
}

func (a *Agent) connectToYK() (*piv.YubiKey, error) {
	cards, err := piv.Cards()
	if err != nil {
		return nil, err
	}
	if len(cards) == 0 {
		return nil, errors.New("no YubiKey detected")
	}
	// TODO: support multiple YubiKeys.
	return piv.Open(cards[0])
}

func (a *Agent) getPIN() (string, error) {
	p, err := pinentry.New()
	if err != nil {
		return "", fmt.Errorf("failed to start %q: %w", pinentry.GetBinary(), err)
	}
	defer p.Close()
	p.Set("title", "yubikey-agent PIN Prompt")
	serial, _ := a.yk.Serial()
	p.Set("desc", fmt.Sprintf("YubiKey serial number: %d", serial))
	p.Set("prompt", "Please enter your PIN:")
	pin, err := p.GetPin()
	return string(pin), err
}

var ErrOperationUnsupported = errors.New("operation unsupported")

func (a *Agent) List() ([]*agent.Key, error) {
	if err := a.ensureYK(); err != nil {
		return nil, fmt.Errorf("could not reach YubiKey: %w", err)
	}
	pk, err := getPublicKey(a.yk, piv.SlotAuthentication)
	if err != nil {
		return nil, err
	}
	serial, _ := a.yk.Serial()
	return []*agent.Key{{
		Format:  pk.Type(),
		Blob:    pk.Marshal(),
		Comment: fmt.Sprintf("YubiKey #%d PIV Slot 9a", serial),
	}}, nil
}

func getPublicKey(yk *piv.YubiKey, slot piv.Slot) (ssh.PublicKey, error) {
	cert, err := yk.Attest(slot)
	if err != nil {
		return nil, fmt.Errorf("could not get public key: %w", err)
	}
	pubKey, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("unexpected public key type: %T", cert.PublicKey)
	}
	pk, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return nil, fmt.Errorf("failed to process public key: %w", err)
	}
	return pk, nil
}

func (a *Agent) Signers() ([]ssh.Signer, error) {
	if err := a.ensureYK(); err != nil {
		return nil, fmt.Errorf("could not reach YubiKey: %w", err)
	}
	pk, err := getPublicKey(a.yk, piv.SlotAuthentication)
	if err != nil {
		return nil, err
	}
	priv, err := a.yk.PrivateKey(
		piv.SlotAuthentication,
		pk.(ssh.CryptoPublicKey).CryptoPublicKey(),
		piv.KeyAuth{PINPrompt: a.getPIN},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare private key: %w", err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare signer: %w", err)
	}
	return []ssh.Signer{s}, nil
}

func (a *Agent) SignWithFlags(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	signers, err := a.Signers()
	if err != nil {
		return nil, err
	}
	for _, s := range signers {
		if !bytes.Equal(s.PublicKey().Marshal(), key.Marshal()) {
			continue
		}
		alg := ssh.SigAlgoRSA
		switch {
		case flags&agent.SignatureFlagRsaSha256 != 0:
			alg = ssh.SigAlgoRSASHA2256
		case flags&agent.SignatureFlagRsaSha512 != 0:
			alg = ssh.SigAlgoRSASHA2512
		}
		// TODO: the PIN is asked every time even if the policy is "once".
		// This is an upstream issue: https://github.com/go-piv/piv-go/issues/35
		// TODO: maybe retry if the PIN is not correct?
		return s.(ssh.AlgorithmSigner).SignWithAlgorithm(rand.Reader, data, alg)
	}
	return nil, fmt.Errorf("no private keys match the requested public key")
}

func (a *Agent) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	return a.SignWithFlags(key, data, 0)
}

func (a *Agent) Extension(extensionType string, contents []byte) ([]byte, error) {
	return nil, agent.ErrExtensionUnsupported
}
func (a *Agent) Add(key agent.AddedKey) error {
	return ErrOperationUnsupported
}
func (a *Agent) Remove(key ssh.PublicKey) error {
	return ErrOperationUnsupported
}
func (a *Agent) RemoveAll() error {
	return ErrOperationUnsupported
}
func (a *Agent) Lock(passphrase []byte) error {
	return ErrOperationUnsupported
}
func (a *Agent) Unlock(passphrase []byte) error {
	return ErrOperationUnsupported
}
