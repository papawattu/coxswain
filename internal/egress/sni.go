package egress

import (
	"encoding/binary"
)

// ExtractSNI extracts the server_name (SNI, extension type 0x0000) from a TLS
// 1.2 ClientHello. It returns the SNI and whether one was found. An absent
// SNI (ECH, or a client that omits it) returns ok=false, which the proxy
// treats as deny (the fail-closed choice, ADR-0007 I42 resolution). The
// parser walks the extension list, so a server_name extension that is not
// first is still found.
//
// It does NOT do a full handshake: it only needs the SNI to cross-check
// against the CONNECT host. The proxy sees the ClientHello because the
// client sends it through the tunnel after the proxy returns 200 for
// CONNECT.
//
// Every length taken from the wire is bounds-checked against the input
// before any slice; on any overflow the parser denies (returns
// ("", false)). It never panics and never reads past len(b): this is an
// enforcement parser on attacker-controlled bytes. The input should be the
// first TLS record read from the CONNECT tunnel (the proxy reads the 5-byte
// record header, verifies the type, and io.ReadFulls exactly recLen bytes).
func ExtractSNI(b []byte) (string, bool) {
	// TLS record header: type(1) + version(2) + length(2).
	if len(b) < 5 || b[0] != 0x16 {
		return "", false
	}
	recLen := int(binary.BigEndian.Uint16(b[3:5]))
	// The record body must be fully present: the proxy reads exactly recLen
	// bytes before calling. A short buffer is malformed input — deny.
	if len(b) < 5+recLen {
		return "", false
	}
	rec := b[5 : 5+recLen]
	return extractFromRecord(rec)
}

// extractFromRecord parses a ClientHello from a complete TLS record body
// (the record header stripped). Split out for the truncated-length table
// tests.
func extractFromRecord(rec []byte) (string, bool) {
	// Handshake header: type(1, 0x01 = ClientHello) + length(3).
	if len(rec) < 4 || rec[0] != 0x01 {
		return "", false
	}
	hsLen := int(rec[1])<<16 | int(rec[2])<<8 | int(rec[3])
	// The handshake body must fit inside the record (a ClientHello is one
	// handshake message per record in practice; be strict).
	if hsLen > len(rec)-4 {
		return "", false
	}
	hs := rec[4 : 4+hsLen]
	// ClientHello body: client_version(2) + random(32) + session_id_len(1)+id
	// + cipher_suites_len(2)+ciphers + compression(1)+methods +
	// exts_len(2)+exts. Minimum: 2+32+1+2+1+2 = 40.
	if len(hs) < 40 {
		return "", false
	}
	pos := 2 + 32 // skip version + random
	if pos+1 > len(hs) {
		return "", false
	}
	sidLen := int(hs[pos])
	pos += 1 + sidLen
	if pos+2 > len(hs) {
		return "", false
	}
	csLen := int(binary.BigEndian.Uint16(hs[pos : pos+2]))
	pos += 2 + csLen
	if pos+1 > len(hs) {
		return "", false
	}
	numComp := int(hs[pos])
	pos += 1 + numComp
	if pos+2 > len(hs) {
		return "", false
	}
	extsLen := int(binary.BigEndian.Uint16(hs[pos : pos+2]))
	pos += 2
	if pos+extsLen > len(hs) {
		return "", false
	}
	return walkExtensions(hs[pos : pos+extsLen])
}

// walkExtensions walks a TLS extension list and returns the first
// server_name (0x0000) SNI. Each extension is: type(2) + length(2) + body.
// The server_name body is: server_name_list_len(2) + (name_type(1) +
// name_len(2) + name)*. Every length from the wire is bounds-checked; any
// overflow denies (returns ("", false)).
func walkExtensions(exts []byte) (string, bool) {
	pos := 0
	for pos+4 <= len(exts) {
		extType := int(binary.BigEndian.Uint16(exts[pos : pos+2]))
		extLen := int(binary.BigEndian.Uint16(exts[pos+2 : pos+4]))
		if pos+4+extLen > len(exts) {
			return "", false
		}
		body := exts[pos+4 : pos+4+extLen]
		if extType != 0x0000 {
			pos += 4 + extLen
			continue
		}
		if len(body) < 2 {
			return "", false
		}
		listLen := int(binary.BigEndian.Uint16(body[:2]))
		if 2+listLen > len(body) {
			return "", false
		}
		names := body[2 : 2+listLen]
		np := 0
		for np+3 <= len(names) {
			nameType := names[np]
			nameLen := int(binary.BigEndian.Uint16(names[np+1 : np+3]))
			if np+3+nameLen > len(names) {
				return "", false
			}
			if nameType != 0x00 {
				np += 3 + nameLen
				continue
			}
			return string(names[np+3 : np+3+nameLen]), true
		}
		// A truncated final name (fewer than 3 bytes left) is malformed —
		// deny rather than ignore.
		if np < len(names) {
			return "", false
		}
		return "", false
	}
	return "", false
}
