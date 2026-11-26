package openai

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"

	wire "github.com/TokenFlux/TokenRouter/internal/protocol/openai"
)

// response_compatibility_body.go reads the bounded wire representation for
// synchronous Responses conversions. The body prefix selects JSON or SSE
// even when the upstream sends the wrong media type. JSON uses the body
// budget, while SSE retains only one physical line and one frame at a time.
//
// The shared frame parser supplies joined SSE data at a blank separator. This
// caller deliberately leaves its EOF flush unused because a frame without a
// separator was never committed on the wire. A normal HTTP EOF still has to
// arrive after that separator, even after an upstream terminal or DONE marker.

// The limits govern bytes delivered by the HTTP body and bound per-line,
// per-frame, and per-event reader work before the resolver parses JSON.
const (
	ResponsesCompatibilityMaxBodyBytes  = 16 << 20 // all decoded HTTP body bytes
	ResponsesCompatibilityMaxLineBytes  = 1 << 20  // one SSE line, including ending
	ResponsesCompatibilityMaxFrameBytes = 4 << 20  // one SSE frame, including separator
	ResponsesCompatibilityMaxFrames     = 16384    // SSE frames with application data
	responsesReadBufferBytes            = 32 << 10 // initial physical read buffer
)

// errResponsesBodyLimit distinguishes budget exhaustion from a normal HTTP EOF.
var (
	errResponsesBodyLimit = errors.New("upstream Responses body exceeds the byte limit")
)

// responsesBoundedReader enforces the shared message budget and reports physical reads.
type responsesBoundedReader struct {
	body      io.Reader // borrowed upstream response body
	remaining int       // bytes still permitted
	onRead    func()    // optional read activity hook
}

// readResponsesCompatibilityBody consumes one bounded response and reports
// whether the wire reached a clean EOF. Format selection is based on the first
// non-whitespace byte, so neither a misleading header nor a JSON string with
// SSE-like content can route a complete Response to the wrong parser.
func readResponsesCompatibilityBody(body io.Reader, resolver *responsesStreamResolver, status int) (bool, error) {
	source := &responsesBoundedReader{
		body: body, remaining: ResponsesCompatibilityMaxBodyBytes, onRead: resolver.attempt.OnRead,
	}
	reader := bufio.NewReaderSize(source, responsesReadBufferBytes)
	if prefix, _ := reader.Peek(3); bytes.Equal(prefix, []byte{0xef, 0xbb, 0xbf}) {
		// JSON and SSE can both arrive from relays with a leading UTF-8 BOM.
		// Strip it before recognition while still charging the body budget.
		_, _ = reader.Discard(3)
	}
	linePrefix := 0
	for skipped := 0; ; skipped++ {
		prefix, err := reader.Peek(1)
		if err != nil {
			if err == io.EOF {
				return true, nil
			}
			return false, err
		}
		switch prefix[0] {
		case ' ', '\t', '\r', '\n':
			if skipped == ResponsesCompatibilityMaxLineBytes {
				return false, errors.New("upstream Responses format prefix exceeds the byte limit")
			}
			if prefix[0] == '\n' {
				linePrefix = 0
			} else {
				linePrefix++
			}
			_, _ = reader.Discard(1)
		case '{':
			data, err := io.ReadAll(reader)
			if err != nil {
				return false, err
			}
			// A fully read HTTP error envelope can reject a field before
			// generation without an SSE or Response object around the error.
			if rejection := directResponsesHTTPRejection(status, data); rejection != nil {
				return true, rejection
			}
			resolver.observe("", data)
			return true, nil
		default:
			// SSE fields start at a physical line boundary. Preserving that
			// boundary also prevents format sniffing from bypassing the line limit.
			if linePrefix != 0 {
				return false, errors.New("upstream Responses SSE field does not start at a physical line boundary")
			}
			return readResponsesCompatibilitySSE(reader, resolver)
		}
	}
}

// readResponsesCompatibilitySSE dispatches complete application frames in order.
// Semantic errors are retained by the resolver, which can still receive independent
// evidence from later valid frames. Physical read and size errors stop consumption
// because the next trustworthy framing boundary cannot be established.
func readResponsesCompatibilitySSE(reader *bufio.Reader, resolver *responsesStreamResolver) (bool, error) {
	var parser wire.OpenAICompatSSEFrameParser
	var eventLabel string
	var frameBytes, frameCount int
	for {
		line, err := readResponsesCompatibilityLine(reader, ResponsesCompatibilityMaxFrameBytes-frameBytes)
		if err == io.EOF && len(line) == 0 && frameBytes == 0 {
			return true, nil
		}
		if err == io.EOF {
			return false, errors.New("upstream Responses event stream ended inside a frame")
		}
		if err != nil {
			return false, err
		}
		frameBytes += len(line)
		text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
		if text == "" {
			if frame, ok := parser.AddLine(""); ok {
				frameCount++
				if frameCount > ResponsesCompatibilityMaxFrames {
					return false, errors.New("upstream Responses stream exceeds the frame count limit")
				}
				resolver.observe(frame.EventType, []byte(frame.Data))
			} else if eventLabel != "" && !upstreamHeartbeatLabel(eventLabel) {
				resolver.reject(errors.New("upstream Responses event has no data"))
			}
			frameBytes = 0
			eventLabel = ""
			continue
		}
		if label, ok := wire.ExtractSSEEventLine(text); ok {
			if eventLabel != "" && label != eventLabel {
				resolver.reject(errors.New("upstream Responses frame has conflicting event labels"))
			}
			eventLabel = label
		} else if _, ok := wire.ExtractSSEDataLine(text); !ok &&
			!strings.HasPrefix(text, ":") && !strings.HasPrefix(text, "id:") && !strings.HasPrefix(text, "retry:") {
			resolver.reject(errors.New("upstream Responses body contains an invalid event-stream line"))
		}
		parser.AddLine(text)
		// A newline-terminated comment is complete without a separator when no
		// application fields are pending. Heartbeats must not keep frame bytes alive.
		if frameBytes == len(line) && strings.HasPrefix(text, ":") {
			frameBytes = 0
		}
	}
}

// readResponsesCompatibilityLine reads one bounded physical line and keeps its
// terminator for exact accounting. A fragmented line is copied incrementally;
// the reader checks both remaining frame space and line space before growing it.
func readResponsesCompatibilityLine(reader *bufio.Reader, frameRemaining int) ([]byte, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > ResponsesCompatibilityMaxLineBytes {
			return nil, errors.New("upstream Responses stream exceeds the line byte limit")
		}
		if len(line)+len(fragment) > frameRemaining {
			return nil, errors.New("upstream Responses stream exceeds the frame byte limit")
		}
		if len(line) == 0 && err != bufio.ErrBufferFull {
			return fragment, err
		}
		line = append(line, fragment...)
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}

// Read forwards bounded physical bytes and observes their arrival synchronously.
// At the exact budget, a one-byte probe distinguishes EOF from an oversized body
// without exposing the extra byte to the parser.
func (r *responsesBoundedReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		var extra [1]byte
		n, err := r.body.Read(extra[:])
		if n != 0 {
			return 0, errResponsesBodyLimit
		}
		return 0, err
	}
	n, err := r.body.Read(p[:min(len(p), r.remaining)])
	r.remaining -= n
	if n > 0 && r.onRead != nil {
		r.onRead()
	}
	return n, err
}

// upstreamHeartbeatLabel identifies data-less heartbeat frames allowed by the reader.
func upstreamHeartbeatLabel(label string) bool {
	return label == "ping" || label == "heartbeat" || label == "keepalive"
}
