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
			if b[0] >= 0x20 {
				e.buf = append(e.buf, 0)
				copy(e.buf[e.pos+1:], e.buf[e.pos:])
				e.buf[e.pos] = rune(b[0])
				e.pos++
				e.redraw()
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

func (e *LineEditor) redraw() {
	fmt.Print("\r\033[K")
	fmt.Print(string(e.buf))
	if e.pos < len(e.buf) {
		skip := len(e.buf) - e.pos
		fmt.Printf("\033[%dD", skip)
	}
}
