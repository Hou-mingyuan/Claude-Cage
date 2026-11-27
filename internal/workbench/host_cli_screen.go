package workbench

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"
)

// HostCLIDiagnostic renders controller errors and notices as plain terminal text.
func HostCLIDiagnostic(message string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, message)
}

// cliScreen is a streaming allowlist for bytes written to an actual host
// terminal. Pipe output stays byte-for-byte intact. Control strings (including
// clipboard and file transfer requests) are consumed through their terminator.
type cliScreen struct {
	state   byte
	csi     []byte
	pending []byte
	invalid bool
}

func (p *cliScreen) filter(chunk []byte, history bool) []byte {
	if len(p.pending) > 0 {
		chunk = append(p.pending, chunk...)
		p.pending = nil
	}
	out := make([]byte, 0, len(chunk))
	for len(chunk) > 0 {
		b := chunk[0]
		switch p.state {
		case 0:
			switch {
			case b == 0x1b:
				p.state = 'e'
			case b == 0x9b:
				p.state = '['
				p.csi = p.csi[:0]
				p.invalid = false
			case b == 0x90 || b == 0x98 || b == 0x9d || b == 0x9e || b == 0x9f:
				p.state = 's'
			case b >= 0x80:
				if !utf8.FullRune(chunk) {
					p.pending = append(p.pending, chunk...)
					return out
				}
				r, n := utf8.DecodeRune(chunk)
				if n > 1 && r >= 0xa0 {
					out = append(out, chunk[:n]...)
				}
				chunk = chunk[n:]
				continue
			case b >= 0x20 && b < 0x7f || b == '\r' || b == '\n' || b == '\t' || b == '\b':
				out = append(out, b)
			}
		case 'e':
			p.state = 0
			switch b {
			case '[':
				p.state = '['
				p.csi = p.csi[:0]
				p.invalid = false
			case ']', 'P', 'X', '^', '_':
				p.state = 's'
			case '(', ')':
				p.state = b
			case '7', '8', 'D', 'M', 'E', '=', '>':
				out = append(out, 0x1b, b)
			case 0x1b:
				p.state = 'e'
			}
		case '(', ')':
			if b == 'B' || b == '0' {
				out = append(out, 0x1b, p.state, b)
			}
			p.state = 0
		case 's':
			if b == 0x07 || b == 0x9c {
				p.state = 0
			} else if b == 0x1b {
				p.state = 't'
			}
		case 't':
			if b == '\\' || b == 0x07 || b == 0x9c {
				p.state = 0
			} else if b != 0x1b {
				p.state = 's'
			}
		case '[':
			if b >= 0x40 && b <= 0x7e {
				if !p.invalid && safeCLICSI(p.csi, b, history) {
					out = append(out, 0x1b, '[')
					out = append(out, p.csi...)
					out = append(out, b)
				}
				p.state = 0
			} else if (b >= '0' && b <= '9' || b == ';' || b == ':' || b == '?') && len(p.csi) < 128 {
				p.csi = append(p.csi, b)
			} else {
				p.invalid = true
			}
		}
		chunk = chunk[1:]
	}
	return out
}

func safeCLICSI(params []byte, final byte, history bool) bool {
	if len(params) > 0 && params[0] == '?' {
		if final != 'h' && final != 'l' {
			return false
		}
		for _, p := range strings.Split(string(params[1:]), ";") {
			switch p {
			case "1", "7", "12", "25", "47", "1000", "1002", "1003", "1004", "1005", "1006", "1015", "1047", "1048", "1049", "2004", "2026":
			default:
				return false
			}
		}
		return true
	}
	if bytes.IndexByte(params, '?') >= 0 {
		return false
	}
	if final == 'n' {
		return !history && (string(params) == "5" || string(params) == "6")
	}
	return strings.IndexByte("@ABCDEFGHJKLMPSTX`abdefgmdrsu", final) >= 0
}
