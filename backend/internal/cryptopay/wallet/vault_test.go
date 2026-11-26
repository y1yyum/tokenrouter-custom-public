package wallet

import (
	"crypto/aes"
	"crypto/cipher"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWalletEnvelopeNonceContract checks that the wallet writer preserves its
// v3 wire format and rejects GCM implementations requiring an explicit nonce.
func TestWalletEnvelopeNonceContract(t *testing.T) {
	key := make([]byte, 32)
	contents := vaultContents{Deposit: "fixture"}
	first, err := sealVaultContentsV3(key, contents)
	require.NoError(t, err)
	second, err := sealVaultContentsV3(key, contents)
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	restored, err := openVaultContentsV3(key, first)
	require.NoError(t, err)
	require.Equal(t, contents, *restored)
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	manual, err := cipher.NewGCM(block)
	require.NoError(t, err)
	_, err = sealWalletEnvelope(manual, nil, managedVaultPrefixV3, managedVaultPrefixV3)
	require.ErrorIs(t, err, ErrVaultUnavailable)
}
