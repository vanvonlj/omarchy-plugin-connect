// Package qr renders a payload as a matrix the Omarchy shell can draw.
package qr

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Matrix returns the QR for payload as rows of '0' and '1'.
//
// The format matches omarchy-network-qr exactly, because the shell already
// knows how to draw it: qrencode's ASCII output uses two characters per module,
// so each pair collapses to one value and the panel renders native QML
// rectangles rather than decoding an image.
//
// Margin 4 is the spec quiet zone. It is not decoration -- a QR drawn flush to
// a dark panel edge is one many scanners will not see.
func Matrix(payload string) ([]string, error) {
	if payload == "" {
		return nil, errors.New("nothing to encode")
	}

	cmd := exec.Command("qrencode", "--type", "ASCII", "--margin", "4", "--output", "-")
	cmd.Stdin = strings.NewReader(payload)

	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("qrencode: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("running qrencode (is it installed?): %w", err)
	}

	var rows []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		var row strings.Builder
		for i := 0; i+1 < len(line) || i < len(line); i += 2 {
			end := i + 2
			if end > len(line) {
				end = len(line)
			}
			if strings.Contains(line[i:end], "#") {
				row.WriteByte('1')
			} else {
				row.WriteByte('0')
			}
		}
		rows = append(rows, row.String())
	}
	return rows, nil
}

// Terminal renders the matrix using half-block characters, two rows per line,
// so a QR printed in a terminal stays square instead of twice as tall as it is
// wide -- which is the difference between a scannable code and a stretched one.
func Terminal(rows []string) string {
	const (
		full  = "█"
		upper = "▀"
		lower = "▄"
		blank = " "
	)

	var b strings.Builder
	for y := 0; y < len(rows); y += 2 {
		top := rows[y]
		var bottom string
		if y+1 < len(rows) {
			bottom = rows[y+1]
		} else {
			bottom = strings.Repeat("0", len(top))
		}

		for x := 0; x < len(top); x++ {
			// Inverted: a QR needs dark modules on a light field, and a
			// terminal's default background is usually dark.
			t := x < len(top) && top[x] == '0'
			bo := x < len(bottom) && bottom[x] == '0'

			switch {
			case t && bo:
				b.WriteString(full)
			case t:
				b.WriteString(upper)
			case bo:
				b.WriteString(lower)
			default:
				b.WriteString(blank)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
