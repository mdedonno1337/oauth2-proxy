package encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SecretBytes attempts to base64 decode the secret, if that fails it treats the secret as binary
func SecretBytes(secret string) []byte {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(secret, "="))
	if err == nil {
		// Only return decoded form if a valid AES length
		// Don't want unintentional decoding resulting in invalid lengths confusing a user
		// that thought they used a 16, 24, 32 length string
		for _, i := range []int{16, 24, 32} {
			if len(b) == i {
				return b
			}
		}
	}
	// If decoding didn't work or resulted in non-AES compliant length,
	// assume the raw string was the intended secret
	return []byte(secret)
}

// cookies are stored in a 3 part (value + timestamp + signature) to enforce that the values are as originally set.
// additionally, the 'value' is encrypted so it's opaque to the browser

// Validate ensures a cookie is properly signed
func Validate(cookie *http.Cookie, seed string, expiration time.Duration) (value []byte, t time.Time, ok bool) {
	// value, timestamp, sig
	parts := strings.Split(cookie.Value, "|")
	if len(parts) != 3 {
		return
	}
	if checkSignature(parts[2], seed, cookie.Name, parts[0], parts[1]) {
		ts, err := strconv.Atoi(parts[1])
		if err != nil {
			return
		}
		// The expiration timestamp set when the cookie was created
		// isn't sent back by the browser. Hence, we check whether the
		// creation timestamp stored in the cookie falls within the
		// window defined by (Now()-expiration, Now()].
		t = time.Unix(int64(ts), 0)
		if t.After(time.Now().Add(expiration*-1)) && t.Before(time.Now().Add(time.Minute*5)) {
			// it's a valid cookie. now get the contents
			rawValue, err := base64.URLEncoding.DecodeString(parts[0])
			if err == nil {
				value = rawValue
				ok = true
				return
			}
		}
	}
	return
}

// SignedValue returns a cookie that is signed and can later be checked with Validate
func SignedValue(seed string, key string, value []byte, now time.Time) string {
	encodedValue := base64.URLEncoding.EncodeToString(value)
	timeStr := fmt.Sprintf("%d", now.Unix())
	sig := cookieSignature(sha256.New, seed, key, encodedValue, timeStr)
	cookieVal := fmt.Sprintf("%s|%s|%s", encodedValue, timeStr, sig)
	return cookieVal
}

func cookieSignature(signer func() hash.Hash, args ...string) string {
	h := hmac.New(signer, []byte(args[0]))
	for _, arg := range args[1:] {
		h.Write([]byte(arg))
	}
	var b []byte
	b = h.Sum(b)
	return base64.URLEncoding.EncodeToString(b)
}

func checkSignature(signature string, args ...string) bool {
	checkSig := cookieSignature(sha256.New, args...)
	if checkHmac(signature, checkSig) {
		return true
	}

	// TODO: After appropriate rollout window, remove support for SHA1
	legacySig := cookieSignature(sha1.New, args...)
	return checkHmac(signature, legacySig)
}

func checkHmac(input, expected string) bool {
	inputMAC, err1 := base64.URLEncoding.DecodeString(input)
	if err1 == nil {
		expectedMAC, err2 := base64.URLEncoding.DecodeString(expected)
		if err2 == nil {
			return hmac.Equal(inputMAC, expectedMAC)
		}
	}
	return false
}

// Cipher provides methods to encrypt and decrypt
type Cipher interface {
	Encrypt(value []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
    EncryptInto(s *string) error
	DecryptInto(s *string) error
}

type DefaultCipher struct {}

// Encrypt is a dummy method for CommonCipher.EncryptInto support
func (c *DefaultCipher) Encrypt(value []byte) ([]byte, error) { return value, nil }

// Decrypt is a dummy method for CommonCipher.DecryptInto support
func (c *DefaultCipher) Decrypt(ciphertext []byte) ([]byte, error) { return ciphertext, nil }

// EncryptInto encrypts the value and stores it back in the string pointer
func (c *DefaultCipher) EncryptInto(s *string) error {
	return into(c.Encrypt, s)
}

// DecryptInto decrypts the value and stores it back in the string pointer
func (c *DefaultCipher) DecryptInto(s *string) error {
	return into(c.Decrypt, s)
}

type Base64Cipher struct {
	DefaultCipher
	Cipher Cipher
}

// NewBase64Cipher returns a new AES Cipher for encrypting cookie values
// and wrapping them in Base64 -- Supports Legacy encryption scheme
func NewBase64Cipher(initCipher func([]byte) (Cipher, error), secret []byte) (Cipher, error) {
	c, err := initCipher(secret)
	if err != nil {
		return nil, err
	}
	return &Base64Cipher{Cipher: c}, nil
}

// Encrypt encrypts a value with AES CFB & base64 encodes it
func (c *Base64Cipher) Encrypt(value []byte) ([]byte, error) {
	encrypted, err := c.Cipher.Encrypt([]byte(value))
	if err != nil {
		return nil, err
	}

	return []byte(base64.StdEncoding.EncodeToString(encrypted)), nil
}

// Decrypt Base64 decodes a value & decrypts it with AES CFB
func (c *Base64Cipher) Decrypt(ciphertext []byte) ([]byte, error) {
	encrypted, err := base64.StdEncoding.DecodeString(string(ciphertext))
	if err != nil {
		return nil, fmt.Errorf("failed to base64 decode value %s", err)
	}

	return c.Cipher.Decrypt(encrypted)
}

type CFBCipher struct {
	DefaultCipher
	cipher.Block
}

// NewCFBCipher returns a new AES CFB Cipher
func NewCFBCipher(secret []byte) (Cipher, error) {
	c, err := aes.NewCipher(secret)
	if err != nil {
		return nil, err
	}
	return &CFBCipher{Block: c}, err
}

// Encrypt with AES CFB
func (c *CFBCipher) Encrypt(value []byte) ([]byte, error) {
	ciphertext := make([]byte, aes.BlockSize+len(value))
	iv := ciphertext[:aes.BlockSize]
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, fmt.Errorf("failed to create initialization vector %s", err)
	}

	stream := cipher.NewCFBEncrypter(c.Block, iv)
	stream.XORKeyStream(ciphertext[aes.BlockSize:], value)
	return ciphertext, nil
}

// Decrypt an AES CFB ciphertext
func (c *CFBCipher) Decrypt(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < aes.BlockSize {
		return nil, fmt.Errorf("encrypted value should be at least %d bytes, but is only %d bytes", aes.BlockSize, len(ciphertext))
	}

	iv := ciphertext[:aes.BlockSize]
	ciphertext = ciphertext[aes.BlockSize:]
	stream := cipher.NewCFBDecrypter(c.Block, iv)
	stream.XORKeyStream(ciphertext, ciphertext)

	return ciphertext, nil
}

type GCMCipher struct {
	DefaultCipher
	cipher.Block
}

// NewGCMCipher returns a new AES GCM Cipher
func NewGCMCipher(secret []byte) (Cipher, error) {
	c, err := aes.NewCipher(secret)
	if err != nil {
		return nil, err
	}
	return &GCMCipher{Block: c}, err
}

// Encrypt with AES GCM on raw bytes
func (c *GCMCipher) Encrypt(value []byte) ([]byte, error) {
	gcm, err := cipher.NewGCM(c.Block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ciphertext := gcm.Seal(nonce, nonce, value, nil)
	return ciphertext, nil
}

// Decrypt an AES GCM ciphertext
func (c *GCMCipher) Decrypt(ciphertext []byte) ([]byte, error) {
	gcm, err := cipher.NewGCM(c.Block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}
	return plaintext, nil
}

// codecFunc is a function that takes a string and encodes/decodes it
type codecFunc func([]byte) ([]byte, error)

func into(f codecFunc, s *string) error {
	// Do not encrypt/decrypt nil or empty strings
	if s == nil || *s == "" {
		return nil
	}

	d, err := f([]byte(*s))
	if err != nil {
		return err
	}
	*s = string(d)
	return nil
}
