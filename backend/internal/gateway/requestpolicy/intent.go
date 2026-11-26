package requestpolicy

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/tidwall/gjson"
)

// intent.go preserves the validated client document and the first rollout
// decision as one request value. Later preparation can generate new fields or
// select a different model while retaining the caller's original constraints.
// The document is a string so readers across protocol layers share one immutable
// snapshot even when an outbound attempt modifies its own byte buffer.
//
// Lookup walks literal object keys and decimal array indices. Keeping lookup
// separate from interpretation lets field validators distinguish omitted input,
// explicit JSON null, and values synthesized by a later protocol layer.

const (
	// Origins distinguish missing input, original client input, and protocol-generated values.
	OriginAbsent Origin = iota
	OriginExplicit
	OriginDerived
)

// Origin reports where a value used by request policy originated.
type Origin uint8

// Value combines a field's provenance and its unmodified JSON representation.
type Value struct {
	Origin Origin // source of value
	Raw    string // original JSON text
}

// Intent carries the original request and its frozen entry policy decision.
type Intent struct {
	Mode           Mode          // effective rollout arm
	Revision       int64         // matched policy revision
	EntryGroupID   int64         // group at authentication
	Endpoint       string        // normalized inbound path
	RequestedModel string        // model before rewrites
	raw            string        // immutable client JSON
	upstreamStatus *atomic.Int32 // last actual text exchange, shared by attempts
}

// intentContextKey distinguishes the frozen policy from other request metadata.
type intentContextKey struct{}

// RecordUpstreamStatus retains transport evidence for attribution of a final
// rejection. Starting another text send clears the preceding status so a
// recovered or interrupted exchange cannot supply the final provider's status.
func (i Intent) RecordUpstreamStatus(status int) {
	if i.upstreamStatus != nil {
		i.upstreamStatus.Store(int32(status))
	}
}

// LastUpstreamStatus returns zero until a text provider actually replies.
func (i Intent) LastUpstreamStatus() int {
	if i.upstreamStatus == nil {
		return 0
	}
	return int(i.upstreamStatus.Load())
}

// FromContext returns the initial matched intent shared by all attempts.
func FromContext(ctx context.Context) (Intent, bool) {
	intent, ok := ctx.Value(intentContextKey{}).(Intent)
	return intent, ok
}

// RawJSON returns the entire original validated JSON object unchanged.
func (i Intent) RawJSON() string { return i.raw }

// Lookup reads a field from the original client object. Each segment is an
// exact key for an object or an unsigned decimal index for an array. An
// omitted path returns OriginAbsent, while JSON null remains explicit.
func (i Intent) Lookup(path ...string) Value {
	current := gjson.Parse(i.raw)
	for _, part := range path {
		switch {
		case current.IsObject():
			var next gjson.Result
			current.ForEach(func(key, value gjson.Result) bool {
				if key.String() != part {
					return true
				}
				next = value
				return false
			})
			current = next
		case current.IsArray():
			index, err := strconv.ParseUint(part, 10, 31)
			if err != nil {
				return Value{}
			}
			current = current.Get(strconv.FormatUint(index, 10))
		default:
			return Value{}
		}
	}
	if !current.Exists() {
		return Value{}
	}
	return Value{Origin: OriginExplicit, Raw: current.Raw}
}

// withIntent binds the first enrollment for the lifetime of the logical
// request. The caller supplies validated client bytes, which are copied into
// the string before any subsequent request transformation can use that buffer.
func withIntent(ctx context.Context, mode Mode, revision int64, entry Entry, raw []byte) context.Context {
	return context.WithValue(ctx, intentContextKey{}, Intent{
		Mode: mode, Revision: revision, EntryGroupID: entry.EntryGroupID,
		Endpoint: entry.Endpoint, RequestedModel: strings.Clone(entry.RequestedModel), raw: string(raw), upstreamStatus: new(atomic.Int32),
	})
}
