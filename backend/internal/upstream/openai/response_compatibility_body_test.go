package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResponsesCompatibilityBounds exercises each resource ceiling separately.
// It checks rejection one byte or one frame above the budget and delivery of a
// valid body exactly at the configured total size.
func TestResponsesCompatibilityBounds(t *testing.T) {
	terminal := compatFrame("response.completed", compatCompleted)
	heartbeat := "event: ping\ndata: {}\n\n"
	frameLine := "data: " + strings.Repeat(" ", ResponsesCompatibilityMaxLineBytes-7) + "\n"
	for _, tc := range []struct {
		name, response, wantError string
	}{
		{"body-at-limit", compatCompleted + strings.Repeat(" ", ResponsesCompatibilityMaxBodyBytes-len(compatCompleted)), ""},
		{"body-over-limit", compatCompleted + strings.Repeat(" ", ResponsesCompatibilityMaxBodyBytes-len(compatCompleted)+1), "body exceeds the byte limit"},
		{"line-at-limit", ":" + strings.Repeat(" ", ResponsesCompatibilityMaxLineBytes-2) + "\n" + terminal, ""},
		{"line-over-limit", terminal + ":" + strings.Repeat(" ", ResponsesCompatibilityMaxLineBytes-1) + "\n", "line byte limit"},
		{"frame-over-limit", terminal + strings.Repeat(frameLine, 4) + "\n", "frame byte limit"},
		{"count-at-limit", strings.Repeat(heartbeat, ResponsesCompatibilityMaxFrames-1) + terminal, ""},
		{"count-over-limit", terminal + strings.Repeat(heartbeat, ResponsesCompatibilityMaxFrames), "frame count limit"},
		{"position-over-limit", compatFrame("", `{"type":"response.output_item.done","output_index":4096,"item":{"type":"message"}}`) + terminal, "invalid output or content index"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver := newResponsesStreamResolver(new(ResponsesStreamAttempt))
			eof, readErr := readResponsesCompatibilityBody(strings.NewReader(tc.response), resolver, 200)
			body, _, err := resolver.resolve(eof, readErr, 200)
			if tc.wantError == "" {
				require.NoError(t, err)
				require.NotEmpty(t, body)
				require.True(t, eof)
				return
			}
			var failure *ResponsesStreamError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, 502, failure.StatusCode)
			require.ErrorContains(t, err, tc.wantError)
			require.Empty(t, body)
		})
	}
}

// TestResponsesCompatibilityPhysicalEOF checks a short HTTP Content-Length
// body that delivered a complete terminal. Transport truncation still
// invalidates publication even when the SSE parser reached a blank line.
func TestResponsesCompatibilityPhysicalEOF(t *testing.T) {
	terminal := compatFrame("response.completed", compatCompleted)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(terminal)+1))
		_, _ = io.WriteString(w, terminal)
	}))
	defer server.Close()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer func() { assert.NoError(t, response.Body.Close()) }()

	resolver := newResponsesStreamResolver(new(ResponsesStreamAttempt))
	eof, readErr := readResponsesCompatibilityBody(response.Body, resolver, response.StatusCode)
	require.ErrorIs(t, readErr, io.ErrUnexpectedEOF)
	require.False(t, eof)
	body, _, err := resolver.resolve(eof, readErr, 200)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Empty(t, body)
}
