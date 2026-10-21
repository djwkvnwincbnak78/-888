package provider

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// sseEvent is one dispatched server-sent event.
type sseEvent struct {
	Event string
	Data  string
}

// readSSE parses an SSE stream until EOF. Data lines are joined with "\n" as
// required by the spec; a blank line dispatches the accumulated event.
// The callback may return io.EOF to stop early.
func readSSE(r io.Reader, fn func(sseEvent) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var event string
	var data []string

	dispatch := func() error {
		if len(data) == 0 && event == "" {
			return nil
		}
		ev := sseEvent{Event: event, Data: strings.Join(data, "\n")}
		event, data = "", nil
		if err := fn(ev); err != nil {
			if errors.Is(err, io.EOF) {
				return io.EOF
			}
			return err
		}
		return nil
	}

	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if derr := dispatch(); derr != nil {
					if errors.Is(derr, io.EOF) {
						return nil
					}
					return derr
				}
			case strings.HasPrefix(line, ":"):
				// comment / heartbeat
			default:
				field, value, found := strings.Cut(line, ":")
				if !found {
					field, value = line, ""
				}
				value = strings.TrimPrefix(value, " ")
				switch field {
				case "event":
					event = value
				case "data":
					data = append(data, value)
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if derr := dispatch(); derr != nil && !errors.Is(derr, io.EOF) {
					return derr
				}
				return nil
			}
			return fmt.Errorf("sse read: %w", err)
		}
	}
}

// maxErrorBody bounds how much of an error response we keep in memory.
const maxErrorBody = 64 << 10
