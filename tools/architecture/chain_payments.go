package architecture

import "strings"

// chain_payments.go describes the package boundaries of the on-chain payment
// integration. Scanner and sweep adapters coordinate SQL with their chain clients,
// while the key-share codec and review-mode packages have no host storage access.
// Exact package names prevent new chain clients or subpackages from inheriting
// an existing adapter's permissions. Test additions cover local fixtures only.
//
// Host stores, settings and database access in the root adapter also have
// file-level restrictions in catalog.go. A fixture dependency cannot authorize
// production code to import the same implementation.
var chainPaymentPackages = map[string]dependencySet{
	"internal/cryptopay": {Production: `bytes context crypto/rand crypto/sha256 database/sql database/sql/driver encoding/hex encoding/json
ent ent/paymentorder errors fmt github.com/ethereum/go-ethereum/common
github.com/ethereum/go-ethereum/common/hexutil github.com/google/uuid github.com/lib/pq
github.com/redis/go-redis/v9 github.com/shopspring/decimal golang.org/x/net/publicsuffix
golang.org/x/sync/singleflight internal/billing internal/config internal/cryptopay/addresses
internal/cryptopay/explorer internal/cryptopay/ledger internal/cryptopay/mode
internal/cryptopay/nativeprice internal/cryptopay/notify internal/cryptopay/rpc
internal/cryptopay/scanner internal/cryptopay/sweeper internal/cryptopay/wallet
internal/egress/urlpolicy internal/notification internal/notification/bark internal/payment
internal/payment/postgres internal/pkg/apperror internal/settings io log/slog maps math math/big
math/rand net net/http net/mail net/url regexp slices sort strconv strings sync sync/atomic time
unicode`, Tests: `ent/enttest ent/runtime entgo.io/ent/dialect entgo.io/ent/dialect/sql github.com/DATA-DOG/go-sqlmock
github.com/alicebob/miniredis/v2 github.com/stretchr/testify/require golang.org/x/sync/errgroup
internal/billing/postgres internal/identity internal/identity/postgres internal/payment/testkit
internal/settings/postgres internal/testutil/postgrescontainer migrations modernc.org/sqlite
net/http/httptest os path/filepath reflect testing`},
	"internal/cryptopay/addresses": {Production: `context database/sql errors fmt internal/cryptopay/mode strings sync`, Tests: `github.com/DATA-DOG/go-sqlmock github.com/stretchr/testify/require regexp testing`},
	"internal/cryptopay/explorer":  {Production: `context encoding/json errors fmt io math/big net/http net/url strconv strings time`, Tests: `net/http/httptest sync/atomic testing`},
	"internal/cryptopay/httpapi": {Production: `bytes context encoding/base64 encoding/csv encoding/json errors github.com/gin-gonic/gin
internal/audit/httpapi internal/config internal/cryptopay internal/cryptopay/addresses
internal/cryptopay/mode internal/cryptopay/rpc internal/cryptopay/sweeper internal/cryptopay/wallet
internal/identity internal/identity/httpapi internal/identity/httpapi/authctx
internal/infra/telemetry internal/notification internal/server/httpx io log/slog net net/http
net/url os reflect sort strconv strings time unicode/utf8`, Tests: ``},
	"internal/cryptopay/ledger": {Production: `context crypto/sha256 database/sql encoding/hex errors fmt github.com/shopspring/decimal
internal/cryptopay/mode internal/cryptopay/nativeprice internal/cryptopay/scanner log/slog math
math/big strings time`, Tests: `database/sql/driver github.com/DATA-DOG/go-sqlmock github.com/stretchr/testify/require
modernc.org/sqlite testing`},
	"internal/cryptopay/mode":        {Production: `encoding/hex errors os strconv strings`, Tests: `testing`},
	"internal/cryptopay/nativeprice": {Production: `context encoding/json errors fmt github.com/shopspring/decimal io net/http net/url strings sync time`, Tests: `github.com/stretchr/testify/require net/http/httptest sync/atomic testing`},
	"internal/cryptopay/notify":      {Production: `bytes context encoding/json fmt io log/slog net/http net/url sort strconv strings sync time`, Tests: `github.com/stretchr/testify/require net/http/httptest sync/atomic testing`},
	"internal/cryptopay/rpc": {Production: `bytes context encoding/json errors fmt github.com/ethereum/go-ethereum/common/hexutil
golang.org/x/net/publicsuffix internal/egress/urlpolicy io math/big math/rand net net/http net/netip
net/url regexp sort strconv strings sync sync/atomic time`, Tests: `github.com/stretchr/testify/require log/slog net/http/httptest testing`},
	"internal/cryptopay/scanner": {Production: `bytes context crypto/sha256 database/sql database/sql/driver encoding/hex encoding/json errors fmt
github.com/lib/pq internal/cryptopay/explorer internal/cryptopay/mode internal/cryptopay/rpc
log/slog math math/big math/rand net/http regexp runtime/debug sort strconv strings sync sync/atomic
time`, Tests: `github.com/DATA-DOG/go-sqlmock github.com/stretchr/testify/require internal/cryptopay/ledger
internal/cryptopay/scanner io modernc.org/sqlite net/http/httptest runtime slices testing`},
	"internal/cryptopay/sweeper": {Production: `bytes context database/sql database/sql/driver encoding/hex encoding/json errors fmt
github.com/ethereum/go-ethereum/common github.com/ethereum/go-ethereum/core/types
internal/cryptopay/mode internal/cryptopay/rpc internal/cryptopay/wallet log/slog math math/big sort
strconv strings time`, Tests: `crypto/ecdsa github.com/DATA-DOG/go-sqlmock github.com/ethereum/go-ethereum/crypto github.com/lib/pq
github.com/stretchr/testify/require internal/cryptopay/scanner migrations net/http net/http/httptest
net/url os sync sync/atomic testing`},
	"internal/cryptopay/wallet": {Production: `bytes context crypto/aes crypto/cipher crypto/ecdsa crypto/ed25519 crypto/hkdf crypto/hmac
crypto/rand crypto/sha256 crypto/sha512 database/sql encoding/base64 encoding/binary encoding/hex
encoding/json errors fmt github.com/btcsuite/btcd/btcutil/hdkeychain
github.com/btcsuite/btcd/chaincfg github.com/ethereum/go-ethereum/common
github.com/ethereum/go-ethereum/core/types github.com/ethereum/go-ethereum/crypto
github.com/gagliardetto/solana-go github.com/tyler-smith/go-bip39 internal/cryptopay/addresses
internal/cryptopay/mode internal/cryptopay/wallet/shards math/big strconv strings sync time`, Tests: `database/sql/driver github.com/DATA-DOG/go-sqlmock github.com/stretchr/testify/require regexp
testing`},
	"internal/cryptopay/wallet/shards": {Production: `crypto/rand crypto/sha256 encoding/base32 errors io sort strings`, Tests: `bytes github.com/stretchr/testify/require testing`},
}

// chainPaymentRules separates project, library and standard imports for arch-go.
// Test files inherit their package's production contract. The Go extensions
// namespace uses arch-go's standard-library category even for external modules.
func chainPaymentRules(boundary dependencySet, test bool) []*dependencyRule {
	var project, libraries, standard []string
	for _, pkg := range boundary.paths(test) {
		switch {
		case pkg == "ent" || pkg == "migrations" || strings.HasPrefix(pkg, "ent/") || strings.HasPrefix(pkg, "internal/"):
			project = append(project, projectExpression(pkg))
		case strings.HasPrefix(pkg, "golang.org/x/") || !strings.Contains(strings.SplitN(pkg, "/", 2)[0], "."):
			standard = append(standard, expression(pkg))
		default:
			libraries = append(libraries, expression(pkg))
		}
	}
	return []*dependencyRule{{ShouldOnlyDependsOn: &dependencies{Internal: closed(project), External: closed(libraries), Standard: closed(standard)}}, denyProject(retiredPackages), denyProject([]string{"internal/app/..."})}
}
