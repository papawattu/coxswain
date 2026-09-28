package egress

import (
	"encoding/binary"
)

// ExtractSNI extracts the server_name (SNI, extension type 0x0000) from a TLS
// 1.2 ClientHello carried in the first bytes of a CONNECT tunnel. It returns
// the SNI and whether one was found. An absent SNI (ECH, or a client that
// omits it) returns ok=false, which the proxy treats as deny (the fail-closed
// choice, ADR-0007 I42 resolution). The parser walks the extension list, so a
// server_name extension that is not first is still found.
//
// It does NOT do a full handshake: it only needs the SNI to cross-check
// against the CONNECT host. The proxy sees the ClientHello because the client
// sends it through the tunnel after the proxy returns 200 for CONNECT.
func ExtractSNI(b []byte) (string, bool) {
	// TLS record header: type(1) + version(2) + length(2).
	if len(b) < 5 || b[0] != 0x16 {
		return "", false
	}
	recLen := int(binary.BigEndian.Uint16(b[3:5]))
	rec := b[5:]
	if len(rec) < recLen {
		rec = b[5:] // tolerate a partial buffer; we only need the handshake head
	}
	// Handshake header: type(1, 0x01 = ClientHello) + length(3).
	if len(rec) < 4 || rec[0] != 0x01 {
		return "", false
	}
	hs := rec[4:]
	// ClientHello body: client_version(2) + random(32) + session_id_len(1)+id +
	// cipher_suites_len(2)+ciphers + compression(1)+methods + exts_len(2)+exts.
	if len(hs) < 34 {
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
	exts := hs[pos : pos+extsLen]
	if len(exts) < extsLen {
		exts = hs[pos:]
	}
	return walkExtensions(exts)
}

// walkExtensions walks a TLS extension list and returns the first
// server_name (0x0000) SNI. Each extension is: type(2) + length(2) + body.
// The server_name body is: server_name_list_len(2) + (name_type(1) +
// name_len(2) + name)*.
func walkExtensions(exts []byte) (string, bool) {
	pos := 0
	for pos+4 <= len(exts) {
		extType := int(binary.BigEndian.Uint16(exts[pos : pos+2]))
		extLen := int(binary.BigEndian.Uint16(exts[pos+2 : pos+4]))
		body := exts[pos+4 : pos+4+extLen]
		if extType != 0x0000 {
			pos += 4 + extLen
			continue
		}
		if len(body) < 2 {
			return "", false
		}
		listLen := int(binary.BigEndian.Uint16(body[:2]))
		names := body[2 : 2+listLen]
		np := 0
		for np+3 <= len(names) {
			nameType := names[np]
			nameLen := int(binary.BigEndian.Uint16(names[np+1 : np+3]))
			if nameType != 0x00 {
				np += 3 + nameLen
				continue
			}
			if np+3+nameLen > len(names) {
				return "", false
			}
			return string(names[np+3 : np+3+nameLen]), true
		}
		return "", false
	}
	return "", false
}
