package egress

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// writeBE writes a uint16 in big-endian to w, panicking on error. A bytes.
// Buffer never errors, so this keeps errcheck satisfied without noise.
func writeBE(w *bytes.Buffer, v uint16) {
	if err := binary.Write(w, binary.BigEndian, v); err != nil {
		panic(err)
	}
}

// buildClientHello constructs a minimal TLS 1.2 ClientHello with the given SNI
// (server_name extension 0x0000). An empty SNI builds a ClientHello with no
// server_name extension (the ECH / no-SNI case). The bytes start with the TLS
// record header (type 0x16) so the proxy's "is this a TLS ClientHello" sniff
// recognises it.
func buildClientHello(sni string) []byte {
	extName := []byte(sni)
	// server_name extension: type(0x0000) + len + server_name_list_len(2) +
	// name_type(1) + name_len(2) + name.
	var ext bytes.Buffer
	ext.Write([]byte{0x00, 0x00}) // extension type: server_name
	var extBody bytes.Buffer
	if len(extName) > 0 {
		listLen := 1 + 2 + len(extName) // name_type + name_len field + name
		writeBE(&extBody, uint16(listLen))
		extBody.Write([]byte{0x00}) // name type: host_name
		writeBE(&extBody, uint16(len(extName)))
		extBody.Write(extName)
	}
	writeBE(&ext, uint16(extBody.Len()))
	ext.Write(extBody.Bytes())

	// ClientHello body: client_version(2) + random(32) + session_id_len(1)+0 +
	// cipher_suites(2) + compression(1)+1 + extensions(2+ext)
	var hello bytes.Buffer
	hello.Write([]byte{0x03, 0x03})           // client_version TLS 1.2
	hello.Write(make([]byte, 32))             // random
	hello.Write([]byte{0x00})                 // session_id length 0
	ciphers := []byte{0x13, 0x01, 0x13, 0x02} // two ciphers
	writeBE(&hello, uint16(len(ciphers)))
	hello.Write(ciphers)
	hello.Write([]byte{0x01, 0x00}) // compression: 1 method, null
	writeBE(&hello, uint16(ext.Len()))
	hello.Write(ext.Bytes())

	// Handshake header: type(0x01 ClientHello) + 3-byte length
	var hs bytes.Buffer
	hs.Write([]byte{0x01})
	var lenB [3]byte
	l := hello.Len()
	lenB[0] = byte(l >> 16)
	lenB[1] = byte(l >> 8)
	lenB[2] = byte(l)
	hs.Write(lenB[:])
	hs.Write(hello.Bytes())

	// TLS record: type 0x16 (handshake), version 0x0301, 2-byte length
	var rec bytes.Buffer
	rec.Write([]byte{0x16, 0x03, 0x01})
	writeBE(&rec, uint16(hs.Len()))
	rec.Write(hs.Bytes())
	return rec.Bytes()
}

func TestExtractSNI(t *testing.T) {
	tests := []struct {
		name    string
		input   []byte
		wantSNI string
		wantOK  bool
	}{
		{"normal SNI", buildClientHello(testAllowHost), testAllowHost, true},
		{"SNI with dots and port-free host", buildClientHello(testAllowHost2), testAllowHost2, true},
		{"no SNI (ECH / omitted) -> absent", buildClientHello(""), "", false},
		{"not a TLS record (plain HTTP) -> not found", []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"), "", false},
		{"random bytes -> not found", []byte{0x01, 0x02, 0x03}, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ExtractSNI(tc.input)
			if ok != tc.wantOK || got != tc.wantSNI {
				t.Fatalf("ExtractSNI = (%q, %v); want (%q, %v)", got, ok, tc.wantSNI, tc.wantOK)
			}
		})
	}
}

// A real-world ClientHello must also round-trip: the standard TLS record with
// the server_name extension is recognised even when there are other
// extensions before/after it.
func TestExtractSNIOtherExtensionsPresent(t *testing.T) {
	// Build a ClientHello with an unsupported_cipher_list extension (0x0016)
	// BEFORE the server_name extension, to prove the parser walks the
	// extension list rather than assuming server_name is first.
	extName := []byte("example.com")
	var extList bytes.Buffer
	// extension 0x0016 (unsupported_ciphers), empty body
	extList.Write([]byte{0x00, 0x16, 0x00, 0x00})
	// extension 0x0000 (server_name): type(2) + len(2) + body
	var sniBody bytes.Buffer
	listLen := 1 + 2 + len(extName)
	writeBE(&sniBody, uint16(listLen))
	sniBody.Write([]byte{0x00}) // name type: host_name
	writeBE(&sniBody, uint16(len(extName)))
	sniBody.Write(extName)
	extList.Write([]byte{0x00, 0x00}) // extension type: server_name
	writeBE(&extList, uint16(sniBody.Len()))
	extList.Write(sniBody.Bytes())

	var hello bytes.Buffer
	hello.Write([]byte{0x03, 0x03})
	hello.Write(make([]byte, 32))
	hello.Write([]byte{0x00})
	ciphers := []byte{0x13, 0x01}
	writeBE(&hello, uint16(len(ciphers)))
	hello.Write(ciphers)
	hello.Write([]byte{0x01, 0x00})
	writeBE(&hello, uint16(extList.Len()))
	hello.Write(extList.Bytes())

	var hs bytes.Buffer
	hs.Write([]byte{0x01})
	var lenB [3]byte
	l := hello.Len()
	lenB[0], lenB[1], lenB[2] = byte(l>>16), byte(l>>8), byte(l)
	hs.Write(lenB[:])
	hs.Write(hello.Bytes())

	var rec bytes.Buffer
	rec.Write([]byte{0x16, 0x03, 0x01})
	writeBE(&rec, uint16(hs.Len()))
	rec.Write(hs.Bytes())

	got, ok := ExtractSNI(rec.Bytes())
	if !ok || got != "example.com" {
		t.Fatalf("ExtractSNI with other extensions = (%q, %v); want (example.com, true)", got, ok)
	}
}
