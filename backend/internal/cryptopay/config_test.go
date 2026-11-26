package cryptopay

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay/rpc"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/sweeper"
	"github.com/TokenFlux/TokenRouter/internal/settings"
)

var _ SettingStore = (*recordingSettingStore)(nil)

// recordingSettingStore serves one stored raw config and records writes, so a
// read path that silently normalizes/backfills the row can be detected.
type recordingSettingStore struct {
	raw     string
	missing bool
	writes  int
	lastSet string
}

// TestNormalizeConfigDefaults passes an empty configuration through normalization.
// The result must contain the default values needed by a newly configured channel.
func TestNormalizeConfigDefaults(t *testing.T) {
	cfg, err := NormalizeConfig(CryptoConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled {
		t.Fatal("default config must be disabled")
	}
	if cfg.SweepGlobalThreshold != 1000 {
		t.Fatalf("threshold = %v, want 1000", cfg.SweepGlobalThreshold)
	}
	if cfg.CreditMultiplier != 6.7 {
		t.Fatalf("multiplier = %v, want 6.7", cfg.CreditMultiplier)
	}
	if cfg.Confirmations[ChainBSC] != 3 {
		t.Fatalf("bsc confirmations = %d, want 3", cfg.Confirmations[ChainBSC])
	}
}

func TestNormalizeConfigRejectsUnknownChain(t *testing.T) {
	_, err := NormalizeConfig(CryptoConfig{Chains: []string{"tron"}})
	if err == nil {
		t.Fatal("expected error for unsupported chain")
	}
}

// Enabling the channel without a hot wallet is a valid state: deposits,
// scanning and crediting all work. Only a real sweep must refuse.
func TestNormalizeConfigAllowsEmptyHotWallets(t *testing.T) {
	cfg, err := NormalizeConfig(CryptoConfig{Enabled: true})
	if err != nil {
		t.Fatalf("empty hot wallets must normalize, got %v", err)
	}
	if !cfg.Enabled {
		t.Fatal("expected enabled config to survive normalization")
	}
}

// The sweep gate is what protects funds: no destination, no sweep.
// A config stored before the rpc key existed reads back as null; the admin
// console indexes that map directly, so it must never stay nil.
func TestNormalizeConfigBackfillsNilRPC(t *testing.T) {
	cfg, err := NormalizeConfig(CryptoConfig{RPC: nil})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RPC == nil {
		t.Fatal("rpc must be backfilled to an empty map, not left nil")
	}
	if len(cfg.RPC) != 0 {
		t.Fatalf("rpc = %v, want empty", cfg.RPC)
	}
}

func TestValidateSweepReady(t *testing.T) {
	tests := []struct {
		name    string
		hot     string
		wantErr bool
	}{
		{"configured", "0x1111111111111111111111111111111111111111", false},
		{"empty", "", true},
		{"whitespace", "   ", true},
		{"zero address", "0x0000000000000000000000000000000000000000", true},
		{"bare prefix", "0x", true},
		{"truncated", "0x1234", true},
		{"non hex", "0xzz11111111111111111111111111111111111111", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CryptoConfig{HotWalletEVM: tt.hot}.ValidateSweepReady()
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateSweepReady(%q) err = %v, wantErr %v", tt.hot, err, tt.wantErr)
			}
		})
	}
}

// A negative confirmation/scan range used to wrap when cast to uint64 and
// stopped the chain from ever scanning; NormalizeConfig must clamp them.
func TestNormalizeConfigClampsRanges(t *testing.T) {
	cfg, err := NormalizeConfig(CryptoConfig{
		Chains:        []string{ChainBSC},
		Confirmations: map[string]int{ChainBSC: -5},
		ScanRange:     map[string]int{ChainBSC: -1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Confirmations[ChainBSC] != 0 {
		t.Fatalf("confirmations = %d, want 0", cfg.Confirmations[ChainBSC])
	}
	if cfg.ScanRange[ChainBSC] != 1 {
		t.Fatalf("scan range = %d, want 1", cfg.ScanRange[ChainBSC])
	}
}

// The console saves the whole config object; a malformed hot wallet must be
// rejected instead of being silently padded by common.HexToAddress.
func TestNormalizeConfigRejectsMalformedHotWallet(t *testing.T) {
	for _, hot := range []string{"0x", "0x1234", "not-an-address"} {
		if _, err := NormalizeConfig(CryptoConfig{
			Chains:       []string{ChainBSC},
			HotWalletEVM: hot,
		}); err == nil {
			t.Fatalf("hot wallet %q should be rejected", hot)
		}
	}
}

func TestIsEVMChain(t *testing.T) {
	for _, chain := range []string{ChainBSC, ChainEthereum, ChainPolygon, ChainPlasma, ChainArbitrum, ChainBase, ChainXLayer} {
		if !IsEVMChain(chain) {
			t.Fatalf("%s should be EVM", chain)
		}
	}
	if IsEVMChain(ChainSolana) {
		t.Fatal("solana must not be EVM")
	}
	if KnownChain("tron") || !KnownChain(ChainEthereum) || !KnownChain(ChainSolana) {
		t.Fatal("supported deposit networks are inconsistent")
	}
}

func TestNormalizeConfigSupportsAllRequestedNetworks(t *testing.T) {
	chains := []string{ChainEthereum, ChainPolygon, ChainPlasma, ChainBase, ChainXLayer, ChainEthereum}
	cfg, err := NormalizeConfig(CryptoConfig{
		Enabled: true, Chains: chains,
		Confirmations: map[string]int{}, ScanRange: map[string]int{}, GasTopUp: map[string]float64{}, SweepMinPerAddress: map[string]float64{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Chains) != 5 {
		t.Fatal("duplicate chain was retained")
	}
	for _, chain := range cfg.Chains {
		if cfg.Confirmations[chain] <= 0 || cfg.ScanRange[chain] <= 0 || cfg.SweepMinPerAddress[chain] <= 0 {
			t.Fatalf("%s is missing operational defaults", chain)
		}
	}
	if cfg.GasTopUp[ChainEthereum] <= 0 || cfg.Confirmations[ChainEthereum] < 64 {
		t.Fatal("Ethereum requires its own gas and confirmation settings")
	}
}

func TestNormalizeConfigKeepsSolanaDepositsDisabled(t *testing.T) {
	if _, err := NormalizeConfig(CryptoConfig{Enabled: true, Chains: []string{ChainSolana}}); err == nil {
		t.Fatal("Solana deposits remain unavailable")
	}
}

func TestConfigSnapshotsDoNotShareMutableMaps(t *testing.T) {
	base := DefaultCryptoConfig()
	s := &Service{config: base}
	snapshot := s.Config()
	snapshot.Confirmations[ChainBSC] = 100
	snapshot.Chains[0] = ChainBase
	if s.Config().Confirmations[ChainBSC] != 3 || s.Config().Chains[0] != ChainBSC {
		t.Fatal("a config reader changed the live config")
	}
	_, err := NormalizeConfig(CryptoConfig{Chains: []string{ChainBSC}, Confirmations: base.Confirmations})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeConfigValidatesRPCProtectionBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  CryptoConfig
	}{
		{"negative throttle", CryptoConfig{RPCThrottleMs: -1}},
		{"oversized throttle", CryptoConfig{RPCThrottleMs: maxRPCThrottleMs + 1}},
		{"oversized blocks", CryptoConfig{RPCLimits: map[string]RPCEndpointLimits{
			"https://rpc.example": {MaxBlocks: maxRPCBlocks + 1},
		}}},
		{"negative addresses", CryptoConfig{RPCLimits: map[string]RPCEndpointLimits{
			"https://rpc.example": {MaxAddresses: -1},
		}}},
		{"oversized addresses", CryptoConfig{RPCLimits: map[string]RPCEndpointLimits{
			"https://rpc.example": {MaxAddresses: maxRPCAddresses + 1},
		}}},
		{"empty key", CryptoConfig{RPCLimits: map[string]RPCEndpointLimits{
			" ": {MaxBlocks: 100},
		}}},
		{"invalid key", CryptoConfig{RPCLimits: map[string]RPCEndpointLimits{
			"not-a-url": {MaxBlocks: 100},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NormalizeConfig(tc.cfg); err == nil {
				t.Fatal("unsafe RPC protection value was accepted")
			}
		})
	}
}

func TestNormalizeConfigAcceptsConfiguredAndPredeclaredRPCLimits(t *testing.T) {
	cfg, err := NormalizeConfig(CryptoConfig{
		RPC: map[string]string{ChainBSC: "https://rpc.example, https://backup.example/"},
		RPCLimits: map[string]RPCEndpointLimits{
			"https://rpc.example":     {MaxBlocks: maxRPCBlocks, MaxAddresses: maxRPCAddresses},
			"  https://new.example  ": {MaxBlocks: 100, MaxAddresses: 20},
		},
		RPCThrottleMs: maxRPCThrottleMs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RPCThrottleMs != maxRPCThrottleMs {
		t.Fatalf("rpc_throttle_ms = %d, want %d", cfg.RPCThrottleMs, maxRPCThrottleMs)
	}
	if got := cfg.RPCLimits["https://rpc.example"]; got.MaxBlocks != maxRPCBlocks || got.MaxAddresses != maxRPCAddresses {
		t.Fatalf("configured endpoint limits = %+v", got)
	}
	if _, ok := cfg.RPCLimits["https://new.example"]; !ok {
		t.Fatalf("trimmed pre-declared key missing: %+v", cfg.RPCLimits)
	}
}

// The market feed is the only external dependency of native crediting: a
// missing value must mean "public default", and a typo must fail at load time,
// not once per native deposit.
func TestNormalizeConfigNativePriceFeed(t *testing.T) {
	cfg, err := NormalizeConfig(CryptoConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NativePriceURL != "https://api.binance.com/api/v3/ticker/price" {
		t.Fatalf("native_price_url = %q, want the public Binance default", cfg.NativePriceURL)
	}

	cfg, err = NormalizeConfig(CryptoConfig{NativePriceURL: "  https://prices.example/ticker  "})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NativePriceURL != "https://prices.example/ticker" {
		t.Fatalf("native_price_url = %q, want the trimmed value", cfg.NativePriceURL)
	}

	cfg, err = NormalizeConfig(CryptoConfig{NativePriceURL: "https://prices.example/{symbol}"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NativePriceURL != "https://prices.example/{symbol}" {
		t.Fatalf("a symbol template must normalize, got %q", cfg.NativePriceURL)
	}

	for _, bad := range []string{"not-a-url", "ftp://prices.example/ticker", "https:///ticker"} {
		if _, err := NormalizeConfig(CryptoConfig{NativePriceURL: bad}); err == nil {
			t.Fatalf("unsafe native price url %q was accepted", bad)
		}
	}
}

func TestConfigPatchAppliesNativePriceURL(t *testing.T) {
	url := "https://mirror.example/ticker"
	got := CryptoConfigPatch{NativePriceURL: &url}.Apply(DefaultCryptoConfig())
	if got.NativePriceURL != url {
		t.Fatalf("patched native_price_url = %q, want %q", got.NativePriceURL, url)
	}
}

// GetCryptoSetting returns the fixture text or the host's missing-key error.
func (s *recordingSettingStore) GetCryptoSetting(context.Context, string) (string, error) {
	if s.missing {
		return "", settings.ErrSettingNotFound
	}
	return s.raw, nil
}

func (s *recordingSettingStore) SetCryptoSetting(_ context.Context, _ string, value string) error {
	s.writes++
	s.lastSet = value
	return nil
}

// An administrator-enabled channel must survive loading: the defaults used to
// decode a partially stored row only fill absent fields, and reading the config
// must never write the normalized row back.
func TestLoadConfigKeepsStoredEnabledTrueWithoutWriteBack(t *testing.T) {
	store := &recordingSettingStore{raw: `{"enabled":true,"chains":["bsc"],"rpc":{"bsc":"https://bsc.example.test"},"credit_multiplier":5.5}`}
	cfg, err := LoadConfig(t.Context(), store)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Enabled {
		t.Fatal("stored enabled=true was overwritten by the disabled default")
	}
	if cfg.CreditMultiplier != 5.5 {
		t.Fatalf("credit multiplier = %v, want the stored 5.5", cfg.CreditMultiplier)
	}
	if store.writes != 0 {
		t.Fatalf("LoadConfig wrote %d times (value %q); reads must never normalize the stored row", store.writes, store.lastSet)
	}
}

// Fields absent from an older stored row are filled from the defaults, while
// fields that are present keep their stored value.
func TestLoadConfigFillsMissingFieldsOnly(t *testing.T) {
	store := &recordingSettingStore{raw: `{"enabled":true,"chains":["bsc"],"scan_range":{"bsc":150}}`}
	cfg, err := LoadConfig(t.Context(), store)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Enabled {
		t.Fatal("present enabled=true was not preserved")
	}
	if cfg.ScanRange[ChainBSC] != 150 {
		t.Fatalf("present scan_range = %d, want 150", cfg.ScanRange[ChainBSC])
	}
	if cfg.ScanRange[ChainEthereum] != DefaultCryptoConfig().ScanRange[ChainEthereum] {
		t.Fatalf("missing scan_range.ethereum = %d, want the default", cfg.ScanRange[ChainEthereum])
	}
	if cfg.CreditMultiplier != DefaultCryptoConfig().CreditMultiplier {
		t.Fatalf("missing credit_multiplier = %v, want the default", cfg.CreditMultiplier)
	}
	if store.writes != 0 {
		t.Fatalf("LoadConfig wrote the config row while backfilling defaults (%d writes)", store.writes)
	}
}

// A never-saved key is the normal first-run state and still yields defaults
// without persisting them.
func TestLoadConfigMissingKeyYieldsDefaultsWithoutWrite(t *testing.T) {
	store := &recordingSettingStore{missing: true}
	cfg, err := LoadConfig(t.Context(), store)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Enabled || cfg.CreditMultiplier != DefaultCryptoConfig().CreditMultiplier {
		t.Fatalf("first-run defaults wrong: %+v", cfg)
	}
	if store.writes != 0 {
		t.Fatalf("first-run load persisted defaults (%d writes)", store.writes)
	}
}

func fingerprintForTest(t *testing.T, cfg CryptoConfig) string {
	t.Helper()
	fingerprint, err := configFingerprint(cfg)
	require.NoError(t, err)
	return fingerprint
}

// Equal configs must not restart workers just because map iteration differs.
// All persisted fields, including crediting and sweep settings, affect reloads.
func TestConfigFingerprintCoversEveryExportedField(t *testing.T) {
	// No exclusions: CryptoConfig has no timestamps, nonces or display-only fields.
	excluded := map[string]string{}
	base := DefaultCryptoConfig()
	configType := reflect.TypeOf(base)
	for field := range configType.Fields() {
		if !field.IsExported() {
			continue
		}
		t.Run(field.Name, func(t *testing.T) {
			changed := DefaultCryptoConfig()
			mutateFingerprintField(t, reflect.ValueOf(&changed).Elem().FieldByIndex(field.Index))
			if reason, ok := excluded[field.Name]; ok {
				require.NotEmpty(t, reason)
				require.True(t, fingerprintForTest(t, base) == fingerprintForTest(t, changed), reason)
				return
			}
			require.True(t, fingerprintForTest(t, base) != fingerprintForTest(t, changed), "field must participate in reload detection")
		})
	}
}

func mutateFingerprintField(t *testing.T, value reflect.Value) {
	t.Helper()
	switch value.Kind() {
	case reflect.Bool:
		value.SetBool(!value.Bool())
	case reflect.String:
		value.SetString(value.String() + "changed")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(value.Int() + 1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value.SetUint(value.Uint() + 1)
	case reflect.Float32, reflect.Float64:
		value.SetFloat(value.Float() + 1)
	case reflect.Slice:
		value.Set(reflect.Append(value, reflect.Zero(value.Type().Elem())))
	case reflect.Map:
		if value.IsNil() {
			value.Set(reflect.MakeMap(value.Type()))
		}
		entry := reflect.New(value.Type().Elem()).Elem()
		mutateFingerprintField(t, entry)
		value.SetMapIndex(reflect.ValueOf("changed").Convert(value.Type().Key()), entry)
	case reflect.Struct:
		mutateFingerprintField(t, value.Field(0))
	case reflect.Pointer:
		// Tri-state config switches (e.g. instant_credit_enabled) are pointers
		// so "absent" stays distinguishable from an explicit false.
		if value.IsNil() {
			value.Set(reflect.New(value.Type().Elem()))
		}
		mutateFingerprintField(t, value.Elem())
	default:
		t.Fatalf("add a fingerprint mutation for %s", value.Type())
	}
}

func TestConfigFingerprintNormalizationIsIdempotent(t *testing.T) {
	for _, cfg := range []CryptoConfig{
		{},
		DefaultCryptoConfig(),
		{
			Chains: []string{ChainBSC, ChainBSC}, RPCThrottleMs: 25,
			RPCLimits:  map[string]RPCEndpointLimits{" https://rpc.example.test ": {MaxBlocks: 100}},
			NativeScan: " BLOCKS ", NativePriceURL: " https://price.example.test ",
			ScanRange: map[string]int{ChainBSC: -1}, ScanConcurrency: map[string]int{ChainBSC: 1000},
			ExplorerAPIURL: map[string]string{ChainBSC: " https://explorer.example.test/api/ "},
		},
	} {
		first, err := NormalizeConfig(cfg)
		require.NoError(t, err)
		raw, err := json.Marshal(first)
		require.NoError(t, err)
		var readBack CryptoConfig
		require.NoError(t, json.Unmarshal(raw, &readBack))
		second, err := NormalizeConfig(readBack)
		require.NoError(t, err)
		require.True(t, fingerprintForTest(t, first) == fingerprintForTest(t, second), "normalizing a stored snapshot again must not trigger reload")
	}
}

func TestConfigFingerprintRejectsNonSerializableValues(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		cfg := DefaultCryptoConfig()
		cfg.CreditMultiplier = value
		fingerprint, err := configFingerprint(cfg)
		require.Error(t, err)
		require.Empty(t, fingerprint, "a serialization error must not look like an applied config")
	}
}

func TestConfigFingerprintTracksReloadRelevantFields(t *testing.T) {
	base := CryptoConfig{
		Enabled:       true,
		Chains:        []string{"bsc", "ethereum"},
		RPC:           map[string]string{"bsc": "https://a", "ethereum": "https://b"},
		ScanRange:     map[string]int{"bsc": 500, "ethereum": 2000},
		Confirmations: map[string]int{"bsc": 3, "ethereum": 64},
	}
	same := base
	same.RPC = map[string]string{"bsc": "https://a", "ethereum": "https://b"}
	same.ScanRange = map[string]int{"bsc": 500, "ethereum": 2000}
	same.Confirmations = map[string]int{"bsc": 3, "ethereum": 64}

	require.Equal(t, fingerprintForTest(t, base), fingerprintForTest(t, same),
		"an equal config must not look changed")

	t.Run("rpc endpoint changed", func(t *testing.T) {
		mutated := base
		mutated.RPC = map[string]string{"bsc": "https://new", "ethereum": "https://b"}
		require.NotEqual(t, fingerprintForTest(t, base), fingerprintForTest(t, mutated))
	})

	t.Run("rpc limits changed", func(t *testing.T) {
		mutated := base
		mutated.RPCLimits = map[string]RPCEndpointLimits{"https://a": {MaxBlocks: 100, MaxAddresses: 20}}
		require.NotEqual(t, fingerprintForTest(t, base), fingerprintForTest(t, mutated))
	})

	t.Run("rpc throttle changed", func(t *testing.T) {
		mutated := base
		mutated.RPCThrottleMs = 250
		require.NotEqual(t, fingerprintForTest(t, base), fingerprintForTest(t, mutated))
	})

	t.Run("scan range changed", func(t *testing.T) {
		mutated := base
		mutated.ScanRange = map[string]int{"bsc": 250, "ethereum": 2000}
		require.NotEqual(t, fingerprintForTest(t, base), fingerprintForTest(t, mutated))
	})

	t.Run("confirmations changed", func(t *testing.T) {
		mutated := base
		mutated.Confirmations = map[string]int{"bsc": 5, "ethereum": 64}
		require.NotEqual(t, fingerprintForTest(t, base), fingerprintForTest(t, mutated))
	})

	t.Run("chain removed", func(t *testing.T) {
		mutated := base
		mutated.Chains = []string{"bsc"}
		require.NotEqual(t, fingerprintForTest(t, base), fingerprintForTest(t, mutated))
	})

	t.Run("enabled toggled", func(t *testing.T) {
		mutated := base
		mutated.Enabled = false
		require.NotEqual(t, fingerprintForTest(t, base), fingerprintForTest(t, mutated))
	})

	t.Run("map order does not matter", func(t *testing.T) {
		a := base
		a.RPC = map[string]string{"bsc": "https://a", "ethereum": "https://b"}
		b := base
		b.RPC = map[string]string{"ethereum": "https://b", "bsc": "https://a"}
		require.Equal(t, fingerprintForTest(t, a), fingerprintForTest(t, b),
			"map iteration order must not cause a spurious reload")
	})
}

// A config stored before the instant-credit switch existed has no key. It must
// load as ON (the shipped default), never as an accidental OFF.
func TestInstantCreditDefaultsOnForLegacyConfig(t *testing.T) {
	for _, raw := range []string{`{}`, `{"enabled":true}`, `{"enabled":true,"confirmations":{"bsc":3}}`} {
		var cfg CryptoConfig
		require.NoError(t, json.Unmarshal([]byte(raw), &cfg))
		normalized, err := NormalizeConfig(cfg)
		require.NoError(t, err)
		require.True(t, normalized.InstantCreditOn(), "legacy config %s must default to instant credit", raw)
		data, err := json.Marshal(normalized)
		require.NoError(t, err)
		var fields map[string]any
		require.NoError(t, json.Unmarshal(data, &fields))
		require.Equal(t, true, fields["instant_credit_enabled"])
	}
}

// The switch is an operator kill switch: one config write must restore the old
// confirmation-gated behaviour.
func TestInstantCreditPatchCanDisableAndSetFloor(t *testing.T) {
	base := DefaultCryptoConfig()
	off := false
	cfg := CryptoConfigPatch{InstantCreditEnabled: &off, InstantCreditMinAmount: floatPtr(5)}.Apply(base)
	normalized, err := NormalizeConfig(cfg)
	require.NoError(t, err)
	require.False(t, normalized.InstantCreditOn())
	require.Equal(t, 5.0, normalized.InstantCreditMinAmount)

	on := true
	patched := CryptoConfigPatch{InstantCreditEnabled: &on}.Apply(normalized)
	normalized, err = NormalizeConfig(patched)
	require.NoError(t, err)
	require.True(t, normalized.InstantCreditOn())
	require.Equal(t, 5.0, normalized.InstantCreditMinAmount, "omitted floor must be preserved")

	// An empty patch never changes the switch.
	normalized, err = NormalizeConfig(CryptoConfigPatch{}.Apply(base))
	require.NoError(t, err)
	require.True(t, normalized.InstantCreditOn())
}

func TestInstantCreditNegativeFloorIsSanitized(t *testing.T) {
	base := DefaultCryptoConfig()
	cfg := CryptoConfigPatch{InstantCreditMinAmount: floatPtr(-3)}.Apply(base)
	normalized, err := NormalizeConfig(cfg)
	require.NoError(t, err)
	require.Zero(t, normalized.InstantCreditMinAmount, "a negative floor must not exclude every deposit")
}

// The deposit page must declare the instant-credit state per network.
func TestDepositInfoDeclaresInstantCredit(t *testing.T) {
	cfg := depositInfoConfig()
	info := buildDepositInfo(cfg, depositInfoAssigned())
	require.NotEmpty(t, info.Chains)
	for _, chain := range info.Chains {
		require.True(t, chain.InstantCredit, "chain %s", chain.Chain)
		require.Nil(t, chain.EtaSeconds, "no measurement yet must serialize as null")
	}

	off := false
	cfg.InstantCreditEnabled = &off
	info = buildDepositInfo(cfg, depositInfoAssigned())
	for _, chain := range info.Chains {
		require.False(t, chain.InstantCredit, "chain %s", chain.Chain)
	}
}

func floatPtr(v float64) *float64 { return &v }

// A payload that omits keys must keep the stored values: an older console or
// a scripted PUT previously wiped rpc/other keys to zero values, which
// crashed the admin page on the next load.
func TestCryptoConfigPatchKeepsOmittedFields(t *testing.T) {
	base := CryptoConfig{
		Enabled:       true,
		Chains:        []string{ChainBSC, ChainPolygon},
		RPC:           map[string]string{ChainBSC: "https://rpc.example"},
		RPCLimits:     map[string]RPCEndpointLimits{"https://rpc.example": {MaxBlocks: 100, MaxAddresses: 20}},
		RPCThrottleMs: 250,
		HotWalletEVM:  "0x1111111111111111111111111111111111111111",
		SweepAuto:     true,
		HotIndex:      3,
	}
	// Patch only the EVM hot wallet, as the console does when clearing it.
	empty := ""
	merged := CryptoConfigPatch{HotWalletEVM: &empty}.Apply(base)

	if merged.HotWalletEVM != "" {
		t.Fatalf("hot wallet = %q, want cleared", merged.HotWalletEVM)
	}
	if len(merged.Chains) != 2 {
		t.Fatalf("chains = %v, want them preserved", merged.Chains)
	}
	if merged.RPC[ChainBSC] != "https://rpc.example" {
		t.Fatalf("rpc = %v, want it preserved", merged.RPC)
	}
	if merged.RPCThrottleMs != 250 || merged.RPCLimits["https://rpc.example"].MaxBlocks != 100 {
		t.Fatalf("rpc protection fields not preserved: %+v", merged)
	}
	if !merged.SweepAuto || merged.HotIndex != 3 {
		t.Fatalf("sweep_auto/hot_index not preserved: %+v", merged)
	}
}

func TestCryptoConfigPatchAppliesRPCProtectionFields(t *testing.T) {
	base := CryptoConfig{RPC: map[string]string{ChainBSC: "https://rpc.example"}}
	limits := map[string]RPCEndpointLimits{
		"https://rpc.example": {MaxBlocks: 100, MaxAddresses: 20},
	}
	throttle := 250
	merged := CryptoConfigPatch{RPCLimits: &limits, RPCThrottleMs: &throttle}.Apply(base)
	if merged.RPCThrottleMs != 250 {
		t.Fatalf("rpc_throttle_ms = %d, want 250", merged.RPCThrottleMs)
	}
	if got := merged.RPCLimits["https://rpc.example"]; got.MaxBlocks != 100 || got.MaxAddresses != 20 {
		t.Fatalf("rpc_limits = %+v, want the patch value", got)
	}

	empty := map[string]RPCEndpointLimits{}
	cleared := CryptoConfigPatch{RPCLimits: &empty}.Apply(merged)
	if len(cleared.RPCLimits) != 0 {
		t.Fatalf("rpc_limits = %+v, want cleared", cleared.RPCLimits)
	}
}

// An empty patch changes nothing.
func TestCryptoConfigPatchEmptyIsIdentity(t *testing.T) {
	base := CryptoConfig{Enabled: true, Chains: []string{ChainBSC}}
	merged := CryptoConfigPatch{}.Apply(base)
	if merged.Enabled != base.Enabled || len(merged.Chains) != 1 {
		t.Fatalf("empty patch changed the config: %+v", merged)
	}
}

// TestDefaultGasBudgetsCoverEveryChainFloor pins the relationship the deferral
// rule depends on: one transfer at a chain's minimum gas price must fit inside
// that chain's default native budget. When it does not, every sweep job on the
// chain defers with "gas quote above the configured budget" on every run and
// nothing says the budget itself is wrong.
func TestDefaultGasBudgetsCoverEveryChainFloor(t *testing.T) {
	cfg := DefaultCryptoConfig()
	config := sweeper.Config{GasTopUp: cfg.GasTopUp}
	for _, chain := range evmChains {
		id, ok := chainlistIDs[chain]
		if !ok || id <= 0 {
			t.Fatalf("chain %s has no EIP-155 id, so it has no gas price floor", chain)
		}
		if floor := rpc.GasPriceFloor(uint64(id)); floor.Sign() <= 0 {
			t.Fatalf("chain %s has no positive gas price floor", chain)
		}
		if _, ok := cfg.GasTopUp[chain]; !ok {
			t.Fatalf("chain %s has no default gas_topup to check", chain)
		}
		if err := sweeper.GasBudgetCoversGasFloor(config, chain); err != nil {
			t.Errorf("%s: %v", chain, err)
		}
	}
}

// TestDefaultNativeScanUsesBlocksEveryTenSeconds pins the operator-chosen
// defaults: the per-block scan is the default native source and the native poll
// interval is ten seconds. The address-based txlist path stays selectable.
func TestDefaultNativeScanUsesBlocksEveryTenSeconds(t *testing.T) {
	cfg := DefaultCryptoConfig()
	if cfg.NativeScan != NativeScanBlocks {
		t.Fatalf("default native_scan = %q, want %q", cfg.NativeScan, NativeScanBlocks)
	}
	if cfg.NativePollSeconds != 10 || DefaultNativePollSeconds != 10 {
		t.Fatalf("default native poll = %d (const %d), want 10", cfg.NativePollSeconds, DefaultNativePollSeconds)
	}

	// A config stored before this field existed has neither key: normalization
	// must land on the same defaults instead of keeping the old txlist/30 pair.
	stored, err := NormalizeConfig(CryptoConfig{Chains: []string{ChainBSC}})
	if err != nil {
		t.Fatal(err)
	}
	if stored.NativeScan != NativeScanBlocks || stored.NativePollSeconds != 10 {
		t.Fatalf("normalized legacy config = %q/%d, want %q/10",
			stored.NativeScan, stored.NativePollSeconds, NativeScanBlocks)
	}

	// txlist is still an accepted operator choice with an explicit interval.
	optIn := DefaultCryptoConfig()
	optIn.NativeScan = NativeScanTxList
	optIn.NativePollSeconds = 30
	normalized, err := NormalizeConfig(optIn)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.NativeScan != NativeScanTxList || normalized.NativePollSeconds != 30 {
		t.Fatalf("txlist opt-in = %q/%d, want %q/30",
			normalized.NativeScan, normalized.NativePollSeconds, NativeScanTxList)
	}
}
