package main

import (
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/lightningnetwork/lnd/chainntnfs/btcdnotify"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/routing/chainview"
	"github.com/roasbeef/btcd/rpcclient"
	"github.com/roasbeef/btcwallet/chain"
)

func (conf *btcdConfig) ParseRPCParams(cConfig *chainConfig, net chainCode,
	funcName string) error {

	var daemonName, confDir, confFile string

	// Get the daemon name for displaying proper errors.
	switch net {
	case bitcoinChain:
		daemonName = "btcd"
	case litecoinChain:
		daemonName = "ltcd"
	}

	// If both RPCUser and RPCPass are set, we assume those
	// credentials are good to use.
	if conf.RPCUser != "" && conf.RPCPass != "" {
		return nil
	}

	// If only ONE of RPCUser or RPCPass is set, we assume the
	// user did that unintentionally.
	if conf.RPCUser != "" || conf.RPCPass != "" {
		return fmt.Errorf("please set both or neither of "+
			"%[1]v.rpcuser, %[1]v.rpcpass", daemonName)
	}

	switch net {
	case bitcoinChain:
		confDir = conf.Dir
		confFile = "btcd"
	case litecoinChain:
		confDir = conf.Dir
		confFile = "ltcd"
	}

	// If we're in simnet mode, then the running btcd instance won't read
	// the RPC credentials from the configuration. So if lnd wasn't
	// specified the parameters, then we won't be able to start.
	if cConfig.SimNet {
		str := "%v: rpcuser and rpcpass must be set to your btcd " +
			"node's RPC parameters for simnet mode"
		return fmt.Errorf(str, funcName)
	}

	fmt.Println("Attempting automatic RPC configuration to " + daemonName)

	confFile = filepath.Join(confDir, fmt.Sprintf("%v.conf", confFile))

	rpcUser, rpcPass, err := extractBtcdRPCParams(confFile)
	if err != nil {
		return fmt.Errorf("unable to extract RPC credentials:"+
			" %v, cannot start w/o RPC connection",
			err)
	}
	conf.RPCUser, conf.RPCPass = rpcUser, rpcPass

	fmt.Printf("Automatically obtained %v's RPC credentials\n", daemonName)

	return nil
}

// newChainControlFromConfig attempts to create a chainControl instance
// according to the parameters in the passed lnd configuration. Currently two
// branches of chainControl instances exist: one backed by a running btcd
// full-node, and the other backed by a running neutrino light client instance.
func (conf *btcdConfig) NewChainControlFromConfig(cfg *config,
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

	// Otherwise, we'll be speaking directly via RPC to a node.
	//
	// So first we'll load btcd/ltcd's TLS cert for the RPC
	// connection. If a raw cert was specified in the config, then
	// we'll set that directly. Otherwise, we attempt to read the
	// cert from the path specified in the config.
	var rpcCert []byte
	if conf.RawRPCCert != "" {
		rpcCert, err = hex.DecodeString(conf.RawRPCCert)
		if err != nil {
			return nil, nil, err
		}
	} else {
		certFile, err := os.Open(conf.RPCCert)
		if err != nil {
			return nil, nil, err
		}
		rpcCert, err = ioutil.ReadAll(certFile)
		if err != nil {
			return nil, nil, err
		}
		if err := certFile.Close(); err != nil {
			return nil, nil, err
		}
	}

	// If the specified host for the btcd/ltcd RPC server already
	// has a port specified, then we use that directly. Otherwise,
	// we assume the default port according to the selected chain
	// parameters.
	var btcdHost string
	if strings.Contains(conf.RPCHost, ":") {
		btcdHost = conf.RPCHost
	} else {
		btcdHost = fmt.Sprintf("%v:%v", conf.RPCHost,
			activeNetParams.rpcPort)
	}

	btcdUser := conf.RPCUser
	btcdPass := conf.RPCPass
	rpcConfig := &rpcclient.ConnConfig{
		Host:                 btcdHost,
		Endpoint:             "ws",
		User:                 btcdUser,
		Pass:                 btcdPass,
		Certificates:         rpcCert,
		DisableTLS:           false,
		DisableConnectOnNew:  true,
		DisableAutoReconnect: false,
	}
	cc.chainNotifier, err = btcdnotify.New(rpcConfig)
	if err != nil {
		return nil, nil, err
	}

	// Finally, we'll create an instance of the default chain view to be
	// used within the routing layer.
	cc.chainView, err = chainview.NewBtcdFilteredChainView(*rpcConfig)
	if err != nil {
		srvrLog.Errorf("unable to create chain view: %v", err)
		return nil, nil, err
	}

	// Create a special websockets rpc client for btcd which will be used
	// by the wallet for notifications, calls, etc.
	chainRPC, err := chain.NewRPCClient(activeNetParams.Params, btcdHost,
		btcdUser, btcdPass, rpcCert, false, 20)
	if err != nil {
		return nil, nil, err
	}

	walletConfig.ChainSource = chainRPC

	// If we're not in simnet or regtest mode, then we'll attempt
	// to use a proper fee estimator for testnet.
	if !cfg.Bitcoin.SimNet && !cfg.Litecoin.SimNet &&
		!cfg.Bitcoin.RegTest && !cfg.Litecoin.RegTest {

		ltndLog.Infof("Initializing btcd backed fee estimator")

		// Finally, we'll re-initialize the fee estimator, as
		// if we're using btcd as a backend, then we can use
		// live fee estimates, rather than a statically coded
		// value.
		fallBackFeeRate := lnwallet.SatPerVByte(25)
		cc.feeEstimator, err = lnwallet.NewBtcdFeeEstimator(
			*rpcConfig, fallBackFeeRate,
		)
		if err != nil {
			return nil, nil, err
		}
		if err := cc.feeEstimator.Start(); err != nil {
			return nil, nil, err
		}
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

// extractBtcdRPCParams attempts to extract the RPC credentials for an existing
// btcd instance. The passed path is expected to be the location of btcd's
// application data directory on the target system.
func extractBtcdRPCParams(btcdConfigPath string) (string, string, error) {
	// First, we'll open up the btcd configuration file found at the target
	// destination.
	btcdConfigFile, err := os.Open(btcdConfigPath)
	if err != nil {
		return "", "", err
	}
	defer btcdConfigFile.Close()

	// With the file open extract the contents of the configuration file so
	// we can attempt to locate the RPC credentials.
	configContents, err := ioutil.ReadAll(btcdConfigFile)
	if err != nil {
		return "", "", err
	}

	// Attempt to locate the RPC user using a regular expression. If we
	// don't have a match for our regular expression then we'll exit with
	// an error.
	rpcUserRegexp, err := regexp.Compile(`(?m)^\s*rpcuser\s*=\s*([^\s]+)`)
	if err != nil {
		return "", "", err
	}
	userSubmatches := rpcUserRegexp.FindSubmatch(configContents)
	if userSubmatches == nil {
		return "", "", fmt.Errorf("unable to find rpcuser in config")
	}

	// Similarly, we'll use another regular expression to find the set
	// rpcpass (if any). If we can't find the pass, then we'll exit with an
	// error.
	rpcPassRegexp, err := regexp.Compile(`(?m)^\s*rpcpass\s*=\s*([^\s]+)`)
	if err != nil {
		return "", "", err
	}
	passSubmatches := rpcPassRegexp.FindSubmatch(configContents)
	if passSubmatches == nil {
		return "", "", fmt.Errorf("unable to find rpcuser in config")
	}

	return string(userSubmatches[1]), string(passSubmatches[1]), nil
}
