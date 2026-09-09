package main

import (
	"strings"

	"rsc.io/qr"
)

// terminalQR renders text as a QR code using half-block characters, two
// module rows per line, dark modules drawn in the foreground colour (the
// same polarity as qrencode's UTF-8 output). Phones decode either polarity.
func terminalQR(text string) (string, error) {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return "", err
	}
	const quiet = 2
	n := code.Size
	size := n + 2*quiet
	dark := func(x, y int) bool {
		x -= quiet
		y -= quiet
		return x >= 0 && y >= 0 && x < n && y < n && code.Black(x, y)
	}
	var sb strings.Builder
	for y := 0; y < size; y += 2 {
		for x := 0; x < size; x++ {
			top, bottom := dark(x, y), dark(x, y+1)
			switch {
			case top && bottom:
				sb.WriteString("█")
			case top:
				sb.WriteString("▀")
			case bottom:
				sb.WriteString("▄")
			default:
				sb.WriteString(" ")
			}
		}
		sb.WriteByte('\n')
	}
	return sb.String(), nil
}
