package keysmith

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

var cygwinSymlinkMagic = []byte("!<symlink>")

func cygwinSymlinkTarget(raw []byte) (string, bool) {
	if !bytes.HasPrefix(raw, cygwinSymlinkMagic) {
		return "", false
	}
	return decodeLinkTargetBytes(raw[len(cygwinSymlinkMagic):], false)
}

func reparseDataLinkTarget(raw []byte) (string, bool) {
	if target, ok := cygwinSymlinkTarget(raw); ok {
		return target, true
	}
	limit := len(raw)
	if limit > 16 {
		limit = 16
	}
	for off := 0; off < limit; off++ {
		if target, ok := decodeLinkTargetBytes(raw[off:], false); ok {
			return target, true
		}
	}
	return "", false
}

func decodeLinkTargetBytes(raw []byte, requirePathish bool) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	switch {
	case bytes.HasPrefix(raw, []byte{0xff, 0xfe}):
		return decodeUTF16LinkTarget(raw[2:], binary.LittleEndian, requirePathish)
	case bytes.HasPrefix(raw, []byte{0xfe, 0xff}):
		return decodeUTF16LinkTarget(raw[2:], binary.BigEndian, requirePathish)
	case looksUTF16LE(raw):
		return decodeUTF16LinkTarget(raw, binary.LittleEndian, requirePathish)
	case looksUTF16BE(raw):
		return decodeUTF16LinkTarget(raw, binary.BigEndian, requirePathish)
	default:
		return decodeUTF8LinkTarget(raw, requirePathish)
	}
}

func decodeUTF8LinkTarget(raw []byte, requirePathish bool) (string, bool) {
	if i := bytes.IndexByte(raw, 0); i >= 0 {
		raw = raw[:i]
	}
	raw = bytes.TrimRight(raw, "\x00")
	if len(raw) == 0 || !utf8.Valid(raw) {
		return "", false
	}
	return acceptLinkTarget(string(raw), requirePathish)
}

func decodeUTF16LinkTarget(raw []byte, order binary.ByteOrder, requirePathish bool) (string, bool) {
	u16 := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		v := order.Uint16(raw[i : i+2])
		if v == 0 {
			break
		}
		u16 = append(u16, v)
	}
	if len(u16) == 0 {
		return "", false
	}
	return acceptLinkTarget(string(utf16.Decode(u16)), requirePathish)
}

func looksUTF16LE(raw []byte) bool {
	return len(raw) >= 4 && raw[1] == 0 && raw[3] == 0 && raw[0] >= 0x20
}

func looksUTF16BE(raw []byte) bool {
	return len(raw) >= 4 && raw[0] == 0 && raw[2] == 0 && raw[1] >= 0x20
}

func acceptLinkTarget(s string, requirePathish bool) (string, bool) {
	s = strings.TrimRight(s, "\x00")
	if s == "" {
		return "", false
	}
	for _, r := range s {
		if r < 0x20 || r == utf8.RuneError {
			return "", false
		}
	}
	if requirePathish && !strings.ContainsAny(s, `/\`) && !strings.HasPrefix(s, ".") {
		return "", false
	}
	return s, true
}
