package egress

import (
	"bytes"
	"strings"
	"testing"

	"github.com/papawattu/coxswain/internal/egress/egresstest"
)

func TestExtractSNI(t *testing.T) {
	tests := []struct {
		name    string
		input   []byte
		wantSNI string
		wantOK  bool
	}{
		{"normal SNI", egresstest.BuildClientHello(testAllowHost), testAllowHost, true},
		{"SNI with dots and port-free host", egresstest.BuildClientHello(testAllowHost2), testAllowHost2, true},
		{"no SNI (ECH / omitted) -> absent", egresstest.BuildClientHello(""), "", false},
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

// TestExtractSNITruncatedLengths is the table of truncated/oversized length
// fields from the wire: every one must deny (("", false)) — never panic,
// never read out of bounds, never return a spurious SNI.
func TestExtractSNITruncatedLengths(t *testing.T) {
	hello := egresstest.BuildClientHello(testAllowHost) // a fully valid hello to corrupt
	type patch struct {
		off int
		val byte
	}
	cases := map[string][]patch{
		"record length 0xffff (past end)":      {{3, 0xff}, {4, 0xff}},
		"record length 0 (empty record)":       {{3, 0x00}, {4, 0x00}},
		"handshake length 0xffffff (past end)": {{5, 0xff}, {6, 0xff}, {7, 0xff}},
		"handshake length 0 (empty handshake)": {{5, 0x00}, {6, 0x00}, {7, 0x00}},
		"session id length past end":           {{43, 0xff}},
		"cipher suites length past end":        {{44, 0xff}, {45, 0xff}},
		"cipher suites length 0 (empty)":       {{44, 0x00}, {45, 0x00}},
		"num compression methods past end":     {{50, 0xff}},
		"extensions length past end":           {{52, 0xff}, {53, 0xff}},
		"extensions length 0 (no server_name)": {{52, 0x00}, {53, 0x00}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			out := append([]byte(nil), hello...)
			for _, e := range c {
				out[e.off] = e.val
			}
			if got, ok := ExtractSNI(out); ok {
				t.Fatalf("ExtractSNI = (%q, true); want deny", got)
			}
		})
	}
	// A record type that is not a handshake (0x17 = change_cipher_spec) must
	// deny — the handler only relays a 0x16 record to ExtractSNI, but the
	// parser must also refuse it defensively.
	t.Run("record type not handshake", func(t *testing.T) {
		out := append([]byte(nil), hello...)
		out[0] = 0x17
		if got, ok := ExtractSNI(out); ok {
			t.Fatalf("ExtractSNI = (%q, true); want deny", got)
		}
	})
	// A handshake type that is not a ClientHello (0x02 = ServerHello) must
	// deny.
	t.Run("handshake type not client hello", func(t *testing.T) {
		out := append([]byte(nil), hello...)
		out[5] = 0x02
		if got, ok := ExtractSNI(out); ok {
			t.Fatalf("ExtractSNI = (%q, true); want deny", got)
		}
	})
	// A server_name extension whose own length points past the end of the
	// record must deny (walkExtensions bounds-checks the ext body).
	t.Run("server_name ext length past end", func(t *testing.T) {
		out := append([]byte(nil), hello...)
		for i := 52; i+4 <= len(out); i++ {
			if out[i] == 0x00 && out[i+1] == 0x00 {
				out[i+2], out[i+3] = 0xff, 0xff
				break
			}
		}
		if got, ok := ExtractSNI(out); ok {
			t.Fatalf("ExtractSNI = (%q, true); want deny", got)
		}
	})
	// A server_name list length that points past the end of the ext body
	// must deny (bounds-checked inside walkExtensions).
	t.Run("server_name list length past end", func(t *testing.T) {
		out := append([]byte(nil), hello...)
		for i := 52; i+4 <= len(out); i++ {
			if out[i] == 0x00 && out[i+1] == 0x00 {
				// listLen is at the start of the ext body (i+4).
				out[i+4], out[i+5] = 0xff, 0xff
				break
			}
		}
		if got, ok := ExtractSNI(out); ok {
			t.Fatalf("ExtractSNI = (%q, true); want deny", got)
		}
	})
	// A name length that points past the end of the server_name list must
	// deny (bounds-checked inside walkExtensions).
	t.Run("name length past end", func(t *testing.T) {
		out := append([]byte(nil), hello...)
		for i := 52; i+4 <= len(out); i++ {
			if out[i] == 0x00 && out[i+1] == 0x00 {
				// nameLen sits at body+3 (after listLen(2)+nameType(1)).
				out[i+4+3], out[i+4+4] = 0xff, 0xff
				break
			}
		}
		if got, ok := ExtractSNI(out); ok {
			t.Fatalf("ExtractSNI = (%q, true); want deny", got)
		}
	})
	// A record truncated to half its length (partial buffer) must deny —
	// the parser no longer "tolerates" a partial buffer.
	t.Run("truncated record (half cut off)", func(t *testing.T) {
		if got, ok := ExtractSNI(hello[:len(hello)/2]); ok {
			t.Fatalf("ExtractSNI = (%q, true); want deny", got)
		}
	})
}

// TestExtractSNISNIPastByte512 proves the parser finds the SNI when it
// sits after byte 512 of the record (a large pre-SNI extension). This is
// the unit-level twin of the handler's "SNI past byte 512" case: the old
// Peek(512) returned only the first 512 bytes and reported SNI absent.
func TestExtractSNISNIPastByte512(t *testing.T) {
	// padLen=480: dummy ext of 476 bytes before server_name pushes the
	// SNI well past byte 512.
	hello := egresstest.BuildHelloWithPadding(testAllowHost, 480)
	// Sanity: the SNI really is past byte 512 in the record.
	idx := bytes.Index(hello, []byte(testAllowHost))
	if idx < 512 {
		t.Fatalf("test setup: SNI at offset %d is not past byte 512", idx)
	}
	got, ok := ExtractSNI(hello)
	if !ok || got != testAllowHost {
		t.Fatalf("ExtractSNI with SNI past byte 512 = (%q, %v); want (%q, true)", got, ok, testAllowHost)
	}
}

// FuzzExtractSNI: the SNI parser must never panic on any input, and any SNI
// it returns must be a real substring of the input (it must not read past
// the input or fabricate bytes).
func FuzzExtractSNI(f *testing.F) {
	f.Add(egresstest.BuildClientHello(testAllowHost))
	f.Add(egresstest.BuildClientHello(""))
	f.Add([]byte{0x16, 0x03, 0x01, 0x00, 0x05, 0x01})
	f.Fuzz(func(t *testing.T, b []byte) {
		sni, ok := ExtractSNI(b)
		if ok && len(sni) > 0 {
			if !strings.Contains(string(b), sni) {
				t.Fatalf("returned SNI %q is not a substring of the input", sni)
			}
		}
	})
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
	// server_name_list_len(2) + name_type(1) + name_len(2) + name
	sniBody.Write([]byte{byte(listLen >> 8), byte(listLen)})
	sniBody.Write([]byte{0x00}) // name type: host_name
	sniBody.Write([]byte{byte(len(extName) >> 8), byte(len(extName))})
	sniBody.Write(extName)
	extList.Write([]byte{0x00, 0x00}) // extension type: server_name
	extList.Write([]byte{byte(sniBody.Len() >> 8), byte(sniBody.Len())})
	extList.Write(sniBody.Bytes())

	var hello bytes.Buffer
	hello.Write([]byte{0x03, 0x03})
	hello.Write(make([]byte, 32))
	hello.Write([]byte{0x00})
	ciphers := []byte{0x13, 0x01}
	hello.Write([]byte{byte(len(ciphers) >> 8), byte(len(ciphers))})
	hello.Write(ciphers)
	hello.Write([]byte{0x01, 0x00})
	hello.Write([]byte{byte(extList.Len() >> 8), byte(extList.Len())})
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
	rec.Write([]byte{byte(hs.Len() >> 8), byte(hs.Len())})
	rec.Write(hs.Bytes())

	got, ok := ExtractSNI(rec.Bytes())
	if !ok || got != "example.com" {
		t.Fatalf("ExtractSNI with other extensions = (%q, %v); want (example.com, true)", got, ok)
	}
}
