package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"

	"github.com/TokenFlux/TokenRouter/internal/billing"
)

// redeem_code.go defines redemption credentials and records balance adjustments.
// Use counters, expiration, and recipient receipts let RedeemService check
// availability before issuing balance, concurrency, or subscription benefits.
// The code retains the latest recipient and redemption time. A chain deposit
// reversal shares this history table and records its source separately from
// the required code field.

// RedeemCode stores entitlement definitions, redemption availability, and balance adjustments.
// Deletes physically remove code rows; this entity has no soft-delete mixin.
type RedeemCode struct {
	ent.Schema // generated entity definition
}

// Annotations maps entitlement code persistence to the redeem_codes database table.
func (RedeemCode) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "redeem_codes"},
	}
}

// Fields describes code identity, entitlement, eligibility, counters, expiry,
// and the optional source identity of a chain deposit reversal.
// Generated Ent inserts start unused with a balance entitlement and one use.
// Code validation counts Unicode code points, matching the API's 32-character
// limit across ASCII and multibyte text.
func (RedeemCode) Fields() []ent.Field {
	return []ent.Field{
		// Codes contain at most 32 Unicode code points, matching API validation.
		field.String("code").
			MaxRuneLen(32).
			NotEmpty().
			Unique(),
		// type selects the stored code category. RedeemService grants balance,
		// concurrency, or subscription benefits; invitation and administrator
		// adjustment categories are handled by their owning workflows.
		field.String("type").
			MaxLen(20).
			Default(billing.RedeemTypeBalance),
		// value supplies the balance credit or concurrency adjustment amount.
		// RedeemService converts concurrency value to an integer; subscription
		// redemption instead obtains its entitlement from the selected plan_id.
		field.Float("value").
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,8)"}).
			Default(0),
		// Stored availability state.
		field.String("status").
			MaxLen(20).
			Default(billing.StatusUnused),
		// requires_payment asks RedeemService to check the recipient's paid-order
		// history through HasPaidOrder before issuing benefits. Missing history
		// rejects redemption, and an unavailable lookup returns an error.
		field.Bool("requires_payment").
			Default(false),
		// max_uses and used_count govern remaining redemptions. Billing's code
		// model treats a zero maximum as unlimited and negative maxima as the
		// single-use default; successful redemption increments the stored count.
		// Total redemption limit.
		field.Int("max_uses").
			Default(1),
		// Completed redemption count.
		field.Int("used_count").
			Default(0),
		// expires_at is optional. The billing code model treats a present
		// timestamp at or before the current time as expired, while an absent
		// timestamp leaves availability to status and use-count checks.
		field.Time("expires_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		// used_by and used_at identify the latest successful recipient and time.
		// RedeemService updates them with used_count, while RedeemCodeUsage keeps
		// the per-recipient receipts needed for reusable code history.
		// Latest recipient identity.
		field.Int64("used_by").
			Optional().
			Nillable(),
		// Latest redemption timestamp.
		field.Time("used_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		// Optional administrator notes.
		field.String("notes").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "text"}),
		// source_ref identifies the exact deposit event behind a reversal. Ordinary
		// redemption rows leave it NULL, and the reversal index protects its uniqueness.
		field.String("source_ref").
			MaxLen(160).
			Optional().
			Comment("Synthetic ledger row source reference, e.g. crypto_deposit:<deposit_id>:<chain>:<tx_hash>:<log_index>"),
		// Immutable creation timestamp.
		field.Time("created_at").
			Immutable().
			Default(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		// plan_id selects the subscription template for a subscription entitlement.
		// RedeemService rejects that entitlement when the stored plan identity
		// is absent or nonpositive before assigning the subscription.
		field.Int64("plan_id").
			Optional().
			Nillable(),
		// validity_days freezes a subscription's issued duration when present.
		// NULL preserves legacy live-plan duration; this value does not govern
		// when the credential itself expires or how many recipients can redeem it.
		field.Int("validity_days").
			Optional().
			Nillable().
			Positive().
			Max(billing.MaxValidityDays).
			SchemaType(map[string]string{dialect.Postgres: "integer"}),
	}
}

// Edges exposes redemption receipts and optionally links the latest user and
// subscription plan. The optional scalar identities back unique entity edges;
// each code can retain multiple recipient receipts.
func (RedeemCode) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("usage_records", RedeemCodeUsage.Type),
		edge.From("user", User.Type).
			Ref("redeem_codes").
			Field("used_by").
			Unique(),
		edge.From("plan", SubscriptionPlan.Type).
			Ref("redeem_codes").
			Field("plan_id").
			Unique(),
	}
}

// Indexes supports code availability, expiration, latest-recipient, and plan
// lookups. The partial reversal indexes enforce one adjustment per source and
// support recent reversal lookups by recipient. The code field declares its
// unique constraint separately, and RedeemCodeUsage owns receipt uniqueness.
func (RedeemCode) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("status"),
		index.Fields("expires_at"),
		index.Fields("used_by"),
		index.Fields("plan_id"),
		index.Fields("source_ref").
			Unique().
			StorageKey("uq_redeem_codes_crypto_revoke_source").
			Annotations(entsql.IndexWhere("type = 'crypto_revoke' AND source_ref IS NOT NULL")),
		index.Fields("used_by", "used_at").
			StorageKey("idx_redeem_codes_crypto_revoke_user").
			Annotations(entsql.DescColumns("used_at"), entsql.IndexWhere("type = 'crypto_revoke'")),
	}
}
