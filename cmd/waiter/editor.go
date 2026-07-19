package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

type LineEditor struct {
	buf     []rune
	pos     int
	hist    *History
	histI   int
	pending string
}

func newLineEditor(h *History) *LineEditor {
	return &LineEditor{hist: h, histI: -1}
}

func (e *LineEditor) clear() {
	e.buf = e.buf[:0]
	e.pos = 0
	e.histI = -1
}

func (e *LineEditor) redrawPending(text string) {
	e.pending = text
}

func (e *LineEditor) read() (string, error) {
	if e.pending != "" {
		t := e.pending
		e.pending = ""
		return t, nil
	}

	e.buf = e.buf[:0]
	e.pos = 0
	e.histI = -1

	in := bufio.NewReader(os.Stdin)
	for {
		b := make([]byte, 1)
		_, err := in.Read(b)
		if err != nil {
			return "", err
		}

		switch b[0] {
		case '\r', '\n':
			fmt.Print("\n")
			return string(e.buf), nil

		case 0x03:
			fmt.Print("^C\n")
			os.Exit(130)
			return "", nil

		case 0x04:
			if len(e.buf) == 0 {
				return "", io.EOF
			}
			continue

		case 0x08, 0x7f:
			if e.pos > 0 {
				e.pos--
				e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
				e.redraw()
			}

		case 0x1b:
			seq := make([]byte, 2)
			if _, err := io.ReadFull(in, seq); err != nil {
				continue
			}
			if seq[0] != '[' {
				continue
			}
			switch seq[1] {
			case 'A':
				e.historyPrev()
			case 'B':
				e.historyNext()
			case 'C':
				if e.pos < len(e.buf) {
					e.pos++
					e.redraw()
				}
			case 'D':
				if e.pos > 0 {
					e.pos--
					e.redraw()
				}
			case 'H', '1':
				if seq[1] == '1' {
					io.ReadFull(in, make([]byte, 1))
				}
				e.pos = 0
				e.redraw()
			case 'F', '4':
				if seq[1] == '4' {
					io.ReadFull(in, make([]byte, 1))
				}
				e.pos = len(e.buf)
				e.redraw()
			case '3':
				io.ReadFull(in, make([]byte, 1))
				if e.pos < len(e.buf) {
					e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
					e.redraw()
				}
			}

		case '\t':
			e.doCompletion()

		default:
			r, size := decodeRune(b[0], in)
			if r != -1 {
				e.buf = append(e.buf, 0)
				copy(e.buf[e.pos+1:], e.buf[e.pos:])
				e.buf[e.pos] = r
				e.pos++
				e.redraw()
			} else if size > 0 {
				// skip invalid continuation bytes
				for i := 1; i < size; i++ {
					io.ReadFull(in, make([]byte, 1))
				}
			}
		}
	}
}

func (e *LineEditor) historyPrev() {
	all := e.hist.all()
	if len(all) == 0 {
		return
	}
	if e.histI == -1 {
		e.histI = len(all) - 1
	} else if e.histI > 0 {
		e.histI--
	}
	e.buf = []rune(all[e.histI])
	e.pos = len(e.buf)
	e.redraw()
}

func (e *LineEditor) historyNext() {
	if e.histI == -1 {
		return
	}
	all := e.hist.all()
	e.histI++
	if e.histI >= len(all) {
		e.histI = -1
		e.buf = e.buf[:0]
		e.pos = 0
	} else {
		e.buf = []rune(all[e.histI])
		e.pos = len(e.buf)
	}
	e.redraw()
}

func (e *LineEditor) doCompletion() {
	cmds := []string{"/help", "/exit", "/quit", "/clear", "/reconnect", "/connect ", "/remote ", "/local",
		"/status", "/kernel", "/settings ", "/settings set ", "/chat ",
		"/plugin ", "/plugin list", "/plugin install ", "/plugin remove ", "/plugin info ",
		"/memory ", "/memory query ", "/knowledge", "/agents"}
	prefix := string(e.buf)
	for _, c := range cmds {
		if strings.HasPrefix(c, prefix) && c != prefix {
			e.buf = []rune(c)
			e.pos = len(e.buf)
			e.redraw()
			return
		}
	}
}

// decodeRune reads a UTF-8 encoded rune from the input.
// b is the first byte; in provides continuation bytes if needed.
// Returns the rune (or -1 if invalid) and the total byte count consumed.
func decodeRune(b byte, in *bufio.Reader) (rune, int) {
	if b < 0x80 {
		return rune(b), 1
	}

	var want int
	switch {
	case b >= 0xf0:
		want = 4
	case b >= 0xe0:
		want = 3
	case b >= 0xc0:
		want = 2
	default:
		return -1, 1 // stray continuation byte, skip
	}

	seq := make([]byte, want)
	seq[0] = b
	for i := 1; i < want; i++ {
		if _, err := io.ReadFull(in, seq[i:i+1]); err != nil {
			return -1, want
		}
		if seq[i]&0xc0 != 0x80 {
			return -1, want // invalid continuation
		}
	}

	r := rune(0)
	switch want {
	case 2:
		r = rune(seq[0]&0x1f)<<6 | rune(seq[1]&0x3f)
	case 3:
		r = rune(seq[0]&0x0f)<<12 | rune(seq[1]&0x3f)<<6 | rune(seq[2]&0x3f)
	case 4:
		r = rune(seq[0]&0x07)<<18 | rune(seq[1]&0x3f)<<12 | rune(seq[2]&0x3f)<<6 | rune(seq[3]&0x3f)
	}

	if r == 0 {
		return -1, want
	}
	return r, want
}

func (e *LineEditor) redraw() {
	fmt.Print("\r\033[K")
	fmt.Print(string(e.buf))
	if e.pos < len(e.buf) {
		skip := len(e.buf) - e.pos
		fmt.Printf("\033[%dD", skip)
	}
}
