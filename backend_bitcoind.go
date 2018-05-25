package main

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lightningnetwork/lnd/chainntnfs/bitcoindnotify"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/routing/chainview"
	"github.com/roasbeef/btcd/rpcclient"
	"github.com/roasbeef/btcwallet/chain"
)

func (conf *bitcoindConfig) ParseRPCParams(cConfig *chainConfig, net chainCode,
	funcName string) error {

	var daemonName, confDir, confFile string

	// Get the daemon name for displaying proper errors.
	switch net {
	case bitcoinChain:
		daemonName = "btcd"
	case litecoinChain:
		daemonName = "ltcd"
	}

	// If all of RPCUser, RPCPass, and ZMQPath are set, we assume
	// those parameters are good to use.
	if conf.RPCUser != "" && conf.RPCPass != "" && conf.ZMQPath != "" {
		return nil
	}

	// If only one or two of the parameters are set, we assume the
	// user did that unintentionally.
	if conf.RPCUser != "" || conf.RPCPass != "" || conf.ZMQPath != "" {
		return fmt.Errorf("please set all or none of "+
			"%[1]v.rpcuser, %[1]v.rpcpass, "+
			"and %[1]v.zmqpath", daemonName)
	}

	switch net {
	case bitcoinChain:
		confDir = conf.Dir
		confFile = "bitcoin"
	case litecoinChain:
		confDir = conf.Dir
		confFile = "litecoin"
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

	rpcUser, rpcPass, zmqPath, err := extractBitcoindRPCParams(confFile)
	if err != nil {
		return fmt.Errorf("unable to extract RPC credentials:"+
			" %v, cannot start w/o RPC connection",
			err)
	}
	conf.RPCUser, conf.RPCPass, conf.ZMQPath = rpcUser, rpcPass, zmqPath

	fmt.Printf("Automatically obtained %v's RPC credentials\n", daemonName)

	return nil
}

// newChainControlFromConfig attempts to create a chainControl instance
// according to the parameters in the passed lnd configuration. Currently two
// branches of chainControl instances exist: one backed by a running btcd
// full-node, and the other backed by a running neutrino light client instance.
func (conf *bitcoindConfig) NewChainControlFromConfig(cfg *config,
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
		err          error
		cleanUp      func()
		bitcoindConn *chain.BitcoindClient
	)

	// If spv mode is active, then we'll be using a distinct set of
	// chainControl interfaces that interface directly with the p2p network
	// of the selected chain.

	// Otherwise, we'll be speaking directly via RPC and ZMQ to a
	// bitcoind node. If the specified host for the btcd/ltcd RPC
	// server already has a port specified, then we use that
	// directly. Otherwise, we assume the default port according to
	// the selected chain parameters.
	var bitcoindHost string
	if strings.Contains(conf.RPCHost, ":") {
		bitcoindHost = conf.RPCHost
	} else {
		// The RPC ports specified in chainparams.go assume
		// btcd, which picks a different port so that btcwallet
		// can use the same RPC port as bitcoind. We convert
		// this back to the btcwallet/bitcoind port.
		rpcPort, err := strconv.Atoi(activeNetParams.rpcPort)
		if err != nil {
			return nil, nil, err
		}
		rpcPort -= 2
		bitcoindHost = fmt.Sprintf("%v:%d",
			conf.RPCHost, rpcPort)
		if cfg.Bitcoin.Active && cfg.Bitcoin.RegTest {
			conn, err := net.Dial("tcp", bitcoindHost)
			if err != nil || conn == nil {
				rpcPort = 18443
				bitcoindHost = fmt.Sprintf("%v:%d",
					conf.RPCHost,
					rpcPort)
			} else {
				conn.Close()
			}
		}
	}

	bitcoindUser := conf.RPCUser
	bitcoindPass := conf.RPCPass
	rpcConfig := &rpcclient.ConnConfig{
		Host:                 bitcoindHost,
		User:                 bitcoindUser,
		Pass:                 bitcoindPass,
		DisableConnectOnNew:  true,
		DisableAutoReconnect: false,
		DisableTLS:           true,
		HTTPPostMode:         true,
	}
	cc.chainNotifier, err = bitcoindnotify.New(rpcConfig,
		conf.ZMQPath, *activeNetParams.Params)
	if err != nil {
		return nil, nil, err
	}

	// Next, we'll create an instance of the bitcoind chain view to
	// be used within the routing layer.
	cc.chainView, err = chainview.NewBitcoindFilteredChainView(
		*rpcConfig, conf.ZMQPath,
		*activeNetParams.Params)
	if err != nil {
		srvrLog.Errorf("unable to create chain view: %v", err)
		return nil, nil, err
	}

	// Create a special rpc+ZMQ client for bitcoind which will be
	// used by the wallet for notifications, calls, etc.
	bitcoindConn, err = chain.NewBitcoindClient(
		activeNetParams.Params, bitcoindHost, bitcoindUser,
		bitcoindPass, conf.ZMQPath,
		time.Millisecond*100)
	if err != nil {
		return nil, nil, err
	}

	walletConfig.ChainSource = bitcoindConn

	// If we're not in regtest mode, then we'll attempt to use a
	// proper fee estimator for testnet.
	if cfg.Bitcoin.Active && !cfg.Bitcoin.RegTest {
		ltndLog.Infof("Initializing bitcoind backed fee estimator")

		// Finally, we'll re-initialize the fee estimator, as
		// if we're using bitcoind as a backend, then we can
		// use live fee estimates, rather than a statically
		// coded value.
		fallBackFeeRate := lnwallet.SatPerVByte(25)
		cc.feeEstimator, err = lnwallet.NewBitcoindFeeEstimator(
			*rpcConfig, fallBackFeeRate,
		)
		if err != nil {
			return nil, nil, err
		}
		if err := cc.feeEstimator.Start(); err != nil {
			return nil, nil, err
		}
	} else if cfg.Litecoin.Active {
		ltndLog.Infof("Initializing litecoind backed fee estimator")

		// Finally, we'll re-initialize the fee estimator, as
		// if we're using litecoind as a backend, then we can
		// use live fee estimates, rather than a statically
		// coded value.
		fallBackFeeRate := lnwallet.SatPerVByte(25)
		cc.feeEstimator, err = lnwallet.NewBitcoindFeeEstimator(
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

// extractBitcoindParams attempts to extract the RPC credentials for an
// existing bitcoind node instance. The passed path is expected to be the
// location of bitcoind's bitcoin.conf on the target system. The routine looks
// for a cookie first, optionally following the datadir configuration option in
// the bitcoin.conf. If it doesn't find one, it looks for rpcuser/rpcpassword.
func extractBitcoindRPCParams(bitcoindConfigPath string) (string, string, string, error) {

	// First, we'll open up the bitcoind configuration file found at the
	// target destination.
	bitcoindConfigFile, err := os.Open(bitcoindConfigPath)
	if err != nil {
		return "", "", "", err
	}
	defer bitcoindConfigFile.Close()

	// With the file open extract the contents of the configuration file so
	// we can attempt to locate the RPC credentials.
	configContents, err := ioutil.ReadAll(bitcoindConfigFile)
	if err != nil {
		return "", "", "", err
	}

	// First, we look for the ZMQ path for raw blocks. If raw transactions
	// are sent over this interface, we can also get unconfirmed txs.
	zmqPathRE, err := regexp.Compile(`(?m)^\s*zmqpubrawblock\s*=\s*([^\s]+)`)
	if err != nil {
		return "", "", "", err
	}
	zmqPathSubmatches := zmqPathRE.FindSubmatch(configContents)
	if len(zmqPathSubmatches) < 2 {
		return "", "", "", fmt.Errorf("unable to find zmqpubrawblock in config")
	}

	// Next, we'll try to find an auth cookie. We need to detect the chain
	// by seeing if one is specified in the configuration file.
	dataDir := path.Dir(bitcoindConfigPath)
	dataDirRE, err := regexp.Compile(`(?m)^\s*datadir\s*=\s*([^\s]+)`)
	if err != nil {
		return "", "", "", err
	}
	dataDirSubmatches := dataDirRE.FindSubmatch(configContents)
	if dataDirSubmatches != nil {
		dataDir = string(dataDirSubmatches[1])
	}

	chainDir := "/"
	switch activeNetParams.Params.Name {
	case "testnet3":
		chainDir = "/testnet3/"
	case "testnet4":
		chainDir = "/testnet4/"
	case "regtest":
		chainDir = "/regtest/"
	}

	cookie, err := ioutil.ReadFile(dataDir + chainDir + ".cookie")
	if err == nil {
		splitCookie := strings.Split(string(cookie), ":")
		if len(splitCookie) == 2 {
			return splitCookie[0], splitCookie[1],
				string(zmqPathSubmatches[1]), nil
		}
	}

	// We didn't find a cookie, so we attempt to locate the RPC user using
	// a regular expression. If we  don't have a match for our regular
	// expression then we'll exit with an error.
	rpcUserRegexp, err := regexp.Compile(`(?m)^\s*rpcuser\s*=\s*([^\s]+)`)
	if err != nil {
		return "", "", "", err
	}
	userSubmatches := rpcUserRegexp.FindSubmatch(configContents)
	if userSubmatches == nil {
		return "", "", "", fmt.Errorf("unable to find rpcuser in config")
	}

	// Similarly, we'll use another regular expression to find the set
	// rpcpass (if any). If we can't find the pass, then we'll exit with an
	// error.
	rpcPassRegexp, err := regexp.Compile(`(?m)^\s*rpcpassword\s*=\s*([^\s]+)`)
	if err != nil {
		return "", "", "", err
	}
	passSubmatches := rpcPassRegexp.FindSubmatch(configContents)
	if passSubmatches == nil {
		return "", "", "", fmt.Errorf("unable to find rpcpassword in config")
	}

	return string(userSubmatches[1]), string(passSubmatches[1]),
		string(zmqPathSubmatches[1]), nil
}
