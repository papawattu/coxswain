// Package egresstest holds the ClientHello builders used by the SNI tests
// (internal/egress/sni_test.go and cmd/egress-proxy/main_test.go). It is a
// non-test package imported only by tests, so the builders can be shared
// without leaking test scaffolding into the production egress package.
package egresstest

import (
	"bytes"
	"encoding/binary"
)

// writeBE writes a uint16 in big-endian to w, panicking on error. A bytes.
// Buffer never errors, so this keeps errcheck satisfied without noise.
func writeBE(w *bytes.Buffer, v uint16) {
	if err := binary.Write(w, binary.BigEndian, v); err != nil {
		panic(err)
	}
}

// BuildClientHello constructs a minimal TLS 1.2 ClientHello with the given
// SNI (server_name extension 0x0000). An empty SNI builds a ClientHello with
// no server_name extension (the ECH / no-SNI case). The bytes start with the
// TLS record header (type 0x16) so the proxy's "is this a TLS ClientHello"
// sniff recognises it. The resulting hello is well under 512 bytes.
func BuildClientHello(sni string) []byte {
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

// BuildHelloWithPadding builds a ClientHello (like BuildClientHello) but
// inserts a large dummy extension (0x0010 session_id, padLen-4 zero bytes)
// BEFORE the server_name extension, pushing the SNI to a deterministic byte
// offset. With a large enough padLen the SNI lands past byte 512 — the
// "SNI past byte 512" case the old Peek(512) missed.
func BuildHelloWithPadding(sni string, padLen int) []byte {
	extName := []byte(sni)
	var extBody bytes.Buffer
	listLen := 1 + 2 + len(extName)
	writeBE(&extBody, uint16(listLen))
	extBody.Write([]byte{0x00}) // name type: host_name
	writeBE(&extBody, uint16(len(extName)))
	extBody.Write(extName)

	var extList bytes.Buffer
	if padLen > 0 {
		// Dummy extension 0x0010 (session_id) with padLen-4 bytes of 0x00.
		dummy := make([]byte, padLen-4)
		extList.Write([]byte{0x00, 0x10})
		writeBE(&extList, uint16(padLen-4))
		extList.Write(dummy)
	}
	var sniExt bytes.Buffer
	sniExt.Write([]byte{0x00, 0x00})
	writeBE(&sniExt, uint16(extBody.Len()))
	sniExt.Write(extBody.Bytes())
	extList.Write(sniExt.Bytes())

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
	return rec.Bytes()
}
