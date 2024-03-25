package interchaintest_test

import (
	"context"
	"fmt"
	"testing"

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

	rly := interchaintest.NewBuiltinRelayerFactory(
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
		AddRelayer(rly, "relayer").
		AddLink(interchaintest.InterchainLink{
			Chain1:  noble,
			Chain2:  gaia,
			Path:    path,
			Relayer: rly,
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

	gaiaWallets := interchaintest.GetAndFundTestUsers(t, ctx, "gaia", 1_000_000, gaia)
	gaiaWallet := gaiaWallets[0]

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

	nobleChans, err := rly.GetChannels(ctx, eRep, noble.Config().ChainID)
	require.NoError(t, err, "failed to get noble channels")
	require.Len(t, nobleChans, 1, "more than one channel found")
	nobleChan := nobleChans[0]

	gaiaReceiver := gaiaWallet.FormattedAddress()

	height, err := noble.Height(ctx)
	require.NoError(t, err, "failed to get noble height")

	_, err = nobleValidator.ExecTx(ctx, gw.fiatTfRoles.MasterMinter.KeyName(),
		"fiat-tokenfactory", "configure-minter-controller", gw.fiatTfRoles.MinterController.FormattedAddress(), gw.fiatTfRoles.Minter.FormattedAddress(), "-b", "block",
	)
	require.NoError(t, err, "failed to execute configure minter controller tx")

	err = rly.StartRelayer(ctx, eRep, path)
	require.NoError(t, err, "failed to start relayer")
	defer rly.StopRelayer(ctx, eRep)

	// Test successful transfer
	tx, err := noble.SendIBCTransfer(ctx, nobleChan.ChannelID, gw.extraWallets.User.KeyName(), ibc.WalletAmount{
		Address: gaiaReceiver,
		Denom:   denomMetadataUsdc.Base,
		Amount:  10,
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


	userBalBefore, _ := noble.GetBalance(ctx, gw.extraWallets.User.FormattedAddress(), denomMetadataUsdc.Base)

	err = rly.StartRelayer(ctx, eRep, path)
	require.NoError(t, err, "failed to start relayer")
	defer rly.StopRelayer(ctx, eRep)

	_, err = gaia.SendIBCTransfer(ctx, nobleChan.Counterparty.ChannelID, gaiaWallet.KeyName(), ibc.WalletAmount{
		Address: gw.extraWallets.User.FormattedAddress(),
		Denom:   ibcDenom,
		Amount:  10,
	}, ibc.TransferOptions{})
	require.NoError(t, err, "failed to send ibc transfer")

	require.NoError(t, testutil.WaitForBlocks(ctx, 10, noble, gaia))

	userBalAfter, _ := noble.GetBalance(ctx, gw.extraWallets.User.FormattedAddress(), denomMetadataUsdc.Base)
	require.Equal(t, userBalBefore+10, userBalAfter, "User wallet balance should have increased")
}

func mintToWallet(t *testing.T, ctx context.Context, noble *cosmos.CosmosChain, gw genesisWrapper, user ibc.Wallet) {
	nobleValidator := noble.Validators[0]
	_, err := nobleValidator.ExecTx(ctx, gw.fiatTfRoles.Minter.KeyName(),
		"fiat-tokenfactory", "mint", user.FormattedAddress(), fmt.Sprintf("%d%s", 1000000000000, denomMetadataUsdc.Base), "-b", "block",
	)
	require.NoError(t, err, "failed to execute mint to user tx")

	userBalance, err := noble.GetBalance(ctx, user.FormattedAddress(), denomMetadataUsdc.Base)
	require.NoError(t, err, "failed to get user balance")
	
	require.Equalf(t, int64(1000000000000), userBalance, "failed to mint %s to user", denomMetadataUsdc.Base)
}
