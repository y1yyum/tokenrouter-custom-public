package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/internal/upstream"
)

// These fixtures share one Response ID and effective output so unrelated
// differences in each case isolate its terminal, identity, or metering rule.
const (
	compatMessage   = `{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`
	compatCompleted = `{"object":"response","id":"resp_1","status":"completed","model":"upstream-model","output":[` + compatMessage + `]}`
	compatPartial   = `{"object":"response","id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[` + compatMessage + `]}`
	compatUsage     = `{"type":"response.usage","response_id":"resp_1","service_tier":"priority","usage":{"input_tokens":7,"output_tokens":2}}`
	compatOverload  = `{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"server_is_overloaded","message":"busy"}}}`
	compatProof     = `{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"server_is_overloaded","message":"busy","execution_started":false}}}`
)

// compatErrorReader ends a body with a transport error instead of EOF.
type compatErrorReader struct {
	err error // injected transport read error
}

// TestResponsesCompatibilityBothEntries receives real HTTP bodies through
// both buffered adapters. Chunked CRLF events and mislabeled JSON must produce
// one JSON delivery with the same validated response, usage, and model.
func TestResponsesCompatibilityBothEntries(t *testing.T) {
	conversion := []string{
		": heartbeat\r\n\r\n",
		"event: response.created\r\ndata: {\"object\":\"response\",\"id\":\"resp_1\",\"status\":\"in_progress\",\"service_tier\":\"auto\"}\r\n\r\n",
		compatFrame("", `{"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"content_index":2,"delta":"second"}`),
		compatFrame("", `{"type":"response.refusal.delta","item_id":"msg_1","output_index":1,"content_index":3,"delta":"declined"}`),
		compatFrame("", `{"type":"response.output_item.added","output_index":0,"item":{"id":"tool_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"","status":"in_progress"}}`),
		compatFrame("", `{"type":"response.function_call_arguments.delta","item_id":"tool_1","delta":"{\"x\":"}`),
		compatFrame("", `{"type":"response.function_call_arguments.delta","call_id":"call_1","delta":"1}"}`),
		compatFrame("", `{"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"first"}`),
		compatFrame("", compatUsage),
		"event: response.completed\r\ndata: {\"object\":\"response\",\"id\":\"resp_1\",\r\ndata: \"status\":\"completed\",\"model\":\"actual-model\",\"output\":[]}\r\n\r\n",
		compatFrame("response.done", `{"type":"response.completed","response":{"id":"resp_1","status":"completed","service_tier":"fast","usage":{"output_tokens":2}}}`),
		"data: [DONE]\n\n: ended\n",
	}
	for _, passthrough := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "rewritable"}[passthrough], func(t *testing.T) {
			for _, tc := range []struct {
				name, mediaType string
				chunks          []string
			}{
				{"fragmented", "application/json", conversion},
				{"direct", "text/event-stream", []string{compatCompleted}},
				{"missing-type", "", []string{compatFrame("response.completed", compatCompleted)}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.Header()["Content-Type"] = nil
						if tc.mediaType != "" {
							w.Header().Set("Content-Type", tc.mediaType)
						}
						controller := http.NewResponseController(w)
						for _, chunk := range tc.chunks {
							for chunk != "" {
								n := min(19, len(chunk))
								if _, err := io.WriteString(w, chunk[:n]); err != nil {
									t.Errorf("write upstream response fragment: %v", err)
									return
								}
								if err := controller.Flush(); err != nil {
									t.Errorf("flush upstream response fragment: %v", err)
									return
								}
								chunk = chunk[n:]
							}
						}
					}))
					defer server.Close()
					req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
					require.NoError(t, err)
					resp, err := server.Client().Do(req)
					require.NoError(t, err)
					defer func() { assert.NoError(t, resp.Body.Close()) }()

					attempt := new(ResponsesStreamAttempt)
					result, sink, err := compatRead(t.Context(), passthrough, resp, attempt)
					require.NoError(t, err)
					require.True(t, result.Served)
					require.Len(t, sink.heads, 1)
					require.Len(t, sink.bodies, 1)
					require.Equal(t, http.StatusOK, sink.heads[0].Status)
					require.Equal(t, "application/json", sink.heads[0].Header.Get("Content-Type"))
					require.Empty(t, sink.heads[0].Header.Get("Content-Length"))
					client := gjson.ParseBytes(sink.bodies[0])
					require.True(t, gjson.ValidBytes(sink.bodies[0]))
					require.Equal(t, "client-model", client.Get("model").String())
					require.Equal(t, "resp_1", result.ResponseID)
					snapshot := attempt.Snapshot()
					require.Equal(t, ResponsesStreamCompleted, snapshot.Resolution)
					require.True(t, snapshot.CleanEOF)
					require.True(t, snapshot.ReplayBlocked)
					if tc.name != "fragmented" {
						require.Equal(t, "hello", client.Get("output.0.content.0.text").String())
						return
					}
					require.Equal(t, "priority", snapshot.ServiceTier)
					require.Equal(t, "priority", client.Get("service_tier").String())
					require.Equal(t, 7, result.Usage.InputTokens)
					require.Equal(t, 2, result.Usage.OutputTokens)
					require.Equal(t, `{"x":1}`, client.Get("output.0.arguments").String())
					require.Equal(t, "call_1", client.Get("output.0.call_id").String())
					require.Equal(t, "first", client.Get("output.1.content.0.text").String())
					require.Equal(t, "second", client.Get("output.1.content.1.text").String())
					require.Equal(t, "declined", client.Get("output.1.content.2.refusal").String())
					require.EqualValues(t, 7, client.Get("usage.input_tokens").Int())
				})
			}
		})
	}
}

// TestResponsesCompatibilityResolution exercises the shared decision in both
// entry points. It checks the downstream commitment, returned evidence and
// repeatability of identity, terminal, and error classification.
func TestResponsesCompatibilityResolution(t *testing.T) {
	completed := compatFrame("response.completed", compatCompleted)
	incomplete := compatFrame("response.incomplete", compatPartial)
	withUsage := strings.Replace(compatCompleted, `"output":`, `"usage":{"input_tokens":7,"output_tokens":2},"output":`, 1)
	withTier := strings.Replace(withUsage, `"output":`, `"service_tier":"priority","output":`, 1)
	empty := `{"id":"resp_1","object":"response","status":"completed","output":[],"usage":{"input_tokens":9}}`
	tool := `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}]}`
	refusal := `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"declined"}]}]}`
	created := compatFrame("response.created", `{"object":"response","id":"resp_1","status":"in_progress","output":[]}`)
	recoveringError := compatFrame("", `{"type":"error","error":{"code":"server_is_overloaded","message":"busy"}}`)
	proof := compatFrame("", compatProof)
	report := compatFrame("", compatUsage)
	for _, passthrough := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "rewritable"}[passthrough], func(t *testing.T) {
			for _, tc := range []struct {
				name, body string
				resolution ResponsesStreamResolution
				status     int
				usage      ResponsesUsageState
				tier       string
				replay     bool
				eof        bool
			}{
				{"equivalent", completed + compatFrame("response.done", compatCompleted), ResponsesStreamCompleted, 200, ResponsesUsageMissing, "", false, true},
				{"incomplete-duplicate", incomplete + compatFrame("response.done", compatPartial), ResponsesStreamIncomplete, 200, ResponsesUsageMissing, "", false, true},
				{"refusal", refusal, ResponsesStreamRefused, 200, ResponsesUsageMissing, "", false, true},
				{"structured-policy-refusal", compatFrame("", `{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"cyber_policy","message":"request refused"}}}`), ResponsesStreamRefused, 400, ResponsesUsageMissing, "", false, true},
				{"tool-only", tool, ResponsesStreamCompleted, 200, ResponsesUsageMissing, "", false, true},
				{"hosted-tool-only", compatFrame("", `{"type":"response.web_search_call.completed","item_id":"search_1","output_index":0}`) + compatFrame("response.completed", empty), ResponsesStreamCompleted, 200, ResponsesUsageReported, "", false, true},
				{"recovered-leading-error", recoveringError + completed, ResponsesStreamCompleted, 200, ResponsesUsageMissing, "", false, true},
				{"standalone-overload", recoveringError, ResponsesStreamFailed, 503, ResponsesUsageMissing, "", false, true},
				{"execution-unknown", created + compatFrame("", compatOverload), ResponsesStreamFailed, 503, ResponsesUsageMissing, "", false, true},
				{"affirmative-capacity-rejection", created + proof, ResponsesStreamFailed, 503, ResponsesUsageMissing, "", true, true},
				{"tool-progress-locks", created + compatFrame("", `{"type":"response.web_search_call.in_progress","output_index":0,"item_id":"search_1"}`) + proof, ResponsesStreamFailed, 503, ResponsesUsageMissing, "", false, true},
				{"text-locks", created + compatFrame("", `{"type":"response.output_text.delta","output_index":0,"delta":"a"}`) + proof, ResponsesStreamFailed, 503, ResponsesUsageMissing, "", false, true},
				{"usage-locks", created + report + proof, ResponsesStreamFailed, 503, ResponsesUsageReported, "priority", false, true},
				{"post-terminal-error", completed + recoveringError, ResponsesStreamProtocolError, 502, ResponsesUsageMissing, "", false, true},
				{"contradictory-terminal", compatFrame("response.completed", withUsage) + incomplete, ResponsesStreamProtocolError, 502, ResponsesUsageUnverified, "", false, true},
				{"independent-survives-conflict", compatFrame("response.completed", withTier) + incomplete + report, ResponsesStreamProtocolError, 502, ResponsesUsageReported, "priority", false, true},
				{"duplicate-usage-conflict", compatFrame("response.completed", withUsage) + compatFrame("response.done", strings.Replace(withUsage, `"input_tokens":7`, `"input_tokens":8`, 1)), ResponsesStreamProtocolError, 502, ResponsesUsageUnverified, "", false, true},
				{"placeholder-zero", compatFrame("response.completed", withUsage) + compatFrame("response.done", strings.Replace(withUsage, `"input_tokens":7,"output_tokens":2`, `"input_tokens":0,"output_tokens":0`, 1)), ResponsesStreamCompleted, 200, ResponsesUsageReported, "", false, true},
				{"explicit-zero", strings.Replace(withUsage, `"input_tokens":7,"output_tokens":2`, `"input_tokens":0,"output_tokens":0`, 1), ResponsesStreamCompleted, 200, ResponsesUsageZero, "", false, true},
				{"invalid-negative", strings.Replace(withUsage, `"input_tokens":7`, `"input_tokens":-7`, 1), ResponsesStreamCompleted, 200, ResponsesUsageUnverified, "", false, true},
				{"tier-conflict", compatFrame("response.completed", withTier) + compatFrame("response.done", strings.Replace(withTier, "priority", "default", 1)), ResponsesStreamProtocolError, 502, ResponsesUsageUnverified, "", false, true},
				{"mixed-id", created + compatFrame("response.completed", strings.Replace(withUsage, "resp_1", "resp_2", 1)), ResponsesStreamProtocolError, 502, ResponsesUsageMissing, "", false, true},
				{"mixed-id-explicit-usage", report + compatFrame("response.completed", strings.Replace(withUsage, "resp_1", "resp_2", 1)), ResponsesStreamProtocolError, 502, ResponsesUsageReported, "priority", false, true},
				{"same-frame-mixed-id", compatFrame("", `{"type":"response.completed","response_id":"resp_2","response":`+compatCompleted+`}`), ResponsesStreamProtocolError, 502, ResponsesUsageMissing, "", false, true},
				{"same-frame-event-conflict", compatFrame("response.failed", `{"type":"response.completed","response":`+compatCompleted+`}`), ResponsesStreamProtocolError, 502, ResponsesUsageMissing, "", false, true},
				{"status-conflict", compatFrame("response.completed", compatPartial), ResponsesStreamProtocolError, 502, ResponsesUsageMissing, "", false, true},
				{"output-conflict", completed + compatFrame("response.done", strings.Replace(compatCompleted, "hello", "different", 1)), ResponsesStreamProtocolError, 502, ResponsesUsageMissing, "", false, true},
				{"explicit-empty-text-conflicts", completed + compatFrame("response.done", strings.Replace(compatCompleted, "hello", "", 1)), ResponsesStreamProtocolError, 502, ResponsesUsageMissing, "", false, true},
				{"item-id-conflict", compatFrame("", `{"type":"response.output_text.delta","item_id":"wrong","output_index":0,"delta":"hel"}`) + completed, ResponsesStreamProtocolError, 502, ResponsesUsageMissing, "", false, true},
				{"empty-completed", empty, ResponsesStreamProtocolError, 502, ResponsesUsageUnverified, "", false, true},
				{"empty-incomplete", strings.Replace(compatPartial, compatMessage, "", 1), ResponsesStreamProtocolError, 502, ResponsesUsageMissing, "", false, true},
				{"no-incomplete-reason", strings.Replace(compatPartial, "max_output_tokens", "", 1), ResponsesStreamProtocolError, 502, ResponsesUsageMissing, "", false, true},
				{"statusless-done", compatFrame("response.done", strings.Replace(compatCompleted, `"status":"completed",`, "", 1)), ResponsesStreamProtocolError, 502, ResponsesUsageMissing, "", false, true},
				{"done-only", "data: [DONE]\n\n", ResponsesStreamProtocolError, 502, ResponsesUsageMissing, "", false, true},
				{"incomplete-frame", completed + "data: {\"type\":\"response.failed\"}\n", ResponsesStreamProtocolError, 502, ResponsesUsageMissing, "", false, false},
				{"json-trailing-value", compatCompleted + "{}", ResponsesStreamProtocolError, 502, ResponsesUsageMissing, "", false, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					attempt := new(ResponsesStreamAttempt)
					resp := compatResponse(strings.NewReader(tc.body))
					defer func() { assert.NoError(t, resp.Body.Close()) }()
					result, sink, err := compatRead(t.Context(), passthrough, resp, attempt)
					require.NotNil(t, result)
					if tc.status == http.StatusOK {
						require.NoError(t, err)
						require.Len(t, sink.heads, 1)
						require.Len(t, sink.bodies, 1)
						require.True(t, gjson.ValidBytes(sink.bodies[0]))
					} else {
						var streamErr *ResponsesStreamError
						require.ErrorAs(t, err, &streamErr)
						require.Equal(t, tc.status, streamErr.StatusCode, streamErr.Message)
						require.Empty(t, sink.heads)
						require.Empty(t, sink.bodies)
					}
					snapshot := attempt.Snapshot()
					require.Equal(t, tc.resolution, snapshot.Resolution)
					require.Equal(t, tc.eof, snapshot.CleanEOF)
					require.Equal(t, tc.replay, snapshot.ReplayAllowed)
					require.Equal(t, !tc.replay, snapshot.ReplayBlocked)
					require.Equal(t, tc.usage, snapshot.Usage.State)
					require.Equal(t, tc.tier, snapshot.ServiceTier)
					switch tc.usage {
					case ResponsesUsageReported:
						if tc.name == "hosted-tool-only" {
							require.Equal(t, 9, result.Usage.InputTokens)
							require.Zero(t, result.Usage.OutputTokens)
						} else {
							require.Equal(t, 7, result.Usage.InputTokens)
							require.Equal(t, 2, result.Usage.OutputTokens)
						}
					case ResponsesUsageZero:
						require.NotNil(t, result.Usage)
						require.Zero(t, result.Usage.InputTokens)
						require.Zero(t, result.Usage.OutputTokens)
					default:
						require.Nil(t, result.Usage)
					}
					if tc.name == "independent-survives-conflict" {
						require.Equal(t, ResponsesUsageIndependent, snapshot.Usage.Source)
						require.Equal(t, snapshot.IndependentUsage, snapshot.Usage)
					}
				})
			}
		})
	}
}

// TestResponsesCompatibilityKeepsIndependentImages feeds one completed image
// and one preview before a failed terminal. Both adapters preserve completed
// image accounting without publishing any part of the failed response.
func TestResponsesCompatibilityKeepsIndependentImages(t *testing.T) {
	image := compatFrame("response.output_item.done", `{"response_id":"resp_1","output_index":0,"item":{"id":"img_1","type":"image_generation_call","status":"completed","result":"aW1hZ2U=","size":"1024x1024"}}`)
	preview := compatFrame("", `{"type":"response.image_generation_call.partial_image","output_index":1,"item_id":"img_2","partial_image_b64":"cHJldmlldw=="}`)
	for _, passthrough := range []bool{false, true} {
		resp := compatResponse(strings.NewReader(image + preview + compatFrame("", compatOverload)))
		attempt := new(ResponsesStreamAttempt)
		result, sink, err := compatRead(t.Context(), passthrough, resp, attempt)
		require.NoError(t, resp.Body.Close())
		var failure *ResponsesStreamError
		require.ErrorAs(t, err, &failure)
		require.Equal(t, 503, failure.StatusCode)
		require.Empty(t, sink.bodies)
		require.Equal(t, 1, result.ImageCount)
		require.Equal(t, []string{"1024x1024"}, result.ImageOutputSizes)
		require.Nil(t, result.Usage)
		require.True(t, attempt.Snapshot().ReplayBlocked)
	}
}

// TestResponsesCompatibilityWaitsForEOF holds an HTTP body open after a
// terminal and DONE marker. It verifies that replay and the output timer see
// new evidence immediately while downstream commitment waits for the tail.
func TestResponsesCompatibilityWaitsForEOF(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			reader, writer := io.Pipe()
			type result struct {
				sink *nonStreamingTestSink // downstream commitment buffer
				err  error                 // classified reader outcome
			}
			done := make(chan result, 1)
			attempt := new(ResponsesStreamAttempt)
			firstOutput := make(chan ResponsesStreamSnapshot, 1)
			attempt.OnFirstOutput = func() { firstOutput <- attempt.Snapshot() }
			go func() {
				resp := compatResponse(reader)
				_, sink, err := compatRead(t.Context(), passthrough, resp, attempt)
				done <- result{sink, err}
			}()
			_, err := io.WriteString(writer, compatFrame("", `{"type":"response.output_text.delta","output_index":0,"delta":"hel"}`))
			require.NoError(t, err)
			snapshot := <-firstOutput
			require.True(t, snapshot.ReplayBlocked)
			require.Empty(t, snapshot.Resolution)
			_, err = io.WriteString(writer, compatFrame("response.completed", compatCompleted)+"data: [DONE]\n\n: heartbeat\n")
			require.NoError(t, err)
			synctest.Wait()
			require.Empty(t, done)
			require.False(t, attempt.Snapshot().CleanEOF)
			_, err = io.WriteString(writer, compatFrame("response.incomplete", compatPartial))
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			res := <-done
			require.Error(t, res.err)
			require.Empty(t, res.sink.bodies)
			require.Equal(t, ResponsesStreamProtocolError, attempt.Snapshot().Resolution)
		})
	}
}

// TestResponsesCompatibilityReadFailure supplies an independently measured
// response before transport failure. Accounting and replay state must survive
// even though neither EOF nor a successful terminal can be committed.
func TestResponsesCompatibilityReadFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		tail   string
		status int
	}{
		{"unexpected-eof", io.ErrUnexpectedEOF, "", 502},
		{"timeout", context.DeadlineExceeded, "", 504},
		{"overload-before-timeout", context.DeadlineExceeded, compatFrame("", compatOverload), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := io.MultiReader(strings.NewReader(compatFrame("", compatUsage)+tc.tail), compatErrorReader{tc.err})
			attempt := new(ResponsesStreamAttempt)
			resp := compatResponse(reader)
			defer func() { assert.NoError(t, resp.Body.Close()) }()
			result, sink, err := compatRead(t.Context(), false, resp, attempt)
			var streamErr *ResponsesStreamError
			require.ErrorAs(t, err, &streamErr)
			require.True(t, errors.Is(err, tc.err))
			require.Equal(t, tc.status, streamErr.StatusCode)
			require.Empty(t, sink.bodies)
			require.Equal(t, 7, result.Usage.InputTokens)
			require.Equal(t, 2, result.Usage.OutputTokens)
			require.True(t, attempt.Snapshot().ReplayBlocked)
			require.False(t, attempt.Snapshot().CleanEOF)
		})
	}
}

// Read reports the configured error without supplying replacement body bytes.
func (r compatErrorReader) Read([]byte) (int, error) { return 0, r.err }

// compatFrame encodes one blank-line delimited fixture, including optional event labels.
func compatFrame(label, body string) string {
	frame := "data: " + body + "\n\n"
	if label != "" {
		return "event: " + label + "\n" + frame
	}
	return frame
}

// compatResponse creates an upstream response whose body remains owned by the test.
func compatResponse(body io.Reader) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(body), Header: make(http.Header)}
}

// compatRead drives the selected public nonstream reader with real callback
// order and captures every attempted downstream write. A legacy full-buffered
// reader invocation is a test failure because it bypasses incremental reading.
func compatRead(ctx context.Context, passthrough bool, resp *http.Response, attempt *ResponsesStreamAttempt) (*NonStreamingResult, *nonStreamingTestSink, error) {
	var calls []string
	sink := &nonStreamingTestSink{calls: &calls}
	headers := func(dst, _ http.Header) {
		dst.Set("Content-Type", "text/event-stream")
		dst.Set("Content-Length", "999")
	}
	unexpectedRead := func(io.Reader) ([]byte, error) {
		return nil, errors.New("compatibility selected the legacy reader")
	}
	if passthrough {
		options := passthroughTestOptions(&calls)
		options.NonStream.Compatibility = attempt
		options.NonStream.ReadBody = unexpectedRead
		options.Headers = headers
		result, err := ReadPassthroughNonStreaming(ctx, resp, sink, options, "client-model", "upstream-model")
		return result, sink, err
	}
	options := nonStreamingTestOptions(&calls)
	options.Compatibility = attempt
	options.ReadBody = unexpectedRead
	options.ResponseHeaders = headers
	result, err := ReadNonStreamingResponse(ctx, resp, upstream.NewOutputContext(sink), options, "client-model", "upstream-model")
	return result, sink, err
}
