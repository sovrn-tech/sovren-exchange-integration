package tx

import (
	"fmt"
	"sync"

	txsigning "cosmossdk.io/x/tx/signing"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/codec"
	addresscodec "github.com/cosmos/cosmos-sdk/codec/address"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/std"
	signingtypes "github.com/cosmos/cosmos-sdk/types/tx/signing"
	authtx "github.com/cosmos/cosmos-sdk/x/auth/tx"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/gogoproto/proto"
)

const (
	Bech32PrefixAccount   = "sovr"
	Bech32PrefixValidator = "sovrvaloper"
	Denom                 = "usovr"
	MsgSendTypeURL        = "/cosmos.bank.v1beta1.MsgSend"

	accountAddressLen = 20
)

var accountAddressCodec = addresscodec.NewBech32Codec(Bech32PrefixAccount)

type kitCodec struct {
	txConfig client.TxConfig
	registry codectypes.InterfaceRegistry
}

// loadCodec builds the package TxConfig exactly once. Bech32 prefixes come
// from the explicit signing options — never from the process-global
// sdk.GetConfig() — so this package works unchanged in processes that handle
// multiple Cosmos chains. SIGN_MODE_DIRECT is the only enabled sign mode.
var loadCodec = sync.OnceValues(func() (*kitCodec, error) {
	signingOpts := txsigning.Options{
		AddressCodec:          addresscodec.NewBech32Codec(Bech32PrefixAccount),
		ValidatorAddressCodec: addresscodec.NewBech32Codec(Bech32PrefixValidator),
	}
	registry, err := codectypes.NewInterfaceRegistryWithOptions(codectypes.InterfaceRegistryOptions{
		ProtoFiles:     proto.HybridResolver,
		SigningOptions: signingOpts,
	})
	if err != nil {
		return nil, fmt.Errorf("tx: interface registry: %w", err)
	}
	std.RegisterInterfaces(registry)
	banktypes.RegisterInterfaces(registry)

	txConfig, err := authtx.NewTxConfigWithOptions(codec.NewProtoCodec(registry), authtx.ConfigOptions{
		EnabledSignModes: []signingtypes.SignMode{signingtypes.SignMode_SIGN_MODE_DIRECT},
		SigningOptions:   &signingOpts,
	})
	if err != nil {
		return nil, fmt.Errorf("tx: tx config: %w", err)
	}
	return &kitCodec{txConfig: txConfig, registry: registry}, nil
})
