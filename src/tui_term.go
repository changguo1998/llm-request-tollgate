// tui_term.go: 终端原始模式、画布与显示宽度。

package main

import (
	"bytes"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"
	"unsafe"
)

const (
	tuiTCGETS     = 0x5401
	tuiTCSETS     = 0x5402
	tuiTIOCGWINSZ = 0x5413
	tuiFlagIflag  = 0x1 // BRKINT
	tuiFlagIcrnl  = 0x100
	tuiFlagOpost  = 0x1
	tuiFlagEcho   = 0x8
	tuiFlagIcanon = 0x2
	tuiFlagIsig   = 0x1
	tuiFlagIexten = 0x8000
	tuiFlagVmin   = 6
	tuiFlagVtime  = 5
)

type tuiTermios struct {
	Iflag  uint32
	Oflag  uint32
	Cflag  uint32
	Lflag  uint32
	Line   uint8
	Cc     [19]uint8
	Ispeed uint32
	Ospeed uint32
}

type tuiWinsize struct{ Row, Col, Xpixel, Ypixel uint16 }

func tuiIoctl(fd uintptr, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// tuiRaw 关掉行缓冲 / 回显 / 信号键, 读满 1 字节即返回 (VMIN=1)。
func tuiRaw(fd uintptr) (*tuiTermios, error) {
	var old tuiTermios
	if err := tuiIoctl(fd, tuiTCGETS, unsafe.Pointer(&old)); err != nil {
		return nil, err
	}
	raw := old
	raw.Iflag &^= tuiFlagIcrnl | tuiFlagIflag
	raw.Oflag &^= tuiFlagOpost
	raw.Lflag &^= tuiFlagEcho | tuiFlagIcanon | tuiFlagIsig | tuiFlagIexten
	raw.Cc[tuiFlagVmin], raw.Cc[tuiFlagVtime] = 1, 0
	if err := tuiIoctl(fd, tuiTCSETS, unsafe.Pointer(&raw)); err != nil {
		return nil, err
	}
	return &old, nil
}

func tuiRestore(fd uintptr, t *tuiTermios) {
	if t != nil {
		tuiIoctl(fd, tuiTCSETS, unsafe.Pointer(t))
	}
}

// tuiSize 取终端行列; 失败时给 100x30, 保证还能画出东西。
func tuiSize() (int, int) {
	ws := tuiWinsize{}
	if tuiIoctl(os.Stdout.Fd(), tuiTIOCGWINSZ, unsafe.Pointer(&ws)) == nil && ws.Col > 0 {
		return int(ws.Col), int(ws.Row)
	}
	return 100, 30
}

type tuiCanvas struct {
	b    bytes.Buffer
	cols int
}

func (c *tuiCanvas) line(s string) {
	if tuiWidth(s) > c.cols {
		s = tuiTrunc(s, c.cols)
	}
	c.b.WriteString(s)
	c.b.WriteString("\x1b[K\r\n")
}

// ansiSpan 从 s[i] (ESC) 起量出一个 ANSI 序列的长度; 不是序列就返回 0。
// 只处理 CSI (ESC [ ... 终止字节 @-~), 本文件画面上用的全是这种。
func ansiSpan(s string, i int) int {
	if s[i] != 0x1b || i+1 >= len(s) || s[i+1] != '[' {
		return 0
	}
	j := i + 2
	for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
		j++
	}
	if j >= len(s) {
		return len(s) - i
	}
	return j + 1 - i
}

// tuiWidth 是显示宽度: ASCII 1 列, CJK 全角 2 列, ANSI 序列 0 列。
// 不这么做的话, 中文列会把表头挤歪半行。
func tuiWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if n := ansiSpan(s, i); n > 0 {
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			i++
			w++
			continue
		}
		w += runeWidth(r)
		i += size
	}
	return w
}

// runeWidth 只区分"窄/宽"两档, 够表格对齐用 (不引入 wcwidth 依赖)。
func runeWidth(r rune) int {
	switch {
	case r < 0x1100:
		return 1
	case r >= 0x1100 && r <= 0x115f, // Hangul Jamo
		r >= 0x2e80 && r <= 0xa4cf, // CJK 部首 / 假名 / 注音
		r >= 0xac00 && r <= 0xd7a3, // Hangul 音节
		r >= 0xf900 && r <= 0xfaff, // CJK 兼容汉字
		r >= 0xfe30 && r <= 0xfe6f, // CJK 兼容形式
		r >= 0xff00 && r <= 0xff60, // 全角 ASCII
		r >= 0xffe0 && r <= 0xffe6,
		r >= 0x20000 && r <= 0x3fffd: // 扩展区汉字
		return 2
	}
	return 1
}

// tuiPad 补到 w 列 (不截断; 需要截断先 tuiTrunc)。
func tuiPad(s string, w int) string {
	if n := tuiWidth(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

// tuiPadLeft 左补空格到 w 列 (右对齐显示用, 不截断)。
func tuiPadLeft(s string, w int) string {
	if n := tuiWidth(s); n < w {
		return strings.Repeat(" ", w-n) + s
	}
	return s
}

// tuiTrunc 截到 w 列以内 (只看可见字符, ANSI 序列不占列宽); 截断时末尾留 ~。
func tuiTrunc(s string, w int) string {
	if tuiWidth(s) <= w {
		return s
	}
	limit := w
	if w > 1 {
		limit = w - 1
	}
	var b strings.Builder
	used, cut := 0, false
	for i := 0; i < len(s); {
		if n := ansiSpan(s, i); n > 0 { // 序列原样带走, 不计宽
			b.WriteString(s[i : i+n])
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := runeWidth(r)
		if used+rw > limit {
			cut = true
			break
		}
		b.WriteString(s[i : i+size])
		used += rw
		i += size
	}
	if cut && w > 1 {
		b.WriteString("~")
	}
	if cut && strings.Contains(s, "\x1b[") {
		// 截断会把末尾的重置序列一起切掉, 补一个, 免得颜色串到后面的列。
		b.WriteString(tuiReset)
	}
	return b.String()
}
