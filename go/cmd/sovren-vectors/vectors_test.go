package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// committedDir is exchange-kit/test-vectors relative to this package.
const committedDir = "../../../test-vectors"

// TestCommittedVectorsByteIdentical is the CI byte-identity gate: the four
// committed vector files must be exactly what deterministic regeneration
// produces (contracts/test-vectors.md). On failure, run
// `go run ./cmd/sovren-vectors generate --out ../test-vectors` and commit.
func TestCommittedVectorsByteIdentical(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, GenerateAll(tmp))
	mismatches, err := CompareDirs(tmp, committedDir)
	require.NoError(t, err)
	require.Empty(t, mismatches)
}

func TestGenerateDeterministic(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	require.NoError(t, GenerateAll(dirA))
	require.NoError(t, GenerateAll(dirB))
	for _, name := range VectorFiles {
		a, err := os.ReadFile(filepath.Join(dirA, name))
		require.NoError(t, err)
		b, err := os.ReadFile(filepath.Join(dirB, name))
		require.NoError(t, err)
		require.True(t, bytes.Equal(a, b), "%s not deterministic", name)
	}
}

func TestEnvelopeShape(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, GenerateAll(tmp))
	for _, name := range VectorFiles {
		data, err := os.ReadFile(filepath.Join(tmp, name))
		require.NoError(t, err)
		var env struct {
			SchemaVersion  int             `json:"schema_version"`
			KitVersion     string          `json:"kit_version"`
			UnsafeTestOnly bool            `json:"UNSAFE_TEST_ONLY"`
			Vectors        json.RawMessage `json:"vectors"`
		}
		require.NoError(t, json.Unmarshal(data, &env), name)
		require.Equal(t, schemaVersion, env.SchemaVersion, name)
		require.Equal(t, kitVersion, env.KitVersion, name)
		require.True(t, env.UnsafeTestOnly, name)
		require.NotEmpty(t, env.Vectors, name)
	}
}

// TestAddressVectorsCoverEveryErrorCode pins FR-014: every documented address
// error code appears in the committed pure-validation entries.
func TestAddressVectorsCoverEveryErrorCode(t *testing.T) {
	vectors, err := loadSuite(committedDir, "addresses")
	require.NoError(t, err)
	covered := map[string]bool{}
	for _, v := range vectors {
		if v.Validation != nil && v.Validation.ErrorCode != "" {
			covered[v.Validation.ErrorCode] = true
		}
	}
	for _, code := range []string{
		"ADDRESS_EMPTY", "ADDRESS_INVALID_BECH32", "ADDRESS_WRONG_PREFIX",
		"ADDRESS_WRONG_LENGTH", "ADDRESS_NOT_ACCOUNT_TYPE", "ADDRESS_PROHIBITED",
		"ADDRESS_WHITESPACE",
	} {
		require.True(t, covered[code], "no vector covers %s", code)
	}
}

// TestAmountVectorsCoverEveryErrorCode pins FR-018 the same way.
func TestAmountVectorsCoverEveryErrorCode(t *testing.T) {
	vectors, err := loadSuite(committedDir, "amounts")
	require.NoError(t, err)
	covered := map[string]bool{}
	for _, v := range vectors {
		if v.ErrorCode != "" {
			covered[v.ErrorCode] = true
		}
	}
	for _, code := range []string{
		"AMOUNT_TOO_MANY_DECIMALS", "AMOUNT_NEGATIVE", "AMOUNT_SCIENTIFIC_NOTATION",
		"AMOUNT_COMMAS", "AMOUNT_EMPTY", "AMOUNT_NOT_NUMERIC", "AMOUNT_EXCEEDS_MAX",
	} {
		require.True(t, covered[code], "no vector covers %s", code)
	}
}

// TestGoConformanceSelfConsistent runs the Go runner over the committed
// vectors and diffs it against itself: expectation checks must all hold and
// coverage must be complete.
func TestGoConformanceSelfConsistent(t *testing.T) {
	results, err := runGoConformance(committedDir)
	require.NoError(t, err)
	require.NotEmpty(t, results)
	byKey := map[string]conformanceResult{}
	for _, r := range results {
		byKey[r.Suite+"/"+r.ID] = r
	}
	divergences, compared, err := compareResults(committedDir, byKey, byKey)
	require.NoError(t, err)
	require.Empty(t, divergences)
	total := 0
	for _, suite := range conformanceSuites {
		total += compared[suite]
	}
	require.Equal(t, len(results), total)
}

// TestInvalidCasesCoverEveryCategory pins FR-060: every required category —
// address, amount, AND transaction — appears in the committed file.
func TestInvalidCasesCoverEveryCategory(t *testing.T) {
	vectors, err := loadSuite(committedDir, "invalid-cases")
	require.NoError(t, err)
	covered := map[string]bool{}
	for _, v := range vectors {
		covered[v.Category] = true
	}
	for _, category := range []string{
		"WRONG_BECH32_PREFIX", "INVALID_CHECKSUM", "VALIDATOR_OPERATOR_ADDRESS",
		"ZERO_AMOUNT", "NEGATIVE_AMOUNT", "WRONG_DENOM", "EXCESS_DECIMALS",
		"INCORRECT_SEQUENCE", "WRONG_CHAIN_ID", "INSUFFICIENT_FEE",
		"INSUFFICIENT_FUNDS", "MALFORMED_PUBKEY", "INVALID_SIGNATURE",
		"DUPLICATE_WITHDRAWAL_ID", "FAILED_TX_RESULT",
	} {
		require.True(t, covered[category], "no vector covers %s", category)
	}
}

// TestTxVectorsSpanFourFiles pins the shared-id contract: every MsgSend
// logical vector appears in all four transaction files.
func TestTxVectorsSpanFourFiles(t *testing.T) {
	ids := map[string]map[string]bool{}
	for _, suite := range txSuites {
		vectors, err := loadSuite(committedDir, suite)
		require.NoError(t, err)
		for _, v := range vectors {
			if ids[v.ID] == nil {
				ids[v.ID] = map[string]bool{}
			}
			ids[v.ID][suite] = true
		}
	}
	require.NotEmpty(t, ids)
	for id, suites := range ids {
		require.Len(t, suites, len(txSuites), "vector %s does not span all four files", id)
	}
}

func TestVerifySubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"verify", "--dir", committedDir}, &out, &errOut)
	require.Equal(t, 0, code, "stdout=%s stderr=%s", out.String(), errOut.String())
	require.Contains(t, out.String(), "byte-identical")
}

func TestDeriveNewTestAddress(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"derive", "--new-test-address"}, &out, &errOut)
	require.Equal(t, 0, code, errOut.String())
	var got struct {
		UnsafeTestOnly bool   `json:"UNSAFE_TEST_ONLY"`
		Mnemonic       string `json:"mnemonic"`
		DerivationPath string `json:"derivation_path"`
		Bech32Address  string `json:"bech32_address"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	require.True(t, got.UnsafeTestOnly)
	require.NotEmpty(t, got.Mnemonic)
	require.NotEmpty(t, got.Bech32Address)

	// Two invocations must differ — this is the one nondeterministic path.
	var out2 bytes.Buffer
	code = run([]string{"derive", "--new-test-address"}, &out2, &errOut)
	require.Equal(t, 0, code)
	require.NotEqual(t, out.String(), out2.String())
}
