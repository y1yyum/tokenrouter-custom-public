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
// These in-memory operations are shared across package boundaries. I/O,
// chain libraries and repository dependencies remain explicitly scoped below.
const chainPaymentValueImports = "bytes context encoding/hex encoding/json errors fmt maps math slices sort strconv strings sync sync/atomic time"

var chainPaymentPackages = map[string]dependencySet{
	"internal/cryptopay": {Production: `crypto/rand crypto/sha256 database/sql database/sql/driver ent ent/paymentorder
github.com/ethereum/go-ethereum/common github.com/ethereum/go-ethereum/common/hexutil
github.com/google/uuid github.com/lib/pq github.com/redis/go-redis/v9 github.com/shopspring/decimal
golang.org/x/net/publicsuffix golang.org/x/sync/singleflight internal/billing internal/config
internal/cryptopay/addresses internal/cryptopay/explorer internal/cryptopay/ledger
internal/cryptopay/mode internal/cryptopay/nativeprice internal/cryptopay/notify
internal/cryptopay/rpc internal/cryptopay/scanner internal/cryptopay/sweeper
internal/cryptopay/wallet internal/egress/urlpolicy internal/notification internal/notification/bark
internal/payment internal/payment/postgres internal/pkg/apperror internal/settings io log/slog
math/big math/rand net net/http net/mail net/url regexp unicode`, Tests: `ent/enttest ent/runtime entgo.io/ent/dialect entgo.io/ent/dialect/sql github.com/DATA-DOG/go-sqlmock
github.com/alicebob/miniredis/v2 github.com/stretchr/testify/require golang.org/x/sync/errgroup
internal/billing/postgres internal/identity internal/identity/postgres internal/payment/testkit
internal/settings/postgres internal/testutil/postgrescontainer migrations modernc.org/sqlite
net/http/httptest os path/filepath reflect`},
	"internal/cryptopay/addresses": {Production: `database/sql internal/cryptopay/mode`, Tests: `github.com/DATA-DOG/go-sqlmock github.com/stretchr/testify/require regexp`},
	"internal/cryptopay/explorer":  {Production: `io math/big net/http net/url`, Tests: `net/http/httptest`},
	"internal/cryptopay/httpapi": {Production: `encoding/base64 encoding/csv github.com/gin-gonic/gin internal/audit/httpapi internal/config
internal/cryptopay internal/cryptopay/addresses internal/cryptopay/mode internal/cryptopay/rpc
internal/cryptopay/sweeper internal/cryptopay/wallet internal/identity internal/identity/httpapi
internal/identity/httpapi/authctx internal/infra/telemetry internal/notification
internal/server/httpx io log/slog net net/http net/url os reflect unicode/utf8`, Tests: ``},
	"internal/cryptopay/ledger": {Production: `crypto/sha256 database/sql github.com/shopspring/decimal internal/cryptopay/mode
internal/cryptopay/nativeprice internal/cryptopay/scanner log/slog math/big`, Tests: `database/sql/driver github.com/DATA-DOG/go-sqlmock github.com/stretchr/testify/require
modernc.org/sqlite`},
	"internal/cryptopay/mode":        {Production: `os`, Tests: ``},
	"internal/cryptopay/nativeprice": {Production: `github.com/shopspring/decimal io net/http net/url`, Tests: `github.com/stretchr/testify/require net/http/httptest`},
	"internal/cryptopay/notify":      {Production: `io log/slog net/http net/url`, Tests: `github.com/stretchr/testify/require net/http/httptest`},
	"internal/cryptopay/rpc": {Production: `github.com/ethereum/go-ethereum/common/hexutil golang.org/x/net/publicsuffix
internal/egress/urlpolicy io math/big math/rand net net/http net/netip net/url regexp`, Tests: `github.com/stretchr/testify/require log/slog net/http/httptest`},
	"internal/cryptopay/scanner": {Production: `crypto/sha256 database/sql database/sql/driver github.com/lib/pq internal/cryptopay/explorer
internal/cryptopay/mode internal/cryptopay/rpc log/slog math/big math/rand net/http regexp
runtime/debug`, Tests: `github.com/DATA-DOG/go-sqlmock github.com/stretchr/testify/require internal/cryptopay/ledger
internal/cryptopay/scanner io modernc.org/sqlite net/http/httptest runtime`},
	"internal/cryptopay/sweeper": {Production: `database/sql database/sql/driver github.com/ethereum/go-ethereum/common
github.com/ethereum/go-ethereum/core/types internal/cryptopay/mode internal/cryptopay/rpc
internal/cryptopay/wallet log/slog math/big`, Tests: `crypto/ecdsa github.com/DATA-DOG/go-sqlmock github.com/ethereum/go-ethereum/crypto github.com/lib/pq
github.com/stretchr/testify/require internal/cryptopay/scanner migrations net/http net/http/httptest
net/url os`},
	"internal/cryptopay/wallet": {Production: `crypto/aes crypto/cipher crypto/ecdsa crypto/ed25519 crypto/hkdf crypto/hmac crypto/rand
crypto/sha256 crypto/sha512 database/sql encoding/base64 encoding/binary
github.com/btcsuite/btcd/btcutil/hdkeychain github.com/btcsuite/btcd/chaincfg
github.com/ethereum/go-ethereum/common github.com/ethereum/go-ethereum/core/types
github.com/ethereum/go-ethereum/crypto github.com/gagliardetto/solana-go
github.com/tyler-smith/go-bip39 internal/cryptopay/addresses internal/cryptopay/mode
internal/cryptopay/wallet/shards math/big`, Tests: `database/sql/driver github.com/DATA-DOG/go-sqlmock github.com/stretchr/testify/require regexp`},
	"internal/cryptopay/wallet/shards": {Production: `crypto/rand crypto/sha256 encoding/base32 io`, Tests: `github.com/stretchr/testify/require`},
}

// chainPaymentRules separates project, library and standard imports for arch-go.
// Test files inherit their package's production contract. The Go extensions
// namespace uses arch-go's standard-library category even for external modules.
func chainPaymentRules(boundary dependencySet, test bool) []*dependencyRule {
	var project, libraries []string
	standard := expressions(strings.Fields(chainPaymentValueImports), false)
	if test {
		standard = append(standard, expression("testing"))
	}
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
