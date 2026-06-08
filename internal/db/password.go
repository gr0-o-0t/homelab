package db

import (
	"crypto/rand"
	"math/big"
)

const passwordChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func generatePassword(length int) string {
	buf := make([]byte, length)
	max := big.NewInt(int64(len(passwordChars)))
	for i := range buf {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			// Fallback to a predictable character (should never happen).
			buf[i] = 'a'
			continue
		}
		buf[i] = passwordChars[n.Int64()]
	}
	return string(buf)
}
