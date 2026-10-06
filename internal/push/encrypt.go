// Package push sends encrypted Web Push directly to browser vendors.
package push

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
)

var raw = base64.RawURLEncoding

type Subscription struct {
	Endpoint string `json:"endpoint"`
	Origin   string `json:"origin,omitempty"`
	Keys     struct {
		P256DH string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	DeviceID string `json:"device_id,omitempty"`
}

func (s Subscription) Validate() error { _, _, err := subscriptionKeys(s); return err }
func subscriptionKeys(s Subscription) (*ecdh.PublicKey, []byte, error) {
	b, err := raw.DecodeString(s.Keys.P256DH)
	if err != nil {
		return nil, nil, errors.New("invalid push key")
	}
	key, err := ecdh.P256().NewPublicKey(b)
	if err != nil {
		return nil, nil, errors.New("invalid push key")
	}
	auth, err := raw.DecodeString(s.Keys.Auth)
	if err != nil || len(auth) != 16 {
		return nil, nil, errors.New("invalid push secret")
	}
	return key, auth, nil
}

// Encrypt implements RFC 8291, with a fresh ephemeral key and salt per message.
func Encrypt(sub Subscription, plaintext []byte) ([]byte, error) {
	eph, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	return encryptWith(eph, salt, sub, plaintext)
}

type derived struct{ shared, prkKey, info, ikm, prk, cek, nonce []byte }

func derive(eph *ecdh.PrivateKey, salt []byte, sub Subscription) (d derived, err error) {
	ua, auth, err := subscriptionKeys(sub)
	if err != nil {
		return d, err
	}
	d.shared, err = eph.ECDH(ua)
	if err != nil {
		return d, err
	}
	d.prkKey, err = hkdf.Extract(sha256.New, d.shared, auth)
	if err != nil {
		return d, err
	}
	d.info = append(append([]byte("WebPush: info\x00"), ua.Bytes()...), eph.PublicKey().Bytes()...)
	d.ikm, err = hkdf.Expand(sha256.New, d.prkKey, string(d.info), 32)
	if err != nil {
		return d, err
	}
	d.prk, err = hkdf.Extract(sha256.New, d.ikm, salt)
	if err != nil {
		return d, err
	}
	d.cek, err = hkdf.Expand(sha256.New, d.prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return d, err
	}
	d.nonce, err = hkdf.Expand(sha256.New, d.prk, "Content-Encoding: nonce\x00", 12)
	return d, err
}
func encryptWith(eph *ecdh.PrivateKey, salt []byte, sub Subscription, pt []byte) ([]byte, error) {
	if len(salt) != 16 || len(pt) > 3993 {
		return nil, errors.New("invalid push message size")
	}
	d, err := derive(eph, salt, sub)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(d.cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	header := append([]byte{}, salt...)
	header = binary.BigEndian.AppendUint32(header, 4096)
	header = append(header, 65)
	header = append(header, eph.PublicKey().Bytes()...)
	return gcm.Seal(header, d.nonce, append(append([]byte{}, pt...), 2), nil), nil
}
