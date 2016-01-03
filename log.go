// This was created based on log/log.go from the Go source, imported originally from
// https://github.com/golang/go/blob/692054e76e7686c6d5de385df69873e6427a35fb/src/log/log.go

// Portions Copyright 2015 Dan Tillberg. Those portions
// are licensed for use according to the ISC License.

// The original copyright notice is below:

// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package log implements a simple logging package. It defines a type, Logger,
// with methods for formatting output. It also has a predefined 'standard'
// Logger accessible through helper functions Print[f|ln], Fatal[f|ln], and
// Panic[f|ln], which are easier to use than creating a Logger manually.
// That logger writes to standard error and prints the date and time
// of each logged message.
// The Fatal functions call os.Exit(1) after writing the log message.
// The Panic functions call panic after writing the log message.
package alog

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"
)

// These flags define which text to prefix to each log entry generated by the Logger.
const (
	// Bits or'ed together to control what's printed.
	// There is no control over the order they appear (the order listed
	// here) or the format they present (as described in the comments).
	// The prefix is followed by a colon only when Llongfile or Lshortfile
	// is specified.
	// For example, flags Ldate | Ltime (or LstdFlags) produce,
	//  2009/01/23 01:23:23 message
	// while flags Ldate | Ltime | Lmicroseconds | Llongfile produce,
	//  2009/01/23 01:23:23.123123 /a/b/c/d.go:23: message
	Ldate         = 1 << iota // the date in the local time zone: 2009/01/23
	Ltime                     // the time in the local time zone: 01:23:23
	Lmicroseconds             // microsecond resolution: 01:23:23.123123.  assumes Ltime.
	Llongfile                 // full file name and line number: /a/b/c/d.go:23
	Lshortfile                // final file name element and line number: d.go:23. overrides Llongfile
	LUTC                      // if Ldate or Ltime is set, use UTC rather than the local time zone
	Lelapsed                  // elapsed time since this line was first started
	Lisodate
	LstdFlags = Ldate | Ltime // initial values for the standard logger
)

type ColorCode int

const (
	ColorBlack ColorCode = 30 + iota
	ColorRed
	ColorGreen
	ColorYellow
	ColorBlue
	ColorMagenta
	ColorCyan
	ColorWhite
)
const (
	ColorNone     ColorCode = 0
	ColorReset              = 39
	ColorResetAll           = 128
	ColorBright             = 256
	ColorDim                = 512
)

func (code ColorCode) GetAnsiCodes() []int {
	codes := []int{}
	if code&ColorResetAll != 0 {
		codes = append(codes, 0)
		code = code & (^ColorResetAll)
	}
	if code&ColorBright != 0 {
		codes = append(codes, 1)
		code = code & (^ColorBright)
	}
	if code&ColorDim != 0 {
		codes = append(codes, 2)
		code = code & (^ColorDim)
	}
	if code != ColorNone {
		codes = append(codes, int(code))
	}
	return codes
}

var ansiColorCodes = map[string]ColorCode{
	"r":       ColorResetAll,
	"reset":   ColorResetAll,
	"bright":  ColorBright,
	"dim":     ColorBright | ColorBlack,
	"black":   ColorBlack,
	"grey":    ColorBlack,
	"red":     ColorRed,
	"green":   ColorGreen,
	"yellow":  ColorYellow,
	"blue":    ColorBlue,
	"magenta": ColorMagenta,
	"cyan":    ColorCyan,
	"white":   ColorWhite,
	"cr":      ColorReset,

	"error":   ColorRed,
	"success": ColorGreen,
	"warn":    ColorYellow,
}

var tputCache = make(map[string]string)

func tput(strs ...string) string {
	key := strings.Join(strs, "-")
	val, ok := tputCache[key]
	if !ok {
		cmd := exec.Command("tput", strs...)
		out, err := cmd.Output()
		if err != nil {
			msg := fmt.Sprintf("\nFailed to execute `tput %s`. That probably means you need to disable MultilineMode.\n", strings.Join(strs, " "))
			os.Stderr.WriteString(msg)
			return ""
		}
		val = string(out)
		tputCache[key] = val
	}
	return val
}

type WriterState struct {
	mutex           sync.Mutex
	lastTemp        [][]byte
	tempLoggers     []*Logger
	termWidth       int
	multiline       bool
	cursorLineIndex int
	cursorIsInline  bool
	cursorIsAtBegin bool
}

func (w *WriterState) removeTempLogger(l *Logger) {
	// Remove this logger from the list of tempLoggers for this writer
	for i, logger := range w.tempLoggers {
		if logger == l {
			if i == len(w.tempLoggers)-1 {
				w.tempLoggers = w.tempLoggers[:i]
			} else {
				w.tempLoggers = append(w.tempLoggers[:i], w.tempLoggers[i+1:]...)
			}
			break
		}
	}
}

func (w *WriterState) lock()   { w.mutex.Lock() }
func (w *WriterState) unlock() { w.mutex.Unlock() }

func (w *WriterState) addTempLogger(l *Logger) {
	w.tempLoggers = append(w.tempLoggers, l)
}

func (w *WriterState) flushAll() {
	for _, logger := range w.tempLoggers {
		logger.flushInt()
	}
}

func (w *WriterState) closeAll() {
	for _, logger := range w.tempLoggers {
		logger.flushInt()
		logger.closeInt()
	}
}

func getWriterState(writer io.Writer) *WriterState {
	mutexGlobal.RLock()
	ws, ok := writers[writer]
	mutexGlobal.RUnlock()
	if !ok {
		mutexGlobal.Lock()
		ws, ok = writers[writer]
		if !ok {
			ws = &WriterState{}
			ws.cursorIsAtBegin = true
			ws.cursorIsInline = false
			ws.lastTemp = [][]byte{[]byte{}}
			writers[writer] = ws
		}
		mutexGlobal.Unlock()
	}
	return ws
}

// ensures atomic writes; shared by all Logger instances
var mutexGlobal sync.RWMutex
var loggers []*Logger
var writers map[io.Writer]*WriterState = make(map[io.Writer]*WriterState)

const ansiCodeResetAll = 0
const ansiCodeHighestIntensity = 2
const ansiCodeResetForecolor = 39

var bytesEmpty = []byte("")
var bytesCarriageReturn = []byte("\r")
var byteNewline = byte('\n')
var bytesNewline = []byte{byteNewline}
var bytesSpace = []byte(" ")

var bytesComma = []byte(",")
var ansiColorRegexp = regexp.MustCompile("\033\\[(\\d+)m")
var ansiColorOrCharRegexp = regexp.MustCompile("(\033\\[\\d+m)|.")
var ansiBytesEscapeStart = []byte("\033[")
var ansiBytesColorEscapeEnd = []byte("m")
var ansiBytesResetAll = []byte("\033[0m")
var ansiBytesResetForecolor = []byte("\033[39m")

var tempLineSep = []byte(" | ")
var tempLineSepLength = stringLen(tempLineSep)
var tempLineEllipsis = []byte("...")
var tempLineEllipsisLength = stringLen(tempLineEllipsis)

const minTempSegmentLength = 6

// These facilitate "nullable" bools for some settings
var yes = true
var no = false

func boolPointer(flag bool) *bool {
	if flag {
		return &yes
	}
	return &no
}

type ActiveAnsiCodes struct {
	intensity int
	forecolor int
}

func (codes *ActiveAnsiCodes) anyActive() bool {
	return codes.intensity != 0 || codes.forecolor != 0
}

func (codes *ActiveAnsiCodes) add(code int) {
	if code == ansiCodeResetAll {
		codes.intensity = 0
		codes.forecolor = 0
	} else if code <= ansiCodeHighestIntensity {
		codes.intensity = int(code)
	} else if code == ansiCodeResetForecolor {
		codes.forecolor = 0
	} else {
		codes.forecolor = int(code)
	}
}

func (codes *ActiveAnsiCodes) getResetBytes() []byte {
	if codes.intensity != 0 {
		return ansiBytesResetAll
	}
	if codes.forecolor != 0 {
		return ansiBytesResetForecolor
	}
	return bytesEmpty
}

func getActiveAnsiCodes(buf []byte) *ActiveAnsiCodes {
	var ansiActive ActiveAnsiCodes
	for _, groups := range ansiColorRegexp.FindAllSubmatch(buf, -1) {
		code, _ := strconv.ParseInt(string(groups[1]), 10, 32)
		ansiActive.add(int(code))
	}
	return &ansiActive
}

// GetSize returns the dimensions of the given terminal.
func getTermWidth(writer io.Writer) int {
	ws := getWriterState(writer)
	if ws.termWidth != 0 {
		return ws.termWidth
	}
	var fd int
	if writer == os.Stdout {
		fd = syscall.Stdout
	} else {
		// For custom writers, just use the width we get for stderr. This might not be true in some
		// cases (and for those cases, we should add an option to explicitly set width), but it will
		// be true in most cases.
		fd = syscall.Stderr
	}
	var dimensions [4]uint16
	if _, _, err := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&dimensions)), 0, 0, 0); err != 0 {
		// Fall back to a width of 80
		return 80
	}
	return int(dimensions[1])
}

// A Logger represents an active logging object that generates lines of
// output to an io.Writer.  Each logging operation makes a single call to
// the Writer's Write method.  A Logger can be used simultaneously from
// multiple goroutines; it guarantees to serialize access to the Writer.
type Logger struct {
	prefix               []byte    // prefix to write at beginning of each line
	flag                 int       // properties
	out                  io.Writer // destination for output
	buf                  []byte    // for accumulating text to write
	tmp                  []byte    // for formatting the current line
	prefixFormatted      []byte
	cursorByteIndex      int
	tempLineActive       bool
	isClosed             bool
	partialLinesEnabled  *bool
	colorEnabled         *bool
	colorTemplateEnabled *bool
	autoAppendNewline    *bool
	colorRegexp          *regexp.Regexp
	termWidth            int
	callerFile           string
	callerLine           int
	now                  time.Time
	lineStartTime        time.Time
}

// New creates a new Logger.   The out variable sets the
// destination to which log data will be written.
// The prefix appears at the beginning of each generated log line.
// The flag argument defines the logging properties.
func New(out io.Writer, prefix string, flag int) *Logger {
	mutexGlobal.Lock()
	defer mutexGlobal.Unlock()
	var l = &Logger{out: out, prefix: []byte(prefix), flag: flag}
	l.reprocessPrefix()
	loggers = append(loggers, l)
	return l
}

// newStd duplicates some of the work done by New because we can't call
// reprocessPrefix here (as it creates a circular reference back to DefaultLogger)
func newStd() *Logger {
	var l = &Logger{out: os.Stderr, prefix: []byte("@(dim:{isodate}) "), flag: 0}
	l.partialLinesEnabled = &yes
	l.colorRegexp = regexp.MustCompile("@\\(([\\w,]+?)(:([^)]*?))?\\)")
	l.colorEnabled = &yes
	l.colorTemplateEnabled = &yes
	l.autoAppendNewline = &no
	// This is like calling reprocessPrefix:
	l.prefixFormatted = processColorTemplates(l.colorRegexp, l.prefix)
	loggers = append(loggers, l)
	return l
}

var DefaultLogger = newStd()

func isTrueDefaulted(flag *bool, fallback *bool) bool {
	if flag != nil {
		return *flag
	}
	return *fallback
}

func (l *Logger) isColorEnabled() bool {
	return isTrueDefaulted(l.colorEnabled, DefaultLogger.colorEnabled)
}

func (l *Logger) isPartialLinesEnabled() bool {
	return isTrueDefaulted(l.partialLinesEnabled, DefaultLogger.partialLinesEnabled)
}

func (l *Logger) isAutoNewlineEnabled() bool {
	return isTrueDefaulted(l.autoAppendNewline, DefaultLogger.autoAppendNewline)
}

func (l *Logger) getColorTemplateRegexp() *regexp.Regexp {
	if !isTrueDefaulted(l.colorTemplateEnabled, DefaultLogger.colorTemplateEnabled) {
		return nil
	}
	if l.colorRegexp != nil {
		return l.colorRegexp
	}
	return DefaultLogger.colorRegexp
}

// SetOutput sets the output destination for the logger.
func (l *Logger) SetOutput(w io.Writer) {
	// This is all not really threadsafe. Calling SetOutput while simultaneously writing
	// data will result in undefined behavior.
	ws := getWriterState(l.out)
	ws.lock()
	defer ws.unlock()
	l.flushInt()
	l.out = w
}

// Cheap integer to fixed-width decimal ASCII.  Give a negative width to avoid zero-padding.
func itoa(buf *[]byte, i int, wid int) {
	// Assemble decimal in reverse order.
	var b [20]byte
	bp := len(b) - 1
	for i >= 10 || wid > 1 {
		wid--
		q := i / 10
		b[bp] = byte('0' + i - q*10)
		bp--
		i = q
	}
	// i < 10
	b[bp] = byte('0' + i)
	*buf = append(*buf, b[bp:]...)
}

func FormatDuration(duration time.Duration) []byte {
	tmp := []byte{}
	secs := duration.Seconds()
	if secs >= 600 {
		if secs >= 10*3600 {
			hours := duration.Hours()
			if hours > 9999 {
				tmp = append(tmp, fmt.Sprintf("%4.0f", hours)...)
			} else if hours >= 99.95 {
				tmp = append(tmp, fmt.Sprintf("%4.0f", hours)[:4]...)
			} else {
				tmp = append(tmp, fmt.Sprintf("%4.1f", hours)[:4]...)
			}
			tmp = append(tmp, "h"...)
		} else {
			mins := duration.Minutes()
			if mins >= 99.95 {
				tmp = append(tmp, fmt.Sprintf("%4.0f", mins)[:4]...)
			} else {
				tmp = append(tmp, fmt.Sprintf("%4.1f", mins)[:4]...)
			}
			tmp = append(tmp, "m"...)
		}
	} else {
		secs := duration.Seconds()
		if secs >= 0.9995 {
			if secs >= 99.95 {
				tmp = append(tmp, fmt.Sprintf("%4.0f", secs)[:4]...)
			} else {
				tmp = append(tmp, fmt.Sprintf("%4.2f", secs)[:4]...)
			}
			tmp = append(tmp, "s"...)
		} else {
			if secs >= 0.00995 {
				tmp = append(tmp, fmt.Sprintf("%3.0f", 1000*secs)[:3]...)
			} else {
				tmp = append(tmp, fmt.Sprintf("%3.1f", 1000*secs)[:3]...)
			}
			tmp = append(tmp, "ms"...)
		}
	}
	return tmp
}

func (l *Logger) appendDate(buf *[]byte, useIsoDate bool) {
	dateSep := "/"
	if useIsoDate {
		dateSep = "-"
	}
	year, month, day := l.now.Date()
	itoa(buf, year, 4)
	*buf = append(*buf, dateSep...)
	itoa(buf, int(month), 2)
	*buf = append(*buf, dateSep...)
	itoa(buf, day, 2)
}

func (l *Logger) appendTime(buf *[]byte, includeMicros bool) {
	hour, min, sec := l.now.Clock()
	itoa(buf, hour, 2)
	*buf = append(*buf, ':')
	itoa(buf, min, 2)
	*buf = append(*buf, ':')
	itoa(buf, sec, 2)
	if includeMicros {
		*buf = append(*buf, '.')
		itoa(buf, l.now.Nanosecond()/1e3, 6)
	}
}

func (l *Logger) appendIsoDate(buf *[]byte, includeMicros bool) {
	l.appendDate(buf, true)
	*buf = append(*buf, 'T')
	l.appendTime(buf, includeMicros)
	*buf = append(*buf, 'Z')
}

func (l *Logger) appendElapsed(buf *[]byte) {
	if !l.lineStartTime.IsZero() && l.now != l.lineStartTime {
		*buf = append(*buf, FormatDuration(l.now.Sub(l.lineStartTime))...)
	} else {
		*buf = append(*buf, '-')
	}
}

var prefixTemplateRegexp = regexp.MustCompile("{(date|time|isodate|elapsed)( micros)?}|.+?")

func (l *Logger) formatHeader(buf *[]byte) {
	for _, groups := range prefixTemplateRegexp.FindAllSubmatch(l.prefixFormatted, -1) {
		if len(groups[1]) != 0 {
			s := string(groups[1])
			includeMicros := len(groups[2]) > 0
			if s == "date" {
				l.appendDate(buf, false)
			} else if s == "time" {
				l.appendTime(buf, includeMicros)
			} else if s == "isodate" {
				l.appendIsoDate(buf, includeMicros)
			} else if s == "elapsed" {
				l.appendElapsed(buf)
			}
		} else {
			*buf = append(*buf, groups[0]...)
		}
	}

	if l.flag&Lisodate != 0 {
		l.appendIsoDate(buf, l.flag&Lmicroseconds != 0)
		*buf = append(*buf, ' ')
	} else {
		if l.flag&Ldate != 0 {
			l.appendDate(buf, false)
			*buf = append(*buf, ' ')
		}
		if l.flag&(Ltime|Lmicroseconds) != 0 {
			l.appendTime(buf, l.flag&Lmicroseconds != 0)
			*buf = append(*buf, ' ')
		}
	}
	if l.flag&(Lshortfile|Llongfile) != 0 {
		*buf = append(*buf, l.callerFile...)
		*buf = append(*buf, ':')
		itoa(buf, l.callerLine, -1)
		*buf = append(*buf, ": "...)
	}
	if l.flag&Lelapsed != 0 && !l.lineStartTime.IsZero() && l.now != l.lineStartTime {
		*buf = append(*buf, "("...)
		l.appendElapsed(buf)
		*buf = append(*buf, ") "...)
	}
}

func moveCursorToLine(out io.Writer, line int) bool {
	ws := getWriterState(out)
	if line == ws.cursorLineIndex {
		return false
	}
	tmp := []byte{}
	for line != ws.cursorLineIndex {
		if line < ws.cursorLineIndex {
			tmp = append(tmp, tput("cuu", "1")...)
			ws.cursorLineIndex--
		} else {
			tmp = append(tmp, tput("cud", "1")...)
			ws.cursorLineIndex++
		}
	}
	tmp = append(tmp, bytesCarriageReturn...)
	out.Write(tmp)
	ws.cursorLineIndex = line
	ws.cursorIsAtBegin = true
	ws.cursorIsInline = false
	return true
}

func setTempLineOutput(out io.Writer, line int, buf []byte) {
	ws := getWriterState(out)
	cursorIsOnlineAndInline := ws.cursorLineIndex == line && ws.cursorIsInline
	lastBuf := ws.lastTemp[line]
	// These lengths are actually fine being in bytes
	lastLen := len(lastBuf)
	currLen := len(buf)
	if currLen == lastLen && bytes.Equal(lastBuf, buf) {
		// Don't need to do anything
		return
	} else if cursorIsOnlineAndInline && (currLen >= lastLen && bytes.Equal(lastBuf, buf[:lastLen])) {
		out.Write(buf[lastLen:])
	} else {
		out.Write(getActiveAnsiCodes(lastBuf).getResetBytes())
		if !moveCursorToLine(out, line) && !ws.cursorIsAtBegin {
			out.Write(bytesCarriageReturn)
		}
		out.Write(buf)
		currStringLen := stringLen(buf)
		lastStringLen := stringLen(lastBuf)
		for i := currStringLen; i < lastStringLen; i++ {
			out.Write(bytesSpace)
		}
		ws.cursorIsInline = currStringLen >= lastStringLen
	}
	ws.cursorIsAtBegin = false
	// This does a lot of copying to avoid aliasing; maybe some could be avoided?
	ws.lastTemp[line] = append([]byte{}, buf...)
}

func writeLine(out io.Writer, buf []byte) {
	setTempLineOutput(out, 0, buf)
	out.Write(getActiveAnsiCodes(buf).getResetBytes())
	ws := getWriterState(out)
	if ws.multiline {
		ws.lastTemp = ws.lastTemp[1:]
		// Always keep an empty line at the bottom
		if len(ws.lastTemp) == 0 {
			ws.lastTemp = append(ws.lastTemp, []byte{})
			moveCursorToLine(out, 0)
			out.Write(bytesNewline)
		} else {
			ws.cursorLineIndex = -1
			moveCursorToLine(out, 0)
		}
	} else {
		out.Write(bytesNewline)
		ws.lastTemp[0] = bytesEmpty
		ws.cursorIsAtBegin = true
		ws.cursorIsInline = false
	}
}

func updateTempOutput(out io.Writer) {
	ws := getWriterState(out)
	maxWidth := getTermWidth(out) - 1
	var bufs [][]byte
	for _, logger := range ws.tempLoggers {
		bufs = append(bufs, logger.getFormattedLine(logger.buf))
	}
	if ws.multiline {
		for i := len(ws.lastTemp); i < len(bufs); i++ {
			moveCursorToLine(out, i-1)
			out.Write(bytesNewline)
			ws.cursorLineIndex = i
			ws.cursorIsAtBegin = true
			ws.cursorIsInline = false
			ws.lastTemp = append(ws.lastTemp, []byte{})
		}
		for i, buf := range bufs {
			setTempLineOutput(out, i, trimStringEllipsis(buf, maxWidth))
		}
	} else {
		numBufs := len(bufs)
		lengths := make([]int, 0)
		lengthSum := 0
		for _, buf := range bufs {
			length := stringLen(buf)
			lengths = append(lengths, length)
			lengthSum += length
		}
		charsLeft := maxWidth - tempLineSepLength*(numBufs-1)
		var outputBuf []byte
		if len(bufs) > 1 {
			if charsLeft < lengthSum {
				shortenedLengths := make([]int, numBufs)
				copy(shortenedLengths, lengths)
				for charsLeft < lengthSum {
					longestIndex := 0
					longestLength := 0
					for i, length := range shortenedLengths {
						if length > longestLength {
							longestIndex = i
							longestLength = length
						}
					}
					if longestLength < minTempSegmentLength {
						// Don't bother making segments shorter than this
						break
					}
					if longestLength == lengths[longestIndex] {
						// It's at max length; we need to lop off space for the ellipsis
						shortenedLengths[longestIndex] -= tempLineEllipsisLength + 1
					} else {
						shortenedLengths[longestIndex] -= 1
					}
					lengthSum -= 1
				}
				var bufs2 [][]byte
				for i, buf := range bufs {
					if shortenedLengths[i] < lengths[i] {
						buf = append(trimString(buf, shortenedLengths[i]), tempLineEllipsis...)
					}
					bufs2 = append(bufs2, buf)
				}
				bufs = bufs2
			}
		}
		outputBuf = bytes.Join(bufs, tempLineSep)
		outputBuf = trimStringEllipsis(outputBuf, maxWidth)
		setTempLineOutput(out, 0, outputBuf)
	}
}

func ansiEscapeBytes(colorCode int) []byte {
	buf := []byte{}
	buf = append(buf, ansiBytesEscapeStart...)
	buf = append(buf, fmt.Sprintf("%d", colorCode)...)
	buf = append(buf, ansiBytesColorEscapeEnd...)
	return buf
}

func uncolorize(buf []byte) []byte {
	return ansiColorRegexp.ReplaceAll(buf, bytesEmpty)
}

func trimString(buf []byte, length int) []byte {
	if length == 0 {
		return bytesEmpty
	}
	tmp := []byte{}
	for _, groups := range ansiColorOrCharRegexp.FindAllSubmatch(buf, -1) {
		tmp = append(tmp, groups[0]...)
		if len(groups[1]) == 0 {
			// This match was not an ANSI escape, so count it towards the length
			length -= 1
			if length <= 0 {
				return tmp
			}
		}
	}
	return tmp
}

func trimStringEllipsis(buf []byte, length int) []byte {
	if stringLen(buf) > length {
		return append(trimString(buf, length-tempLineEllipsisLength), tempLineEllipsis...)
	}
	return buf
}

func stringLen(buf []byte) int {
	// This is not rune-aware
	return utf8.RuneCount(uncolorize(buf))
}

func (l *Logger) getFormattedLine(line []byte) []byte {
	l.tmp = l.tmp[:0]
	l.formatHeader(&l.tmp)
	codes := getActiveAnsiCodes(l.tmp)
	l.tmp = append(l.tmp, codes.getResetBytes()...)
	l.tmp = append(l.tmp, line...)
	if !l.isColorEnabled() {
		l.tmp = uncolorize(l.tmp)
	}
	return l.tmp
}

func (l *Logger) reprocessPrefix() {
	colorTemplateRegexp := l.getColorTemplateRegexp()
	if colorTemplateRegexp != nil {
		l.prefixFormatted = processColorTemplates(colorTemplateRegexp, l.prefix)
	} else {
		l.prefixFormatted = l.prefix
	}
}

func processColorTemplates(colorTemplateRegexp *regexp.Regexp, buf []byte) []byte {
	// We really want ReplaceAllSubmatchFunc, i.e.: https://github.com/golang/go/issues/5690
	// Instead we call FindSubmatch on each match, which means that backtracking may not be
	// used in custom Regexps (matches must also match on themselves without context).
	colorTemplateReplacer := func(token []byte) []byte {
		tmp2 := []byte{}
		groups := colorTemplateRegexp.FindSubmatch(token)
		var ansiActive ActiveAnsiCodes
		for _, codeBytes := range bytes.Split(groups[1], bytesComma) {
			colorCode, ok := ansiColorCodes[string(codeBytes)]
			if !ok {
				// Don't modify the text if we don't recognize any of the codes
				return groups[0]
			}
			for _, code := range colorCode.GetAnsiCodes() {
				ansiActive.add(code)
				tmp2 = append(tmp2, ansiEscapeBytes(code)...)
			}
		}
		if len(groups[2]) > 0 {
			tmp2 = append(tmp2, groups[3]...)
			tmp2 = append(tmp2, ansiActive.getResetBytes()...)
		}
		return tmp2
	}
	return colorTemplateRegexp.ReplaceAllFunc(buf, colorTemplateReplacer)
}

func (l *Logger) applyColorTemplates(s string) string {
	colorTemplateRegexp := l.getColorTemplateRegexp()
	if colorTemplateRegexp != nil {
		return string(processColorTemplates(colorTemplateRegexp, []byte(s)))
	} else {
		return s
	}
}

func (l *Logger) injectAtVirtualCursor(input []byte) {
	if len(l.buf) != l.cursorByteIndex {
		// Append s to l.buf[:cursorByteIndex], consuming l.buf[cursorByteIndex:] with
		// each rune, but also injecting ansi escapes at the new old/new transition
		// column to keep the colors consistent.
		before := l.buf[:l.cursorByteIndex]
		after := l.buf[l.cursorByteIndex:]
		afterLength := stringLen(after)
		inputLength := stringLen(input)
		if inputLength >= afterLength {
			// We're removing all the after text, so no need to compare colors
			// and/or inject any escapes to heal the after text.
			l.buf = append(before, input...)
			l.cursorByteIndex += len(input)
		} else {
			removed := trimString(after, inputLength)
			ansiOld := getActiveAnsiCodes(append(before, removed...))
			ansiNew := getActiveAnsiCodes(append(before, input...))
			escapes := []byte{}
			changedIntensity := ansiNew.intensity != ansiOld.intensity
			changedForecolor := ansiNew.forecolor != ansiOld.forecolor
			if changedIntensity {
				escapes = append(escapes, ansiBytesResetAll...)
			} else if changedForecolor {
				escapes = append(escapes, ansiBytesResetForecolor...)
			}
			if changedIntensity && ansiOld.intensity != 0 {
				escapes = append(escapes, ansiEscapeBytes(ansiOld.intensity)...)
			}
			if (changedIntensity || changedForecolor) && ansiOld.forecolor != 0 {
				escapes = append(escapes, ansiEscapeBytes(ansiOld.forecolor)...)
			}
			afterKept := append(escapes, after[len(removed):]...)
			l.buf = append(before, input...)
			l.cursorByteIndex += len(input)
			l.buf = append(l.buf, afterKept...) // Don't advance cursor for this part
		}
	} else {
		l.buf = append(l.buf, input...)
		l.cursorByteIndex += len(input)
	}
}

func (l *Logger) Output(calldepth int, _s string) error {
	return l.intOutput(calldepth+1, []byte(_s), false)
}

// Output writes the output for a logging event.  The string s contains
// the text to print after the prefix specified by the flags of the
// Logger.  A newline is appended if the last character of s is not
// already a newline.  Calldepth is used to recover the PC and is
// provided for generality, although at the moment on all pre-defined
// paths it will be 2.
func (l *Logger) intOutput(calldepth int, s []byte, haveLock bool) error {
	l.now = time.Now() // get this early.
	if l.flag&LUTC != 0 {
		l.now = l.now.UTC()
	}
	ws := getWriterState(l.out)
	if !haveLock {
		ws.lock()
		defer ws.unlock()
	}
	if l.isClosed {
		return errors.New("Attempted to write to closed Logger.")
	}
	// This is kind of kludgy, but better than nothing:
	s = []byte(strings.Replace(string(s), "\t", "        ", -1))
	if l.isAutoNewlineEnabled() && len(s) > 0 && s[len(s)-1] != byteNewline {
		s = append(s, byteNewline)
	}
	l.injectAtVirtualCursor(s)
	wroteFullLine := false
	for true {
		indexNewline := bytes.IndexByte(l.buf, '\n')
		var currLine []byte
		if indexNewline == -1 {
			currLine = l.buf
		} else {
			currLine = l.buf[:indexNewline]
		}
		indexCr := bytes.IndexByte(currLine, '\r')
		// Kludge: ignore carriage return just before newline:
		if indexCr != -1 && indexCr != indexNewline-1 {
			// For every carriage return found within the current line, detach the text
			// after the carriage return and inject at the beginning of the line.
			after := l.buf[indexCr+1:]
			l.buf = l.buf[:indexCr]
			l.cursorByteIndex = 0
			l.injectAtVirtualCursor(after)
			continue
		}
		if indexNewline == -1 {
			break
		}
		l.buf = l.buf[indexNewline+1:]
		l.cursorByteIndex -= indexNewline + 1
		if l.flag&(Lshortfile|Llongfile) != 0 && len(l.callerFile) == 0 {
			// release lock while getting caller info - it's expensive.
			if !haveLock {
				ws.unlock()
			}
			var ok bool
			_, l.callerFile, l.callerLine, ok = runtime.Caller(calldepth)
			if !ok {
				l.callerFile = "???"
				l.callerLine = 0
			}
			if l.flag&Lshortfile != 0 {
				for i := len(l.callerFile) - 1; i > 0; i-- {
					if l.callerFile[i] == '/' {
						l.callerFile = l.callerFile[i+1:]
						break
					}
				}
			}
			if !haveLock {
				ws.lock()
			}
		}
		// ansiActive := getActiveAnsiCodes(currLine)
		ws.removeTempLogger(l)
		l.tempLineActive = false
		writeLine(l.out, l.getFormattedLine(currLine))
		wroteFullLine = true
		// // XXX This is probably inefficient?:
		// prepends := []byte{}
		// if ansiActive.intensity != 0 {
		//     prepends = append(prepends, ansiEscapeBytes(ansiActive.intensity)...)
		// }
		// if ansiActive.forecolor != 0 {
		//     prepends = append(prepends, ansiEscapeBytes(ansiActive.forecolor)...)
		// }
		// if len(prepends) > 0 {
		//     l.buf = append(prepends, l.buf...)
		//     l.cursorByteIndex += len(prepends)
		// }
	}
	if wroteFullLine {
		l.callerFile = ""
		l.callerLine = 0
	}
	if !l.tempLineActive && l.isPartialLinesEnabled() && stringLen(l.buf) > 0 {
		ws.addTempLogger(l)
		l.tempLineActive = true
		l.lineStartTime = l.now
	}
	updateTempOutput(l.out)
	return nil
}

func (l *Logger) truncateBuf() {
	l.buf = l.buf[:0]
	l.cursorByteIndex = 0
}

// Printf calls l.Output to print to the logger.
// Arguments are handled in the manner of fmt.Printf.
func (l *Logger) Printf(format string, v ...interface{}) {
	ws := getWriterState(l.out)
	ws.lock()
	defer ws.unlock()
	l.intOutput(2, []byte(fmt.Sprintf(l.applyColorTemplates(format), v...)), true)
}

// Print calls l.Output to print to the logger.
// Arguments are handled in the manner of fmt.Print.
func (l *Logger) Print(v ...interface{}) { l.intOutput(2, []byte(fmt.Sprint(v...)), false) }

func (l *Logger) Replacef(format string, v ...interface{}) {
	ws := getWriterState(l.out)
	ws.lock()
	defer ws.unlock()
	l.truncateBuf()
	l.intOutput(2, []byte(fmt.Sprintf(l.applyColorTemplates(format), v...)), true)
}

func (l *Logger) Replace(v ...interface{}) {
	ws := getWriterState(l.out)
	ws.lock()
	defer ws.unlock()
	l.truncateBuf()
	l.intOutput(2, []byte(fmt.Sprint(v...)), true)
}

// Println calls l.intOutput to print to the logger.
// Arguments are handled in the manner of fmt.Println.
func (l *Logger) Println(v ...interface{}) { l.intOutput(2, []byte(fmt.Sprintln(v...)), false) }

// Fatal is equivalent to l.Print() followed by a call to os.Exit(1).
func (l *Logger) Fatal(v ...interface{}) {
	l.intOutput(2, []byte(fmt.Sprint(v...)), false)
	osExit()
}

// Fatalf is equivalent to l.Printf() followed by a call to os.Exit(1).
func (l *Logger) Fatalf(format string, v ...interface{}) {
	ws := getWriterState(l.out)
	ws.lock()
	l.intOutput(2, []byte(fmt.Sprintf(l.applyColorTemplates(format), v...)), true)
	ws.unlock()
	osExit()
}

// Fatalln is equivalent to l.Println() followed by a call to os.Exit(1).
func (l *Logger) Fatalln(v ...interface{}) {
	l.intOutput(2, []byte(fmt.Sprintln(v...)), false)
	osExit()
}

// Panic is equivalent to l.Print() followed by a call to panic().
func (l *Logger) Panic(v ...interface{}) {
	s := fmt.Sprint(v...)
	l.intOutput(2, []byte(s), false)
	l.flushInt()
	panic(s)
}

// Panicf is equivalent to l.Printf() followed by a call to panic().
func (l *Logger) Panicf(format string, v ...interface{}) {
	ws := getWriterState(l.out)
	ws.lock()
	s := fmt.Sprintf(l.applyColorTemplates(format), v...)
	l.intOutput(2, []byte(s), true)
	l.flushInt()
	ws.unlock()
	panic(s)
}

// Panicln is equivalent to l.Println() followed by a call to panic().
func (l *Logger) Panicln(v ...interface{}) {
	s := fmt.Sprintln(v...)
	l.intOutput(2, []byte(s), false)
	panic(s)
}

func (l *Logger) Bail(err error) {
	// This works best if l.out == os.Stderr, but it should kind of work regardless
	ws := getWriterState(l.out)
	ws.lock()
	l.flushInt()
	size := 4096
	for {
		buf := make([]byte, size)
		bytesWritten := runtime.Stack(buf, false)
		if bytesWritten == size {
			size *= 2
			continue
		}
		stackTrace := strings.TrimSpace(string(buf[:bytesWritten]))
		lines := strings.Split(stackTrace, "\n")
		for i, line := range lines {
			// Try to cut out the Bail() call from the stack trace:
			if (i == 1 || i == 3) && strings.Contains(line, "ansi-log.(*Logger).Bail") {
				continue
			}
			if (i == 2 || i == 4) && strings.Contains(line, "tillberg/ansi-log/log.go") {
				continue
			}
			l.intOutput(2, []byte(line+"\n"), true)
		}
		break
	}
	l.intOutput(2, []byte(fmt.Sprintf("Bailed due to error: %s\n", err.Error())), true)
	ws.unlock()
	osExit()
}

// Flags returns the output flags for the logger.
func (l *Logger) Flags() int {
	ws := getWriterState(l.out)
	ws.lock()
	defer ws.unlock()
	return l.flag
}

// SetFlags sets the output flags for the logger.
func (l *Logger) SetFlags(flag int) {
	ws := getWriterState(l.out)
	ws.lock()
	defer ws.unlock()
	l.flag = flag
}

// Prefix returns the output prefix for the logger.
func (l *Logger) Prefix() string {
	ws := getWriterState(l.out)
	ws.lock()
	defer ws.unlock()
	return string(l.prefix)
}

// SetPrefix sets the output prefix for the logger.
func (l *Logger) SetPrefix(prefix string) {
	ws := getWriterState(l.out)
	ws.lock()
	defer ws.unlock()
	l.prefix = []byte(prefix)
	l.reprocessPrefix()
}

func (l *Logger) Write(p []byte) (n int, err error) {
	err = l.intOutput(2, p, false)
	return len(p), err
}

func (l *Logger) Colorify(s string) string {
	ws := getWriterState(l.out)
	ws.lock()
	defer ws.unlock()
	return l.applyColorTemplates(s)
}

func (l *Logger) flushInt() {
	if len(l.buf) > 0 {
		l.intOutput(2, []byte("\n"), true)
	}
}

func (l *Logger) closeInt() {
	l.isClosed = true
}

func (l *Logger) Flush() {
	ws := getWriterState(l.out)
	ws.lock()
	defer ws.unlock()
	l.flushInt()
}

func (l *Logger) Close() error {
	ws := getWriterState(l.out)
	ws.lock()
	defer ws.unlock()
	l.flushInt()
	l.closeInt()
	return nil
}

func (l *Logger) SetPartialLinesEnabled(flag bool) {
	ws := getWriterState(l.out)
	ws.lock()
	defer ws.unlock()
	l.partialLinesEnabled = boolPointer(flag)
}
func (l *Logger) ShowPartialLines() { l.SetPartialLinesEnabled(true) }
func (l *Logger) HidePartialLines() { l.SetPartialLinesEnabled(false) }

func (l *Logger) SetColorEnabled(flag bool) {
	ws := getWriterState(l.out)
	ws.lock()
	defer ws.unlock()
	l.colorEnabled = boolPointer(flag)
}
func (l *Logger) EnableColor()  { l.SetColorEnabled(true) }
func (l *Logger) DisableColor() { l.SetColorEnabled(false) }

func (l *Logger) SetColorTemplateEnabled(flag bool) {
	ws := getWriterState(l.out)
	ws.lock()
	defer ws.unlock()
	l.colorTemplateEnabled = boolPointer(flag)
	l.reprocessPrefix()
}
func (l *Logger) EnableColorTemplate()  { l.SetColorTemplateEnabled(true) }
func (l *Logger) DisableColorTemplate() { l.SetColorTemplateEnabled(false) }

func (l *Logger) SetAutoNewlines(flag bool) {
	ws := getWriterState(l.out)
	ws.lock()
	defer ws.unlock()
	l.autoAppendNewline = boolPointer(flag)
}
func (l *Logger) EnableAutoNewlines()  { l.SetAutoNewlines(true) }
func (l *Logger) DisableAutoNewlines() { l.SetAutoNewlines(false) }

func (l *Logger) SetColorTemplateRegexp(rgx *regexp.Regexp) {
	ws := getWriterState(l.out)
	ws.lock()
	defer ws.unlock()
	l.colorRegexp = rgx
}

func (l *Logger) SetTerminalWidth(width int) {
	ws := getWriterState(l.out)
	ws.lock()
	defer ws.unlock()
	getWriterState(l.out).flushAll()
	getWriterState(l.out).termWidth = width
}

func (l *Logger) SetMultilineEnabled(flag bool) {
	ws := getWriterState(l.out)
	ws.lock()
	defer ws.unlock()
	getWriterState(l.out).flushAll()
	getWriterState(l.out).multiline = flag
}
func (l *Logger) EnableMultilineMode()  { l.SetMultilineEnabled(true) }
func (l *Logger) EnableSinglelineMode() { l.SetMultilineEnabled(false) }

// func (l *Logger) SetColorTemplate(str string) {
//     var rgx = str.replace
//     l.SetColorTemplateRegexp
// }

// SetOutput sets the output destination for the standard logger.
func SetOutput(w io.Writer) {
	ws := getWriterState(DefaultLogger.out)
	ws.lock()
	defer ws.unlock()
	DefaultLogger.flushInt()
	DefaultLogger.out = w
}

// Flags returns the output flags for the standard logger.
func Flags() int {
	return DefaultLogger.Flags()
}

// SetFlags sets the output flags for the standard logger.
func SetFlags(flag int) {
	DefaultLogger.SetFlags(flag)
}

// Prefix returns the output prefix for the standard logger.
func Prefix() string {
	return DefaultLogger.Prefix()
}

// SetPrefix sets the output prefix for the standard logger.
func SetPrefix(prefix string) {
	DefaultLogger.SetPrefix(prefix)
}

// These functions write to the standard logger.

// Print calls Output to print to the standard logger.
// Arguments are handled in the manner of fmt.Print.
func Print(v ...interface{}) {
	DefaultLogger.intOutput(2, []byte(fmt.Sprint(v...)), false)
}

// Printf calls Output to print to the standard logger.
// Arguments are handled in the manner of fmt.Printf.
func Printf(format string, v ...interface{}) {
	ws := getWriterState(DefaultLogger.out)
	ws.lock()
	defer ws.unlock()
	DefaultLogger.intOutput(2, []byte(fmt.Sprintf(DefaultLogger.applyColorTemplates(format), v...)), true)
}

func Replace(v ...interface{}) {
	ws := getWriterState(DefaultLogger.out)
	ws.lock()
	defer ws.unlock()
	DefaultLogger.truncateBuf()
	DefaultLogger.intOutput(2, []byte(fmt.Sprint(v...)), true)
}

func Replacef(format string, v ...interface{}) {
	ws := getWriterState(DefaultLogger.out)
	ws.lock()
	defer ws.unlock()
	DefaultLogger.truncateBuf()
	DefaultLogger.intOutput(2, []byte(fmt.Sprintf(DefaultLogger.applyColorTemplates(format), v...)), true)
}

// Println calls Output to print to the standard logger.
// Arguments are handled in the manner of fmt.Println.
func Println(v ...interface{}) {
	DefaultLogger.intOutput(2, []byte(fmt.Sprintln(v...)), false)
}

// Fatal is equivalent to Print() followed by a call to os.Exit(1).
func Fatal(v ...interface{}) {
	DefaultLogger.intOutput(2, []byte(fmt.Sprint(v...)), false)
	osExit()
}

// Fatalf is equivalent to Printf() followed by a call to os.Exit(1).
func Fatalf(format string, v ...interface{}) {
	ws := getWriterState(DefaultLogger.out)
	ws.lock()
	DefaultLogger.intOutput(2, []byte(fmt.Sprintf(DefaultLogger.applyColorTemplates(format), v...)), true)
	ws.unlock()
	osExit()
}

// Fatalln is equivalent to Println() followed by a call to os.Exit(1).
func Fatalln(v ...interface{}) {
	DefaultLogger.intOutput(2, []byte(fmt.Sprintln(v...)), false)
	osExit()
}

// Panic is equivalent to Print() followed by a call to panic().
func Panic(v ...interface{}) {
	s := fmt.Sprint(v...)
	DefaultLogger.intOutput(2, []byte(s), false)
	DefaultLogger.flushInt()
	panic(s)
}

// Panicf is equivalent to Printf() followed by a call to panic().
func Panicf(format string, v ...interface{}) {
	ws := getWriterState(DefaultLogger.out)
	ws.lock()
	s := fmt.Sprintf(DefaultLogger.applyColorTemplates(format), v...)
	DefaultLogger.intOutput(2, []byte(s), true)
	DefaultLogger.flushInt()
	ws.unlock()
	panic(s)
}

// Panicln is equivalent to Println() followed by a call to panic().
func Panicln(v ...interface{}) {
	s := fmt.Sprintln(v...)
	DefaultLogger.intOutput(2, []byte(s), false)
	panic(s)
}

func Bail(err error) {
	DefaultLogger.Bail(err)
}

func ShowPartialLines()                         { DefaultLogger.ShowPartialLines() }
func HidePartialLines()                         { DefaultLogger.HidePartialLines() }
func EnableColor()                              { DefaultLogger.EnableColor() }
func DisableColor()                             { DefaultLogger.DisableColor() }
func EnableColorTemplate()                      { DefaultLogger.EnableColorTemplate() }
func DisableColorTemplate()                     { DefaultLogger.DisableColorTemplate() }
func EnableAutoNewlines()                       { DefaultLogger.SetAutoNewlines(true) }
func DisableAutoNewlines()                      { DefaultLogger.SetAutoNewlines(false) }
func SetColorTemplateRegexp(rgx *regexp.Regexp) { DefaultLogger.SetColorTemplateRegexp(rgx) }
func SetTerminalWidth(width int)                { DefaultLogger.SetTerminalWidth(width) }
func EnableMultilineMode()                      { DefaultLogger.EnableMultilineMode() }
func EnableSinglelineMode()                     { DefaultLogger.EnableSinglelineMode() }
func Colorify(s string) string                  { return DefaultLogger.Colorify(s) }

func AddAnsiColorCode(s string, code ColorCode) {
	ansiColorCodes[s] = code
}

func osExit() {
	// Lock everything and hold the locks permanently. Close (and flush) all Loggers,
	// then exit with error code 1.
	// We only hold an RLock on the global mutex to prevent new Loggers from being
	// added (and mutating the writers map) before we exit. And because use Lock
	// would result in a deadlock when we try to RLock during a flush operation when
	// we try to call getWriterState()
	mutexGlobal.RLock()
	for _, ws := range writers {
		ws.lock()
		ws.closeAll()
	}
	os.Exit(1)
}

// Output writes the output for a logging event.  The string s contains
// the text to print after the prefix specified by the flags of the
// Logger.  A newline is appended if the last character of s is not
// already a newline.  Calldepth is the count of the number of
// frames to skip when computing the file name and line number
// if Llongfile or Lshortfile is set; a value of 1 will print the details
// for the caller of Output.
func Output(calldepth int, s string) error {
	return DefaultLogger.Output(calldepth+1, s) // +1 for this frame.
}
