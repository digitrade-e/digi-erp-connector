//go:build windows

package secrets

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

func bytesToBlob(b []byte) windows.DataBlob {
	if len(b) == 0 {
		return windows.DataBlob{}
	}

	return windows.DataBlob{
		Size: uint32(len(b)),
		Data: &b[0],
	}
}

func blobToBytes(b windows.DataBlob) []byte {
	if b.Size == 0 || b.Data == nil {
		return nil
	}

	out := make([]byte, b.Size)
	copy(out, unsafe.Slice(b.Data, b.Size))
	return out
}

func encrypt(plain []byte) ([]byte, error) {
	in := bytesToBlob(plain)
	var out windows.DataBlob

	errP := windows.CryptProtectData(
		&in,
		nil,
		nil,
		0,
		nil,
		windows.CRYPTPROTECT_LOCAL_MACHINE,
		&out,
	)

	if errP != nil {
		return nil, errP
	}

	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))

	return blobToBytes(out), nil

}

func decrypt(cipher []byte) ([]byte, error) {
	in := bytesToBlob(cipher)
	var out windows.DataBlob

	errD := windows.CryptUnprotectData(
		&in,
		nil,
		nil,
		0,
		nil,
		0,
		&out,
	)

	if errD != nil {
		return nil, errD
	}

	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))

	return blobToBytes(out), nil
}
