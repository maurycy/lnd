package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/lightninglabs/neutrino"
	"github.com/lightningnetwork/lnd/chainntnfs/neutrinonotify"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/routing/chainview"
	"github.com/roasbeef/btcwallet/chain"
	"github.com/roasbeef/btcwallet/walletdb"
)

func (b *neutrinoConfig) ParseRPCParams(cConfig *chainConfig, net chainCode,
	funcName string) error {

	// No need to get RPC parameters.
	return nil
}

// newChainControlFromConfig attempts to create a chainControl instance
// according to the parameters in the passed lnd configuration. Currently two
// branches of chainControl instances exist: one backed by a running btcd
// full-node, and the other backed by a running neutrino light client instance.
func (b *neutrinoConfig) NewChainControlFromConfig(cfg *config,
	chanDB *channeldb.DB, privateWalletPw, publicWalletPw []byte,
	birthday time.Time, recoveryWindow uint32) (*chainControl, func(), error) {

	// Set the RPC config from the "home" chain. Multi-chain isn't yet
	// active, so we'll restrict usage to a particular chain for now.
	homeChainConfig := cfg.Bitcoin
	if registeredChains.PrimaryChain() == litecoinChain {
		homeChainConfig = cfg.Litecoin
	}
	ltndLog.Infof("Primary chain is set to: %v",
		registeredChains.PrimaryChain())

	cc := &chainControl{}

	switch registeredChains.PrimaryChain() {
	case bitcoinChain:
		cc.routingPolicy = htlcswitch.ForwardingPolicy{
			MinHTLC:       cfg.Bitcoin.MinHTLC,
			BaseFee:       cfg.Bitcoin.BaseFee,
			FeeRate:       cfg.Bitcoin.FeeRate,
			TimeLockDelta: cfg.Bitcoin.TimeLockDelta,
		}
		cc.feeEstimator = lnwallet.StaticFeeEstimator{
			FeeRate: defaultBitcoinStaticFeeRate,
		}
	case litecoinChain:
		cc.routingPolicy = htlcswitch.ForwardingPolicy{
			MinHTLC:       cfg.Litecoin.MinHTLC,
			BaseFee:       cfg.Litecoin.BaseFee,
			FeeRate:       cfg.Litecoin.FeeRate,
			TimeLockDelta: cfg.Litecoin.TimeLockDelta,
		}
		cc.feeEstimator = lnwallet.StaticFeeEstimator{
			FeeRate: defaultLitecoinStaticFeeRate,
		}
	default:
		return nil, nil, fmt.Errorf("Default routing policy for "+
			"chain %v is unknown", registeredChains.PrimaryChain())
	}

	walletConfig := &btcwallet.Config{
		PrivatePass:    privateWalletPw,
		PublicPass:     publicWalletPw,
		Birthday:       birthday,
		RecoveryWindow: recoveryWindow,
		DataDir:        homeChainConfig.ChainDir,
		NetParams:      activeNetParams.Params,
		FeeEstimator:   cc.feeEstimator,
		CoinType:       activeNetParams.CoinType,
	}

	var (
		err     error
		cleanUp func()
	)

	// First we'll open the database file for neutrino, creating
	// the database if needed. We append the normalized network name
	// here to match the behavior of btcwallet.
	neutrinoDbPath := filepath.Join(homeChainConfig.ChainDir,
		normalizeNetwork(activeNetParams.Name))

	// Ensure that the neutrino db path exists.
	if err := os.MkdirAll(neutrinoDbPath, 0700); err != nil {
		return nil, nil, err
	}

	dbName := filepath.Join(neutrinoDbPath, "neutrino.db")
	nodeDatabase, err := walletdb.Create("bdb", dbName)
	if err != nil {
		return nil, nil, err
	}

	// With the database open, we can now create an instance of the
	// neutrino light client. We pass in relevant configuration
	// parameters required.
	config := neutrino.Config{
		DataDir:      neutrinoDbPath,
		Database:     nodeDatabase,
		ChainParams:  *activeNetParams.Params,
		AddPeers:     cfg.NeutrinoMode.AddPeers,
		ConnectPeers: cfg.NeutrinoMode.ConnectPeers,
		Dialer: func(addr net.Addr) (net.Conn, error) {
			return cfg.net.Dial(addr.Network(), addr.String())
		},
		NameResolver: func(host string) ([]net.IP, error) {
			addrs, err := cfg.net.LookupHost(host)
			if err != nil {
				return nil, err
			}

			ips := make([]net.IP, 0, len(addrs))
			for _, strIP := range addrs {
				ip := net.ParseIP(strIP)
				if ip == nil {
					continue
				}

				ips = append(ips, ip)
			}

			return ips, nil
		},
	}
	neutrino.WaitForMoreCFHeaders = time.Second * 1
	neutrino.MaxPeers = 8
	neutrino.BanDuration = 5 * time.Second
	svc, err := neutrino.NewChainService(config)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create neutrino: %v", err)
	}
	svc.Start()

	// Next we'll create the instances of the ChainNotifier and
	// FilteredChainView interface which is backed by the neutrino
	// light client.
	cc.chainNotifier, err = neutrinonotify.New(svc)
	if err != nil {
		return nil, nil, err
	}
	cc.chainView, err = chainview.NewCfFilteredChainView(svc)
	if err != nil {
		return nil, nil, err
	}

	// Finally, we'll set the chain source for btcwallet, and
	// create our clean up function which simply closes the
	// database.
	walletConfig.ChainSource = chain.NewNeutrinoClient(
		activeNetParams.Params, svc,
	)
	cleanUp = func() {
		svc.Stop()
		nodeDatabase.Close()
	}

	wc, err := btcwallet.New(*walletConfig)
	if err != nil {
		fmt.Printf("unable to create wallet controller: %v\n", err)
		return nil, nil, err
	}

	cc.msgSigner = wc
	cc.signer = wc
	cc.chainIO = wc

	// Select the default channel constraints for the primary chain.
	channelConstraints := defaultBtcChannelConstraints
	if registeredChains.PrimaryChain() == litecoinChain {
		channelConstraints = defaultLtcChannelConstraints
	}

	keyRing := keychain.NewBtcWalletKeyRing(
		wc.InternalWallet(), activeNetParams.CoinType,
	)

	// Create, and start the lnwallet, which handles the core payment
	// channel logic, and exposes control via proxy state machines.
	walletCfg := lnwallet.Config{
		Database:           chanDB,
		Notifier:           cc.chainNotifier,
		WalletController:   wc,
		Signer:             cc.signer,
		FeeEstimator:       cc.feeEstimator,
		SecretKeyRing:      keyRing,
		ChainIO:            cc.chainIO,
		DefaultConstraints: channelConstraints,
		NetParams:          *activeNetParams.Params,
	}
	wallet, err := lnwallet.NewLightningWallet(walletCfg)
	if err != nil {
		fmt.Printf("unable to create wallet: %v\n", err)
		return nil, nil, err
	}
	if err := wallet.Startup(); err != nil {
		fmt.Printf("unable to start wallet: %v\n", err)
		return nil, nil, err
	}

	ltndLog.Info("LightningWallet opened")

	cc.wallet = wallet

	return cc, cleanUp, nil
}
