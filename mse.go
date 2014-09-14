// Package mse (Message Stream Encryption) provides a transparent wrapper for bidirectional
// data streams (e.g. TCP transports) that prevents passive eavesdroping
// and thus protocol or content identification.
//
// It is also designed to provide limited protection against active MITM attacks
// and portscanning by requiring a weak shared secret to complete the handshake.
// You should note that the major design goal was payload and protocol obfuscation,
// not peer authentication and data integrity verification. Thus it does not offer
// protection against adversaries which already know the necessary data to establish
// connections (that is IP/Port/Shared Secret/Payload protocol).
//
// To minimize the load on systems that employ this protocol fast cryptographic
// methods have been chosen over maximum-security algorithms.
//
// See http://wiki.vuze.com/w/Message_Stream_Encryption for details.
package mse

import (
	"bytes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rc4"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
)

const (
	pString = "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A63A36210000000000090563"
)

var (
	p    = new(big.Int) // TODO precalculate bytes
	g    = big.NewInt(2)
	vc   = make([]byte, 8)
	sKey = make([]byte, 20) // TODO take it as argument
)

func init() {
	b, err := hex.DecodeString(pString)
	if err != nil {
		panic(err)
	}
	p.SetBytes(b)
}

type CryptoMethod uint32

// Crypto methods
const (
	PlainText CryptoMethod = 1 << iota
	RC4
)

type Stream struct {
	raw io.ReadWriter
	r   io.Reader
	w   io.Writer
}

func NewStream(rw io.ReadWriter) *Stream { return &Stream{raw: rw} }

func (s *Stream) Read(p []byte) (n int, err error)  { return s.r.Read(p) }
func (s *Stream) Write(p []byte) (n int, err error) { return s.w.Write(p) }

func (s *Stream) HandshakeOutgoing(cryptoProvide CryptoMethod) (selected CryptoMethod, err error) {
	writeBuf := bytes.NewBuffer(make([]byte, 0, 96+512))

	Xa, Ya, err := keyPair()
	if err != nil {
		return
	}

	// Step 1 | A->B: Diffie Hellman Ya, PadA
	err = writePubKey(writeBuf, Ya)
	if err != nil {
		return
	}
	padA, err := pad()
	if err != nil {
		return
	}
	_, err = writeBuf.Write(padA)
	if err != nil {
		return
	}
	fmt.Println("--- out: writing Step 1")
	_, err = writeBuf.WriteTo(s.raw)
	if err != nil {
		return
	}
	fmt.Println("--- out: done")

	// Step 2 | B->A: Diffie Hellman Yb, PadB
	b := make([]byte, 96+512)
	fmt.Println("--- out: reading PubkeyB")
	_, err = io.ReadAtLeast(s.raw, b, 96)
	if err != nil {
		return
	}
	fmt.Println("--- out: done")
	Yb := new(big.Int)
	Yb.SetBytes(b[:96])
	b = nil
	S := Yb.Exp(Yb, Xa, p)
	cipherEnc, err := rc4.NewCipher(rc4Key("keyA", S, sKey))
	if err != nil {
		return
	}
	cipherDec, err := rc4.NewCipher(rc4Key("keyB", S, sKey))
	if err != nil {
		return
	}
	discard := make([]byte, 1024)
	cipherEnc.XORKeyStream(discard, discard)
	cipherDec.XORKeyStream(discard, discard)
	s.w = &cipher.StreamWriter{S: cipherEnc, W: s.raw}
	s.r = &cipher.StreamReader{S: cipherDec, R: s.raw}

	// Step 3 | A->B: HASH('req1', S), HASH('req2', SKEY) xor HASH('req3', S), ENCRYPT(VC, crypto_provide, len(PadC), PadC, len(IA)), ENCRYPT(IA)
	req1 := hashKey("req1", S)
	req2 := hashBytes("req2", sKey)
	req3 := hashKey("req3", S)
	for i := 0; i < sha1.Size; i++ {
		req3[i] ^= req2[i]
	}
	padC, err := pad()
	if err != nil {
		return
	}
	_, err = writeBuf.Write(req1)
	if err != nil {
		return
	}
	_, err = writeBuf.Write(req3)
	if err != nil {
		return
	}
	_, err = writeBuf.Write(vc)
	if err != nil {
		return
	}
	err = binary.Write(writeBuf, binary.BigEndian, cryptoProvide)
	if err != nil {
		return
	}
	err = binary.Write(writeBuf, binary.BigEndian, uint16(len(padC)))
	if err != nil {
		return
	}
	_, err = writeBuf.Write(padC)
	if err != nil {
		return
	}
	err = binary.Write(writeBuf, binary.BigEndian, uint16(0)) // len(IA) TODO take it as argument
	if err != nil {
		return
	}
	encBytes := writeBuf.Bytes()[40:]
	cipherEnc.XORKeyStream(encBytes, encBytes)
	fmt.Println("--- out: writing Step 3")
	_, err = writeBuf.WriteTo(s.raw)
	if err != nil {
		return
	}
	fmt.Println("--- out: done")

	// Step 4 | B->A: ENCRYPT(VC, crypto_select, len(padD), padD), ENCRYPT2(Payload Stream)
	vcRead := make([]byte, 8)
	fmt.Println("--- out: read sync")
	_, err = io.ReadFull(s.r, vcRead)
	if err != nil {
		return
	}
	fmt.Println("--- out: done")
	if !bytes.Equal(vcRead, vc) {
		err = errors.New("invalid VC")
		return
	}
	err = binary.Read(s.r, binary.BigEndian, &selected)
	if err != nil {
		return
	}
	fmt.Printf("--- selected: %#v\n", selected)
	if !isPowerOfTwo(uint32(selected)) {
		err = fmt.Errorf("invalid crypto selected: %d", selected)
		return
	}
	if (selected & cryptoProvide) == 0 {
		err = fmt.Errorf("selected crypto is not provided: %d", selected)
		return
	}
	var lenPadD uint16
	err = binary.Read(s.r, binary.BigEndian, &lenPadD)
	if err != nil {
		return
	}
	fmt.Printf("--- lenPadD: %#v\n", lenPadD)
	if lenPadD > 0 {
		_, err = io.CopyN(ioutil.Discard, s.r, int64(lenPadD))
		if err != nil {
			return
		}
	}

	return selected, nil

	// Step 5 | A->B: ENCRYPT2(Payload Stream)
}

func (s *Stream) HandshakeIncoming(cryptoSelect func(cryptoProvide CryptoMethod) (CryptoMethod, error)) error {
	writeBuf := bytes.NewBuffer(make([]byte, 0, 96+512))

	Xb, Yb, err := keyPair()
	if err != nil {
		return err
	}

	// Step 1 | A->B: Diffie Hellman Ya, PadA
	b := make([]byte, 96+512)
	fmt.Println("--- in: read PubkeyA")
	_, err = io.ReadAtLeast(s.raw, b, 96)
	if err != nil {
		return err
	}
	fmt.Println("--- in: done")
	Ya := new(big.Int)
	Ya.SetBytes(b[:96])
	b = nil
	S := Ya.Exp(Ya, Xb, p)
	cipherEnc, err := rc4.NewCipher(rc4Key("keyB", S, sKey))
	if err != nil {
		return err
	}
	cipherDec, err := rc4.NewCipher(rc4Key("keyA", S, sKey))
	if err != nil {
		return err
	}
	discard := make([]byte, 1024)
	cipherEnc.XORKeyStream(discard, discard)
	cipherDec.XORKeyStream(discard, discard)
	s.w = &cipher.StreamWriter{S: cipherEnc, W: s.raw}
	s.r = &cipher.StreamReader{S: cipherDec, R: s.raw}

	// Step 2 | B->A: Diffie Hellman Yb, PadB
	err = writePubKey(writeBuf, Yb)
	if err != nil {
		return err
	}
	padB, err := pad()
	if err != nil {
		return err
	}
	_, err = writeBuf.Write(padB)
	if err != nil {
		return err
	}
	fmt.Println("--- in: writing Step 2")
	_, err = writeBuf.WriteTo(s.raw)
	if err != nil {
		return err
	}
	fmt.Println("--- in: done")

	// Step 3 | A->B: HASH('req1', S), HASH('req2', SKEY) xor HASH('req3', S), ENCRYPT(VC, crypto_provide, len(PadC), PadC, len(IA)), ENCRYPT(IA)
	hash1Calc := hashKey("req1", S)
	hash2Calc := hashBytes("req2", sKey)
	hash3Calc := hashKey("req3", S)
	for i := 0; i < sha1.Size; i++ {
		hash3Calc[i] ^= hash2Calc[i]
	}
	hashRead := make([]byte, 20)
	_, err = io.ReadFull(s.raw, hashRead)
	if err != nil {
		return err
	}
	if !bytes.Equal(hashRead, hash1Calc) {
		err = errors.New("invalid S hash")
		return err
	}
	_, err = io.ReadFull(s.raw, hashRead)
	if err != nil {
		return err
	}
	if !bytes.Equal(hashRead, hash3Calc) {
		err = errors.New("invalid SKEY hash")
		return err
	}
	vcRead := make([]byte, 8)
	fmt.Println("--- in: read vc")
	_, err = io.ReadFull(s.r, vcRead)
	if err != nil {
		return err
	}
	fmt.Println("--- in: done")
	if !bytes.Equal(vcRead, vc) {
		return fmt.Errorf("invalid VC: %s", hex.EncodeToString(vcRead))
	}
	var cryptoProvide CryptoMethod
	err = binary.Read(s.r, binary.BigEndian, &cryptoProvide)
	if err != nil {
		return err
	}
	selected, err := cryptoSelect(cryptoProvide)
	if err != nil {
		return err
	}
	var lenPadC uint16
	err = binary.Read(s.r, binary.BigEndian, &lenPadC)
	if err != nil {
		return err
	}
	_, err = io.CopyN(ioutil.Discard, s.r, int64(lenPadC))
	if err != nil {
		return err
	}
	var lenIA uint16
	err = binary.Read(s.r, binary.BigEndian, &lenIA)
	if err != nil {
		return err
	}

	// Step 4 | B->A: ENCRYPT(VC, crypto_select, len(padD), padD), ENCRYPT2(Payload Stream)
	fmt.Println("--- in: begin step 4")
	_, err = writeBuf.Write(vc)
	if err != nil {
		return err
	}
	err = binary.Write(writeBuf, binary.BigEndian, selected)
	if err != nil {
		return err
	}
	padD, err := pad()
	if err != nil {
		return err
	}
	err = binary.Write(writeBuf, binary.BigEndian, uint16(len(padD)))
	if err != nil {
		return err
	}
	_, err = writeBuf.Write(padD)
	if err != nil {
		return err
	}
	fmt.Println("--- in: writing step 4")
	_, err = writeBuf.WriteTo(s.w)
	if err != nil {
		return err
	}
	fmt.Println("--- in: done")

	// Step 5 | A->B: ENCRYPT2(Payload Stream)
	return nil
}

func privateKey() (*big.Int, error) {
	b := make([]byte, 20)
	_, err := rand.Read(b)
	if err != nil {
		return nil, err
	}
	var n big.Int
	return n.SetBytes(b), nil
}

func publicKey(private *big.Int) *big.Int {
	var n big.Int
	return n.Exp(g, private, p)
}

func keyPair() (private, public *big.Int, err error) {
	private, err = privateKey()
	if err != nil {
		return
	}
	public = publicKey(private)
	return
}

func writePubKey(buf *bytes.Buffer, key *big.Int) error {
	b := key.Bytes() // big-endian
	pad := 96 - len(b)
	fmt.Printf("--- pad: %d\n", pad)
	_, err := buf.Write(make([]byte, pad))
	if err != nil {
		return err
	}
	_, err = buf.Write(b)
	return err
}

func keyBytesWithPad(key *big.Int) []byte {
	b := key.Bytes()
	pad := 96 - len(b)
	if pad > 0 {
		b = make([]byte, 96)
		copy(b[pad:], key.Bytes())
	}
	return b
}

func isPowerOfTwo(x uint32) bool { return (x != 0) && ((x & (x - 1)) == 0) }

func hashKey(prefix string, key *big.Int) []byte {
	h := sha1.New()
	h.Write([]byte(prefix))
	h.Write(keyBytesWithPad(key))
	return h.Sum(nil)
}

func hashBytes(prefix string, key []byte) []byte {
	h := sha1.New()
	h.Write([]byte(prefix))
	h.Write(key)
	return h.Sum(nil)
}

func rc4Key(prefix string, S *big.Int, sKey []byte) []byte {
	h := sha1.New()
	h.Write([]byte(prefix))
	h.Write(keyBytesWithPad(S))
	h.Write(sKey)
	return h.Sum(nil)
}

func pad() ([]byte, error) {
	padLen, err := rand.Int(rand.Reader, big.NewInt(512))
	if err != nil {
		return nil, err
	}
	b := make([]byte, int(padLen.Int64()))
	_, err = rand.Read(b)
	if err != nil {
		return nil, err
	}
	return b, nil
}
