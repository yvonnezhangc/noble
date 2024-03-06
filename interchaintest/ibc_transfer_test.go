package interchaintest_test

import (
	"context"
	"fmt"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	transfertypes "github.com/cosmos/ibc-go/v4/modules/apps/transfer/types"
	"github.com/strangelove-ventures/interchaintest/v4"
	"github.com/strangelove-ventures/interchaintest/v4/chain/cosmos"
	"github.com/strangelove-ventures/interchaintest/v4/ibc"
	"github.com/strangelove-ventures/interchaintest/v4/testreporter"
	"github.com/strangelove-ventures/interchaintest/v4/testutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// run `make local-image`to rebuild updated binary before running test
func TestIBCTransfer(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	t.Parallel()

	ctx := context.Background()

	rep := testreporter.NewNopReporter()
	eRep := rep.RelayerExecReporter(t)

	client, network := interchaintest.DockerSetup(t)

	var gw genesisWrapper

	nv := 1
	nf := 0

	cf := interchaintest.NewBuiltinChainFactory(zaptest.NewLogger(t), []*interchaintest.ChainSpec{
		nobleChainSpec(ctx, &gw, "noble-1", nv, nf, false, false, true, false),
		{
			Name:          "gaia",
			Version:       "v9.0.2",
			NumValidators: &nv,
			NumFullNodes:  &nf,
		},
	})

	chains, err := cf.Chains(t.Name())
	require.NoError(t, err)

	r := interchaintest.NewBuiltinRelayerFactory(
		ibc.CosmosRly,
		zaptest.NewLogger(t),
		relayerImage,
	).Build(t, client, network)

	var gaia *cosmos.CosmosChain
	gw.chain, gaia = chains[0].(*cosmos.CosmosChain), chains[1].(*cosmos.CosmosChain)
	noble := gw.chain

	path := "p"

	ic := interchaintest.NewInterchain().
		AddChain(noble).
		AddChain(gaia).
		AddRelayer(r, "relayer").
		AddLink(interchaintest.InterchainLink{
			Chain1:  noble,
			Chain2:  gaia,
			Path:    path,
			Relayer: r,
		})

	require.NoError(t, ic.Build(ctx, eRep, interchaintest.InterchainBuildOptions{
		TestName:  t.Name(),
		Client:    client,
		NetworkID: network,

		SkipPathCreation: false,
	}))
	t.Cleanup(func() {
		_ = ic.Close()
	})

	nobleValidator := noble.Validators[0]

	_, err = nobleValidator.ExecTx(ctx, gw.fiatTfRoles.MasterMinter.KeyName(),
		"fiat-tokenfactory", "configure-minter-controller", gw.fiatTfRoles.MinterController.FormattedAddress(), gw.fiatTfRoles.Minter.FormattedAddress(), "-b", "block",
	)
	require.NoError(t, err, "failed to execute configure minter controller tx")

	_, err = nobleValidator.ExecTx(ctx, gw.fiatTfRoles.MinterController.KeyName(),
		"fiat-tokenfactory", "configure-minter", gw.fiatTfRoles.Minter.FormattedAddress(), "2000000000000"+denomMetadataUsdc.Base, "-b", "block",
	)
	require.NoError(t, err, "failed to execute configure minter tx")

	mintToWallet(t, ctx, noble, gw, gw.extraWallets.User)
	mintToWallet(t, ctx, noble, gw, gw.extraWallets.User2)

	nobleChans, err := r.GetChannels(ctx, eRep, noble.Config().ChainID)
	require.NoError(t, err, "failed to get noble channels")
	require.Len(t, nobleChans, 1, "more than one channel found")
	nobleChan := nobleChans[0]

	gaiaReceiver := "cosmos169xaqmxumqa829gg73nxrenkhhd2mrs36j3vrz"

	err = r.StartRelayer(ctx, eRep, path)
	require.NoError(t, err, "failed to start relayer")
	defer r.StopRelayer(ctx, eRep)

	height, err := noble.Height(ctx)
	require.NoError(t, err, "failed to get noble height")

	_, err = nobleValidator.ExecTx(ctx, gw.fiatTfRoles.MasterMinter.KeyName(),
		"fiat-tokenfactory", "configure-minter-controller", gw.fiatTfRoles.MinterController.FormattedAddress(), gw.fiatTfRoles.Minter.FormattedAddress(), "-b", "block",
	)
	require.NoError(t, err, "failed to execute configure minter controller tx")

	// blacklist user
	_, err = nobleValidator.ExecTx(ctx, gw.fiatTfRoles.Blacklister.KeyName(),
		"fiat-tokenfactory", "blacklist", gw.extraWallets.User.FormattedAddress(), "-b", "block",
	)
	require.NoError(t, err, "failed to blacklist user address")

	tx, err := testAuthzTransfer(t, ctx, noble, gw, denomMetadataUsdc.Base, nobleChan, gw.extraWallets.User, gaiaReceiver, gw.extraWallets.User2)
	require.Error(t, err, "failed to block IBC transfer from blacklisted sender")

	userBech32Address := sdk.MustBech32ifyAddressBytes("noble", gw.extraWallets.User.Address())
	userGaiaAddress := sdk.Bech32MainPrefix + sdk.MustAccAddressFromBech32(userBech32Address).String()
	tx, err = testAuthzTransfer(t, ctx, noble, gw, denomMetadataUsdc.Base, nobleChan, gw.extraWallets.User2, userGaiaAddress, gw.extraWallets.Alice)
	require.Error(t, err, "failed to block IBC transfer to blacklisted receiver")

	tx, err = testAuthzTransfer(t, ctx, noble, gw, denomMetadataUsdc.Base, nobleChan, gw.extraWallets.User2, gaiaReceiver, gw.extraWallets.User)
	require.Error(t, err, "failed to block IBC transfer initiated by a blacklisted grantee")

	// unblacklist user
	_, err = nobleValidator.ExecTx(ctx, gw.fiatTfRoles.Blacklister.KeyName(),
		"fiat-tokenfactory", "unblacklist", gw.extraWallets.User.FormattedAddress(), "-b", "block",
	)
	require.NoError(t, err, "failed to unblacklist user address")

	// Pause asset
	_, err = nobleValidator.ExecTx(ctx, gw.fiatTfRoles.Pauser.KeyName(),
		"fiat-tokenfactory", "pause", "-b", "block",
	)
	require.NoError(t, err, "failed to pause")

	tx, err = testAuthzTransfer(t, ctx, noble, gw, denomMetadataUsdc.Base, nobleChan, gw.extraWallets.User, gaiaReceiver, gw.extraWallets.User2)
	require.Error(t, err, "failed to block IBC transfer when asset is paused")

	// Unpause asset
	_, err = nobleValidator.ExecTx(ctx, gw.fiatTfRoles.Pauser.KeyName(),
		"fiat-tokenfactory", "unpause", "-b", "block",
	)
	require.NoError(t, err, "failed to unpause")

	// Test successful transfer
	tx, err = noble.SendIBCTransfer(ctx, nobleChan.ChannelID, gw.extraWallets.User.KeyName(), ibc.WalletAmount{
		Address: gaiaReceiver,
		Denom:   denomMetadataUsdc.Base,
		Amount:  100000000,
	}, ibc.TransferOptions{})
	require.NoError(t, err, "failed to send ibc transfer from noble")

	_, err = testutil.PollForAck(ctx, noble, height, height+10, tx.Packet)
	require.NoError(t, err, "failed to find ack for ibc transfer")

	userBalance, err := noble.GetBalance(ctx, gw.extraWallets.User.FormattedAddress(), denomMetadataUsdc.Base)
	require.NoError(t, err, "failed to get user balance")
	require.Equal(t, int64(999900000000), userBalance, "user balance is incorrect")

	prefixedDenom := transfertypes.GetPrefixedDenom(nobleChan.Counterparty.PortID, nobleChan.Counterparty.ChannelID, denomMetadataUsdc.Base)
	denomTrace := transfertypes.ParseDenomTrace(prefixedDenom)
	ibcDenom := denomTrace.IBCDenom()

	// 100000000 (Transfer Amount) * .0001 (1 BPS) = 10000 taken as fees
	receiverBalance, err := gaia.GetBalance(ctx, gaiaReceiver, ibcDenom)
	require.NoError(t, err, "failed to get receiver balance")
	require.Equal(t, int64(99990000), receiverBalance, "receiver balance incorrect")
}

func mintToWallet(t *testing.T, ctx context.Context, noble *cosmos.CosmosChain, gw genesisWrapper, user ibc.Wallet) {
	nobleValidator := noble.Validators[0]
	_, err := nobleValidator.ExecTx(ctx, gw.fiatTfRoles.Minter.KeyName(),
		"fiat-tokenfactory", "mint", user.FormattedAddress(), "1000000000000"+denomMetadataUsdc.Base, "-b", "block",
	)
	require.NoError(t, err, "failed to execute mint to user tx")

	userBalance := getBalance(t, ctx, denomMetadataUsdc.Base, noble, user)
	require.Equalf(t, int64(1000000000000), userBalance, "failed to mint %s to user", denomMetadataUsdc.Base)
}

func testAuthzTransfer(t *testing.T, ctx context.Context, noble *cosmos.CosmosChain, gw genesisWrapper, mintingDenom string, nobleChan ibc.ChannelOutput, fromWallet ibc.Wallet, receiver string, granteeWallet ibc.Wallet)  (ibc.Tx, error) {
	nobleValidator := noble.Validators[0]
	
	_, err := nobleValidator.ExecTx(ctx, fromWallet.KeyName(), "authz", "grant", granteeWallet.FormattedAddress(), "send", "--spend-limit", fmt.Sprintf("%d%s", 100, mintingDenom))
	require.NoError(t, err, "failed to grant permissions")

	return noble.SendIBCTransfer(ctx, nobleChan.ChannelID, gw.extraWallets.User.KeyName(), ibc.WalletAmount{
		Address: receiver,
		Denom:   denomMetadataUsdc.Base,
		Amount:  100000000,
	}, ibc.TransferOptions{})
}

func getBalance(t *testing.T, ctx context.Context, mintingDenom string, noble *cosmos.CosmosChain, wallet ibc.Wallet) int64 {
	bal, err := noble.GetBalance(ctx, wallet.FormattedAddress(), mintingDenom)
	require.NoError(t, err, "failed to get user balance")
	return bal
}
