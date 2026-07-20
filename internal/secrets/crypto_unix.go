//go:build !windows

package secrets

func encrypt(plain []byte) ([]byte, error) {
	return plain, nil
}

func decrypt(cipher []byte) ([]byte, error) {
	return cipher, nil
}
